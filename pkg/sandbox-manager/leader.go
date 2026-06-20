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
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"k8s.io/client-go/kubernetes"
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
}

func (s *primaryState) IsPrimary() bool {
	if s == nil {
		return true
	}
	return s.primary.Load()
}

func (s *primaryState) set(v bool) {
	if s == nil {
		return
	}
	s.primary.Store(v)
}

type primaryElector struct {
	elector *leaderelection.LeaderElector
}

func newPrimaryElector(opts config.SandboxManagerOptions, state *primaryState) (*primaryElector, error) {
	clientset, err := kubernetes.NewForConfig(opts.RestConfig)
	if err != nil {
		return nil, err
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
			OnStartedLeading: func(context.Context) {
				state.set(true)
			},
			OnStoppedLeading: func() {
				state.set(false)
			},
		},
		Name: primaryLeaseName,
	})
	if err != nil {
		return nil, err
	}

	return &primaryElector{elector: elector}, nil
}

func (e *primaryElector) Run(ctx context.Context) {
	if e == nil || e.elector == nil {
		return
	}
	e.elector.Run(ctx)
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
