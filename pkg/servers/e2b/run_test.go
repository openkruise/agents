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

package e2b

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/proxy"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
)

// newStartupController builds a controller whose infra Run is replaced by
// run, for exercising Controller.Run startup paths. The ext-proc listener is
// disabled so only the route-refresh port would be bound if startup ever got
// far enough to start the proxy.
func newStartupController(t *testing.T, run func(context.Context) error) *Controller {
	t.Helper()
	opts := config.InitOptions(config.SandboxManagerOptions{DisableEnvoyExtProc: true})
	fakeCache, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	proxyServer := proxy.NewServer(opts)
	manager, err := sandboxmanager.NewSandboxManagerBuilder(opts).
		WithCustomInfra(func() (infra.Builder, error) {
			return hookInfraBuilder{
				base: sandboxcr.NewInfraBuilder(opts).
					WithCache(fakeCache).
					WithAPIReader(fc).
					WithRouteReader(proxyServer),
				run: run,
			}, nil
		}).
		Build()
	require.NoError(t, err)
	return &Controller{
		server:  &http.Server{Addr: "127.0.0.1:0"},
		manager: manager,
	}
}

// TestControllerSignalDuringStartupExitsCleanly pins the startup crash
// semantics: a termination signal that lands while the manager is still
// starting makes Run return a canceled context with no error, so main exits
// zero without waiting for startup or running per-component cleanup.
func TestControllerSignalDuringStartupExitsCleanly(t *testing.T) {
	started := make(chan struct{})
	controller := newStartupController(t, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})

	type runResult struct {
		ctx context.Context
		err error
	}
	result := make(chan runResult, 1)
	stop := make(chan os.Signal, 1)
	go func() {
		ctx, err := controller.Run(stop)
		result <- runResult{ctx: ctx, err: err}
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for manager startup to block")
	}
	stop <- syscall.SIGTERM

	select {
	case got := <-result:
		require.NoError(t, got.err)
		require.ErrorIs(t, got.ctx.Err(), context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Run did not return after SIGTERM during startup")
	}
}

// TestControllerRunPropagatesStartupFailure pins that startup errors surface
// synchronously from Run instead of crashing from a background goroutine.
func TestControllerRunPropagatesStartupFailure(t *testing.T) {
	controller := newStartupController(t, func(context.Context) error {
		return errors.New("cache sync failed")
	})

	_, err := controller.Run(make(chan os.Signal, 1))
	require.Error(t, err)
	assert.ErrorContains(t, err, "sandbox manager failed to start")
}

func TestAwaitStartup(t *testing.T) {
	tests := []struct {
		name     string
		startup  func(startupDone chan error, stop chan os.Signal)
		expected []startupOutcome
		errMsg   string
	}{
		{
			name:     "startup succeeds",
			startup:  func(startupDone chan error, _ chan os.Signal) { startupDone <- nil },
			expected: []startupOutcome{startupCompleted},
		},
		{
			name:     "startup fails",
			startup:  func(startupDone chan error, _ chan os.Signal) { startupDone <- errors.New("boom") },
			expected: []startupOutcome{startupFailed},
			errMsg:   "boom",
		},
		{
			name:     "signal interrupts startup",
			startup:  func(_ chan error, stop chan os.Signal) { stop <- syscall.SIGTERM },
			expected: []startupOutcome{startupInterrupted},
		},
		{
			// The outer select picks either ready channel; both branches must
			// report the completed startup so the caller shuts down gracefully.
			name: "signal pending with completed startup is never an interruption",
			startup: func(startupDone chan error, stop chan os.Signal) {
				startupDone <- nil
				stop <- syscall.SIGTERM
			},
			expected: []startupOutcome{startupCompleted, startupSignaled},
		},
		{
			name: "signal pending with failed startup reports the failure",
			startup: func(startupDone chan error, stop chan os.Signal) {
				startupDone <- errors.New("boom")
				stop <- syscall.SIGTERM
			},
			expected: []startupOutcome{startupFailed},
			errMsg:   "boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Repeat so rows with two ready channels exercise both select
			// branches in practice.
			for range 32 {
				startupDone := make(chan error, 1)
				stop := make(chan os.Signal, 1)
				tt.startup(startupDone, stop)

				outcome, err := awaitStartup(startupDone, stop)
				assert.Contains(t, tt.expected, outcome)
				if tt.errMsg != "" {
					assert.ErrorContains(t, err, tt.errMsg)
				} else {
					assert.NoError(t, err)
				}
			}
		})
	}
}

func TestControllerStartHTTPServerReportsBindFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	controller := &Controller{server: &http.Server{Addr: listener.Addr().String()}}
	err = controller.startHTTPServer()
	require.Error(t, err)
	assert.ErrorContains(t, err, "listen for E2B API")
}
