/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sandbox_manager

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/openkruise/agents/pkg/sandbox-manager/config"
)

const (
	primaryLeaseName     = "sandbox-manager-primary"
	primaryLeaseDuration = 15 * time.Second
	primaryRenewDeadline = 10 * time.Second
	primaryRetryPeriod   = 2 * time.Second
)

type primaryState struct {
	primary atomic.Bool
	mu      sync.Mutex
	changed chan struct{} // lazily allocated; never nil after first PrimaryChanged()
}

func (s *primaryState) IsPrimary() bool {
	if s == nil {
		return true
	}
	return s.primary.Load()
}

func (s *primaryState) PrimaryChanged() <-chan struct{} {
	if s == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changed == nil {
		s.changed = make(chan struct{})
	}
	return s.changed
}

func (s *primaryState) set(v bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.primary.Load() == v {
		return
	}
	s.primary.Store(v)
	if s.changed != nil {
		close(s.changed)
	}
	s.changed = make(chan struct{})
}

func (s *primaryState) WaitPrimary(ctx context.Context) error {
	if s.IsPrimary() {
		return nil
	}
	for {
		ch := s.PrimaryChanged()
		if s.IsPrimary() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
			if s.IsPrimary() {
				return nil
			}
		}
	}
}

// leaderElectionRunner abstracts the K8s LeaderElector so that the election
// loop can be tested without a real API server.
type leaderElectionRunner interface {
	Run(ctx context.Context)
}

type primaryElector struct {
	elector leaderElectionRunner
	state   *primaryState

	runOnce  sync.Once
	stopOnce sync.Once
	stopCh   chan struct{}
	done     chan struct{}
}

func newPrimaryElector(opts config.SandboxManagerOptions, state *primaryState) (*primaryElector, error) {
	clientset, err := kubernetes.NewForConfig(primaryKubeClientConfig(opts.RestConfig))
	if err != nil {
		return nil, err
	}

	primary := &primaryElector{
		state:  state,
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
	}

	lock, err := resourcelock.New(
		resourcelock.LeasesResourceLock,
		opts.SystemNamespace,
		primaryLeaseName,
		clientset.CoreV1(),
		clientset.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity: resolvePrimaryIdentity(),
		},
	)
	if err != nil {
		return nil, err
	}

	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: primaryLeaseDuration,
		RenewDeadline: primaryRenewDeadline,
		RetryPeriod:   primaryRetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				primary.startLeading(ctx)
			},
			OnStoppedLeading: func() {
				primary.stopLeading()
			},
		},
		Name: primaryLeaseName,
	})
	if err != nil {
		return nil, err
	}

	primary.elector = elector
	return primary, nil
}

func primaryKubeClientConfig(cfg *rest.Config) *rest.Config {
	if cfg == nil {
		return nil
	}
	out := rest.CopyConfig(cfg)
	timeout := primaryRenewDeadline / 2
	//goland:noinspection GoBoolExpressions Just for defense
	if timeout < time.Second {
		timeout = time.Second
	}
	out.Timeout = timeout
	if out.UserAgent != "" {
		out.UserAgent = out.UserAgent + " " + primaryLeaseName
		return out
	}
	return rest.AddUserAgent(out, primaryLeaseName)
}

func (e *primaryElector) Run(ctx context.Context) {
	if e == nil || e.elector == nil {
		return
	}

	e.runOnce.Do(func() {
		if e.stopCh == nil {
			e.stopCh = make(chan struct{})
		}
		if e.done == nil {
			e.done = make(chan struct{})
		}

		runCtx, cancel := context.WithCancel(ctx)
		defer func() {
			cancel()
			e.state.set(false)
			close(e.done)
		}()

		go func() {
			select {
			case <-e.stopCh:
				cancel()
			case <-runCtx.Done():
			}
		}()

		for runCtx.Err() == nil {
			e.elector.Run(runCtx)
		}
	})
}

func (e *primaryElector) Stop(ctx context.Context) {
	if e == nil {
		return
	}

	if e.stopCh != nil {
		e.stopOnce.Do(func() { close(e.stopCh) })
	}
	e.state.set(false)
	if e.done == nil {
		return
	}

	select {
	case <-e.done:
	case <-ctx.Done():
	}
}

func (e *primaryElector) startLeading(ctx context.Context) {
	if e == nil {
		return
	}
	if ctx.Err() != nil {
		return
	}
	if e.stopCh != nil {
		select {
		case <-e.stopCh:
			return
		default:
		}
	}
	e.state.set(true)
}

func (e *primaryElector) stopLeading() {
	if e == nil {
		return
	}
	e.state.set(false)
}

func resolvePrimaryIdentity() string {
	if hostname := os.Getenv("HOSTNAME"); hostname != "" {
		return hostname
	}
	if podName := os.Getenv("POD_NAME"); podName != "" {
		return podName
	}
	return "sandbox-manager-" + uuid.NewString()[:8]
}
