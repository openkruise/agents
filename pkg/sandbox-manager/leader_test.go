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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"

	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
)

type recordingLeaderElector struct {
	calls atomic.Int64
}

func (r *recordingLeaderElector) Run(context.Context) {
	r.calls.Add(1)
}

type cancelAfterRunsLeaderElector struct {
	calls  atomic.Int64
	cancel context.CancelFunc
	after  int64
}

func (r *cancelAfterRunsLeaderElector) Run(context.Context) {
	if r.calls.Add(1) >= r.after {
		r.cancel()
	}
}

func TestPrimaryState(t *testing.T) {
	tests := []struct {
		name   string
		state  *primaryState
		steps  []bool
		expect bool
	}{
		{
			name:   "zero value is not primary",
			state:  &primaryState{},
			expect: false,
		},
		{
			name:   "set true",
			state:  &primaryState{},
			steps:  []bool{true},
			expect: true,
		},
		{
			name:   "set true then false",
			state:  &primaryState{},
			steps:  []bool{true, false},
			expect: false,
		},
		{
			name:   "nil state defaults to primary",
			state:  nil,
			expect: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, step := range tt.steps {
				tt.state.set(step)
			}
			assert.Equal(t, tt.expect, tt.state.IsPrimary())
		})
	}
}

func TestPrimaryElectorCallbacksRespectRunLifecycle(t *testing.T) {
	tests := []struct {
		name       string
		cancel     bool
		stopped    bool
		expectLive bool
	}{
		{
			name:       "active run becomes primary",
			expectLive: true,
		},
		{
			name:   "canceled context ignores start callback",
			cancel: true,
		},
		{
			name:    "stopped elector ignores start callback",
			stopped: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &primaryState{}
			elector := &primaryElector{state: state, stopped: tt.stopped}
			ctx, cancel := context.WithCancel(context.Background())
			if tt.cancel {
				cancel()
			} else {
				defer cancel()
			}

			elector.startLeading(ctx)
			assert.Equal(t, tt.expectLive, state.IsPrimary())
		})
	}
}

func TestPrimaryElectorStopLeadingClearsPrimary(t *testing.T) {
	state := &primaryState{}
	state.set(true)
	elector := &primaryElector{state: state}

	elector.stopLeading()
	assert.False(t, state.IsPrimary())
}

func TestPrimaryElectorStopCancelsAndClearsPrimary(t *testing.T) {
	state := &primaryState{}
	state.set(true)
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	elector := &primaryElector{
		state:  state,
		cancel: cancel,
		done:   done,
	}

	go func() {
		<-runCtx.Done()
		close(done)
	}()

	elector.Stop(context.Background())
	assert.False(t, state.IsPrimary())
	assert.ErrorIs(t, runCtx.Err(), context.Canceled)
}

func TestPrimaryElectorRunDoesNotStartAfterStop(t *testing.T) {
	state := &primaryState{}
	runner := &recordingLeaderElector{}
	elector := &primaryElector{state: state, elector: runner}

	elector.Stop(context.Background())
	elector.Run(context.Background())

	assert.Equal(t, int64(0), runner.calls.Load())
	assert.False(t, state.IsPrimary())
}

func TestPrimaryElectorRunRecontendsAfterLeaderElectorReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &cancelAfterRunsLeaderElector{cancel: cancel, after: 2}
	elector := &primaryElector{state: &primaryState{}, elector: runner}

	elector.Run(ctx)

	assert.GreaterOrEqual(t, runner.calls.Load(), int64(2))
}

func TestPrimaryKubeClientConfigUsesBoundedTimeoutAndUserAgent(t *testing.T) {
	base := &rest.Config{
		Host:      "https://kubernetes.example",
		Timeout:   time.Minute,
		UserAgent: "existing-agent",
	}

	got := primaryKubeClientConfig(base)

	require.NotSame(t, base, got)
	assert.Equal(t, base.Host, got.Host)
	assert.Equal(t, time.Minute, base.Timeout)
	assert.Equal(t, primaryRenewDeadline/2, got.Timeout)
	assert.Contains(t, got.UserAgent, "existing-agent")
	assert.Contains(t, got.UserAgent, primaryLeaseName)
}

func TestSandboxManagerIsPrimary(t *testing.T) {
	tests := []struct {
		name    string
		manager *SandboxManager
		expect  bool
	}{
		{
			name:    "nil manager defaults to primary",
			manager: nil,
			expect:  true,
		},
		{
			name:    "nil primary state defaults to primary",
			manager: &SandboxManager{},
			expect:  true,
		},
		{
			name:    "unset state is not primary",
			manager: &SandboxManager{primary: &primaryState{}},
			expect:  false,
		},
		{
			name:    "true state is primary",
			manager: func() *SandboxManager { s := &primaryState{}; s.set(true); return &SandboxManager{primary: s} }(),
			expect:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expect, tt.manager.IsPrimary())
		})
	}
}

func TestResolvePrimaryIdentity(t *testing.T) {
	tests := []struct {
		name         string
		hostname     string
		podName      string
		expectPrefix string
	}{
		{
			name:         "prefers hostname",
			hostname:     "manager-0",
			podName:      "pod-0",
			expectPrefix: "manager-0",
		},
		{
			name:         "falls back to pod name",
			hostname:     "",
			podName:      "pod-1",
			expectPrefix: "pod-1",
		},
		{
			name:         "falls back to generated suffix",
			hostname:     "",
			podName:      "",
			expectPrefix: "sandbox-manager-",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOSTNAME", tt.hostname)
			t.Setenv("POD_NAME", tt.podName)

			identity := resolvePrimaryIdentity()
			assert.True(t, strings.HasPrefix(identity, tt.expectPrefix), "identity %q should start with %q", identity, tt.expectPrefix)
		})
	}
}

func TestNewSandboxManagerBuilderPrimaryDefaults(t *testing.T) {
	opts := config.SandboxManagerOptions{
		SystemNamespace: "test-namespace",
	}

	cache, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)

	builder := NewSandboxManagerBuilder(opts).
		WithCustomInfra(func() (infra.Builder, error) {
			proxyServer := proxy.NewServer(opts)
			return sandboxcr.NewInfraBuilder(opts).
				WithCache(cache).
				WithAPIReader(fc).
				WithProxy(proxyServer), nil
		})

	require.NotNil(t, builder.instance.primary)

	manager, err := builder.Build()
	require.NoError(t, err)
	require.NotNil(t, manager.primary)
	assert.Nil(t, manager.elector)
	assert.True(t, manager.IsPrimary())
}

func TestPrimaryStateWaitPrimary(t *testing.T) {
	state := &primaryState{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- state.WaitPrimary(ctx) }()

	select {
	case <-done:
		t.Fatal("WaitPrimary returned before primary")
	case <-time.After(20 * time.Millisecond):
	}

	state.set(true)
	require.NoError(t, <-done)
}

func TestPrimaryStateWaitPrimaryAlreadyPrimary(t *testing.T) {
	state := &primaryState{}
	state.set(true)
	require.NoError(t, state.WaitPrimary(context.Background()))
}

func TestPrimaryStateWaitPrimaryChecksAfterSubscribe(t *testing.T) {
	state := &primaryState{}
	state.mu.Lock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- state.WaitPrimary(ctx) }()

	time.Sleep(20 * time.Millisecond)
	state.primary.Store(true)
	state.changed = make(chan struct{})
	state.mu.Unlock()

	require.NoError(t, <-done)
}

func TestPrimaryStateWaitPrimaryContextCancel(t *testing.T) {
	state := &primaryState{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- state.WaitPrimary(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("WaitPrimary did not return on context cancel")
	}
}

func TestPrimaryStateChangedNotifiesOnDemotion(t *testing.T) {
	state := &primaryState{}
	state.set(true)

	ch := state.PrimaryChanged()
	state.set(false)

	select {
	case <-ch:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("PrimaryChanged did not notify on demotion")
	}
}

func TestPrimaryStateChangedNilSafe(t *testing.T) {
	var state *primaryState
	ch := state.PrimaryChanged()
	require.NotNil(t, ch)
	select {
	case <-ch:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("nil primaryState PrimaryChanged should return closed channel")
	}
}

func TestPrimaryStateWaitPrimaryNilSafe(t *testing.T) {
	var state *primaryState
	require.NoError(t, state.WaitPrimary(context.Background()))
}
