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

type leaderElectionRunner interface {
	Run(ctx context.Context)
}

type primaryElector struct {
	elector leaderElectionRunner
	state   *primaryState

	mu      sync.Mutex
	runID   uint64
	running bool
	stopped bool
	cancel  context.CancelFunc
	done    chan struct{}

	nextRunID atomic.Uint64
}

type primaryRunIDContextKey struct{}

func newPrimaryElector(opts config.SandboxManagerOptions, state *primaryState) (*primaryElector, error) {
	clientset, err := kubernetes.NewForConfig(primaryKubeClientConfig(opts.RestConfig))
	if err != nil {
		return nil, err
	}

	primary := &primaryElector{state: state}

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
	done := make(chan struct{})

	e.mu.Lock()
	if e.stopped || e.running {
		e.mu.Unlock()
		close(done)
		return
	}
	e.running = true
	e.done = done
	e.mu.Unlock()

	defer func() {
		e.mu.Lock()
		e.running = false
		e.runID = 0
		e.cancel = nil
		if e.done == done {
			e.done = nil
		}
		e.state.set(false)
		e.mu.Unlock()
		close(done)
	}()

	for ctx.Err() == nil {
		runCtx, cancel := context.WithCancel(ctx)
		runID := e.nextRunID.Add(1)
		runCtx = context.WithValue(runCtx, primaryRunIDContextKey{}, runID)

		e.mu.Lock()
		if e.stopped {
			e.mu.Unlock()
			cancel()
			return
		}
		e.runID = runID
		e.cancel = cancel
		e.mu.Unlock()

		e.elector.Run(runCtx)
		cancel()

		e.mu.Lock()
		if e.runID == runID {
			e.runID = 0
			e.state.set(false)
		}
		e.cancel = nil
		stopped := e.stopped
		e.mu.Unlock()
		if stopped {
			return
		}
	}
}

func (e *primaryElector) Stop(ctx context.Context) {
	if e == nil {
		return
	}

	e.mu.Lock()
	cancel := e.cancel
	done := e.done
	e.stopped = true
	e.runID = 0
	e.state.set(false)
	e.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done == nil {
		return
	}

	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (e *primaryElector) startLeading(ctx context.Context) {
	if e == nil || ctx.Err() != nil {
		return
	}
	runID, ok := ctx.Value(primaryRunIDContextKey{}).(uint64)
	if !ok {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped || e.runID != runID || ctx.Err() != nil {
		return
	}
	e.state.set(true)
}

func (e *primaryElector) stopLeading() {
	if e == nil {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.runID = 0
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
