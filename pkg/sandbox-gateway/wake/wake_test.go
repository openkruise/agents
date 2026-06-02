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

package wake

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
)

func TestWaker_WakeAndWait_StatusMapping(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		expectErr error
	}{
		{name: "not found", status: http.StatusNotFound, expectErr: ErrSandboxNotFound},
		{name: "unauthorized", status: http.StatusUnauthorized, expectErr: ErrUnauthorized},
		{name: "forbidden", status: http.StatusForbidden, expectErr: ErrUnauthorized},
		{name: "no content", status: http.StatusNoContent, expectErr: ErrWakeFailed},
		{name: "bad request", status: http.StatusBadRequest, expectErr: ErrWakeFailed},
		{name: "server error", status: http.StatusInternalServerError, expectErr: ErrWakeFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := registry.GetRegistry()
			reg.Clear()
			reg.Update("sandbox", proxy.Route{ID: "sandbox", State: agentsv1alpha1.SandboxStatePaused, ResourceVersion: "1"})
			w := testWaker(&fakeConnector{statuses: []int{tt.status}}, reg)

			err := w.WakeAndWait(context.Background(), "sandbox", "timeout:300")

			assert.ErrorIs(t, err, tt.expectErr)
		})
	}
}

func TestWaker_WakeAndWait_RetriesConflictThenPollsRunning(t *testing.T) {
	reg := registry.GetRegistry()
	reg.Clear()
	reg.Update("sandbox", proxy.Route{ID: "sandbox", State: agentsv1alpha1.SandboxStatePaused, ResourceVersion: "1"})
	connector := &fakeConnector{statuses: []int{http.StatusConflict, http.StatusConflict, http.StatusOK}}
	w := testWaker(connector, reg)
	go func() {
		time.Sleep(10 * time.Millisecond)
		reg.Update("sandbox", proxy.Route{ID: "sandbox", State: agentsv1alpha1.SandboxStateRunning, ResourceVersion: "2"})
	}()

	err := w.WakeAndWait(context.Background(), "sandbox", "timeout:300")

	require.NoError(t, err)
	assert.Equal(t, 3, connector.calls())
}

func TestWaker_WakeAndWait_SingleflightFollowerCancelDoesNotPoisonLeader(t *testing.T) {
	reg := registry.GetRegistry()
	reg.Clear()
	reg.Update("sandbox", proxy.Route{ID: "sandbox", State: agentsv1alpha1.SandboxStatePaused, ResourceVersion: "1"})
	connector := &fakeConnector{statuses: []int{http.StatusOK}, block: make(chan struct{})}
	w := testWaker(connector, reg)
	leaderErr := make(chan error, 1)
	followerErr := make(chan error, 1)

	go func() {
		leaderErr <- w.WakeAndWait(context.Background(), "sandbox", "timeout:300")
	}()
	require.Eventually(t, func() bool { return connector.calls() == 1 }, time.Second, time.Millisecond)

	followerCtx, cancel := context.WithCancel(context.Background())
	go func() {
		followerErr <- w.WakeAndWait(followerCtx, "sandbox", "timeout:300")
	}()
	cancel()
	close(connector.block)
	reg.Update("sandbox", proxy.Route{ID: "sandbox", State: agentsv1alpha1.SandboxStateRunning, ResourceVersion: "2"})

	assert.ErrorIs(t, <-followerErr, context.Canceled)
	require.NoError(t, <-leaderErr)
	assert.Equal(t, 1, connector.calls())
}

func TestWaker_WakeAndWait_FilterContextBoundsRetryAndPoll(t *testing.T) {
	tests := []struct {
		name      string
		connector *fakeConnector
		seedState string
	}{
		{
			name:      "conflict retry loop",
			connector: &fakeConnector{statuses: []int{http.StatusConflict}},
			seedState: agentsv1alpha1.SandboxStatePaused,
		},
		{
			name:      "registry poll loop",
			connector: &fakeConnector{statuses: []int{http.StatusOK}},
			seedState: agentsv1alpha1.SandboxStatePaused,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := registry.GetRegistry()
			reg.Clear()
			reg.Update("sandbox", proxy.Route{ID: "sandbox", State: tt.seedState, ResourceVersion: "1"})
			w := testWaker(tt.connector, reg)
			w.DetachedTimeout = time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(30*time.Millisecond, cancel)

			err := w.WakeAndWait(ctx, "sandbox", "timeout:300")

			assert.ErrorIs(t, err, context.Canceled)
			assert.GreaterOrEqual(t, tt.connector.calls(), 1)
		})
	}
}

func TestWaker_WakeAndWait_DetachedConnectTimeout(t *testing.T) {
	reg := registry.GetRegistry()
	reg.Clear()
	reg.Update("sandbox", proxy.Route{ID: "sandbox", State: agentsv1alpha1.SandboxStatePaused, ResourceVersion: "1"})
	w := testWaker(&fakeConnector{statuses: []int{http.StatusOK}, block: make(chan struct{})}, reg)
	w.DetachedTimeout = time.Millisecond

	err := w.WakeAndWait(context.Background(), "sandbox", "timeout:300")

	assert.ErrorIs(t, err, ErrTransport)
}

func TestWaker_WakeAndWait_DisabledAndTransport(t *testing.T) {
	tests := []struct {
		name       string
		annotation string
		connector  *fakeConnector
		expectErr  error
	}{
		{name: "disabled", annotation: "garbage", connector: &fakeConnector{}, expectErr: ErrWakeDisabled},
		{name: "transport", annotation: "timeout:300", connector: &fakeConnector{err: errors.New("dial")}, expectErr: ErrTransport},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := registry.GetRegistry()
			reg.Clear()
			reg.Update("sandbox", proxy.Route{ID: "sandbox", State: agentsv1alpha1.SandboxStateRunning, ResourceVersion: "1"})
			w := testWaker(tt.connector, reg)

			err := w.WakeAndWait(context.Background(), "sandbox", tt.annotation)

			assert.ErrorIs(t, err, tt.expectErr)
		})
	}
}

func testWaker(connector *fakeConnector, reg RouteRegistry) *Waker {
	return &Waker{
		Connector:       connector,
		Registry:        reg,
		DetachedTimeout: time.Second,
		RetryBackoff:    time.Millisecond,
		PollInterval:    time.Millisecond,
	}
}

type fakeConnector struct {
	mu       sync.Mutex
	statuses []int
	err      error
	block    chan struct{}
	count    int
}

func (f *fakeConnector) Connect(ctx context.Context, sandboxID string, timeoutSeconds int) (int, error) {
	f.mu.Lock()
	f.count++
	idx := f.count - 1
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-f.block:
		}
	}
	if f.err != nil {
		return 0, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if idx >= len(f.statuses) {
		idx = len(f.statuses) - 1
	}
	return f.statuses[idx], nil
}

func (f *fakeConnector) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count
}
