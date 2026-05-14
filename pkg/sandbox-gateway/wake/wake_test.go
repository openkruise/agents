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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
)

func TestWakeAndWait(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		action      proxy.WakeAction
		setup       func(sandboxID string)
		afterServer func(sandboxID string)
		expectError error
	}{
		{
			name:   "manager 200 and registry already running",
			status: http.StatusOK,
			setup: func(sandboxID string) {
				setRoute(sandboxID, agentsv1alpha1.SandboxStateRunning, "2")
			},
		},
		{
			name:   "manager 200 and registry transitions to running",
			status: http.StatusOK,
			setup: func(sandboxID string) {
				setRoute(sandboxID, agentsv1alpha1.SandboxStatePaused, "1")
			},
			afterServer: func(sandboxID string) {
				go func() {
					time.Sleep(80 * time.Millisecond)
					setRoute(sandboxID, agentsv1alpha1.SandboxStateRunning, "2")
				}()
			},
		},
		{
			name:        "manager 422 invalid policy",
			status:      http.StatusUnprocessableEntity,
			action:      proxy.WakeActionInvalidAutoResumePolicy,
			expectError: ErrInvalidPolicy,
		},
		{
			name:        "manager 422 auto resume disabled",
			status:      http.StatusUnprocessableEntity,
			action:      proxy.WakeActionAutoResumeDisabled,
			expectError: ErrAutoResumeDisabled,
		},
		{
			name:        "manager 409 pausing",
			status:      http.StatusConflict,
			action:      proxy.WakeActionPausing,
			expectError: ErrPausing,
		},
		{
			name:        "manager 409 bad state",
			status:      http.StatusConflict,
			action:      proxy.WakeActionBadState,
			expectError: ErrBadState,
		},
		{
			name:        "manager 410 gone",
			status:      http.StatusGone,
			action:      proxy.WakeActionGone,
			expectError: ErrGone,
		},
		{
			name:        "manager 404 not found",
			status:      http.StatusNotFound,
			action:      proxy.WakeActionNotFound,
			expectError: ErrNotFound,
		},
		{
			name:        "manager 5xx",
			status:      http.StatusInternalServerError,
			expectError: ErrTransport,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := registry.GetRegistry()
			r.Clear()
			t.Cleanup(r.Clear)

			sandboxID := "default--wake-test"
			if tt.setup != nil {
				tt.setup(sandboxID)
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("method = %s, want %s", r.Method, http.MethodPost)
				}
				if !strings.HasSuffix(r.URL.Path, "/wake/"+sandboxID) {
					t.Errorf("path = %s, want suffix /wake/%s", r.URL.Path, sandboxID)
				}
				w.WriteHeader(tt.status)
				if tt.action != "" {
					_ = json.NewEncoder(w).Encode(proxy.WakeResult{Action: tt.action})
				}
			}))
			defer server.Close()

			if tt.afterServer != nil {
				tt.afterServer(sandboxID)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := NewModuleWithClient(server.URL, server.Client()).WakeAndWait(ctx, sandboxID)
			if tt.expectError == nil {
				if err != nil {
					t.Fatalf("WakeAndWait() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.expectError) {
				t.Fatalf("WakeAndWait() error = %v, want %v", err, tt.expectError)
			}
		})
	}
}

func TestWakeAndWaitSingleflight(t *testing.T) {
	tests := []struct {
		name       string
		sandboxIDs []string
		wantPosts  int32
	}{
		{
			name:       "same sandbox shares one manager request",
			sandboxIDs: []string{"default--shared", "default--shared"},
			wantPosts:  1,
		},
		{
			name:       "different sandboxes do not coalesce",
			sandboxIDs: []string{"default--one", "default--two"},
			wantPosts:  2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := registry.GetRegistry()
			r.Clear()
			t.Cleanup(r.Clear)

			for _, sandboxID := range tt.sandboxIDs {
				setRoute(sandboxID, agentsv1alpha1.SandboxStatePaused, "1")
			}

			var posts atomic.Int32
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				<-release
				sandboxID := strings.TrimPrefix(r.URL.Path, "/wake/")
				setRoute(sandboxID, agentsv1alpha1.SandboxStateRunning, "2")
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			module := NewModuleWithClient(server.URL, server.Client())
			var wg sync.WaitGroup
			errs := make([]error, len(tt.sandboxIDs))
			for i, sandboxID := range tt.sandboxIDs {
				wg.Add(1)
				go func(i int, sandboxID string) {
					defer wg.Done()
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					errs[i] = module.WakeAndWait(ctx, sandboxID)
				}(i, sandboxID)
			}

			deadline := time.After(2 * time.Second)
			for posts.Load() < tt.wantPosts {
				select {
				case <-deadline:
					t.Fatalf("manager posts = %d, want %d", posts.Load(), tt.wantPosts)
				default:
					time.Sleep(10 * time.Millisecond)
				}
			}
			time.Sleep(50 * time.Millisecond)
			if got := posts.Load(); got != tt.wantPosts {
				t.Fatalf("manager posts before release = %d, want %d", got, tt.wantPosts)
			}
			close(release)
			wg.Wait()

			for i, err := range errs {
				if err != nil {
					t.Fatalf("WakeAndWait(%s) error = %v", tt.sandboxIDs[i], err)
				}
			}
		})
	}
}

func TestWakeAndWaitLeaderCancelKeepsFollowerAlive(t *testing.T) {
	r := registry.GetRegistry()
	r.Clear()
	t.Cleanup(r.Clear)

	sandboxID := "default--leader-cancel"
	setRoute(sandboxID, agentsv1alpha1.SandboxStatePaused, "1")

	var posts atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		<-release
		setRoute(sandboxID, agentsv1alpha1.SandboxStateRunning, "2")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	module := NewModuleWithClient(server.URL, server.Client())
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		leaderDone <- module.WakeAndWait(leaderCtx, sandboxID)
	}()

	deadline := time.After(2 * time.Second)
	for posts.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("manager did not receive leader POST")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	followerDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		followerDone <- module.WakeAndWait(ctx, sandboxID)
	}()
	requireFlightWaiters(t, module, sandboxID, 2)

	cancelLeader()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader error = %v, want context.Canceled", err)
	}

	time.Sleep(50 * time.Millisecond)
	if got := posts.Load(); got != 1 {
		t.Fatalf("manager posts = %d, want 1", got)
	}
	close(release)

	if err := <-followerDone; err != nil {
		t.Fatalf("follower error = %v, want nil", err)
	}
}

func requireFlightWaiters(t *testing.T, module *Module, sandboxID string, want int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		module.flightsMu.Lock()
		got := 0
		if module.flights != nil && module.flights[sandboxID] != nil {
			got = module.flights[sandboxID].waiters
		}
		module.flightsMu.Unlock()
		if got == want {
			return
		}

		select {
		case <-deadline:
			t.Fatalf("flight waiters = %d, want %d", got, want)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestWakeAndWaitContextCancelledMidPoll(t *testing.T) {
	r := registry.GetRegistry()
	r.Clear()
	t.Cleanup(r.Clear)

	sandboxID := "default--cancelled"
	setRoute(sandboxID, agentsv1alpha1.SandboxStatePaused, "1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()

	err := NewModuleWithClient(server.URL, server.Client()).WakeAndWait(ctx, sandboxID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WakeAndWait() error = %v, want context.Canceled", err)
	}
}

func TestWakeAndWaitSlowManagerUsesCallerContextOnly(t *testing.T) {
	sandboxID := "default--slow-manager"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewModuleWithClient(server.URL, server.Client()).WakeAndWait(ctx, sandboxID)
	}()

	select {
	case err := <-done:
		t.Fatalf("WakeAndWait returned before caller cancellation: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("WakeAndWait() error = %v, want context.Canceled", err)
	}
}

func TestMapErrToReply(t *testing.T) {
	tests := []struct {
		name           string
		err            error
		wantStatus     int
		wantRetryAfter int
		wantCode       string
	}{
		{"auto resume disabled", ErrAutoResumeDisabled, http.StatusBadGateway, -1, "sandbox_not_running"},
		{"invalid policy", ErrInvalidPolicy, http.StatusServiceUnavailable, 0, "sandbox_wake_invalid_policy"},
		{"pausing", ErrPausing, http.StatusServiceUnavailable, 5, "sandbox_wake_pausing"},
		{"bad state", ErrBadState, http.StatusServiceUnavailable, 15, "sandbox_wake_bad_state"},
		{"gone", ErrGone, http.StatusBadGateway, -1, "sandbox_not_found"},
		{"not found", ErrNotFound, http.StatusBadGateway, -1, "sandbox_not_found"},
		{"transport", ErrTransport, http.StatusServiceUnavailable, 5, "sandbox_wake_failed"},
		{"cancelled", context.Canceled, 0, -1, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, _, retryAfter, code := MapErrToReply(tt.err)
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d", status, tt.wantStatus)
			}
			if retryAfter != tt.wantRetryAfter {
				t.Fatalf("retryAfter = %d, want %d", retryAfter, tt.wantRetryAfter)
			}
			if code != tt.wantCode {
				t.Fatalf("code = %q, want %q", code, tt.wantCode)
			}
		})
	}
}

func setRoute(sandboxID string, state string, resourceVersion string) {
	registry.GetRegistry().Update(sandboxID, proxy.Route{
		IP:              "10.0.0.1",
		State:           state,
		ResourceVersion: resourceVersion,
	})
}
