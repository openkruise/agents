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

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// fastBackoff keeps retry-based tests quick while still allowing several attempts.
var fastBackoff = wait.Backoff{
	Duration: time.Millisecond,
	Factor:   1.0,
	Steps:    5,
}

// newMountTestServer starts an httptest server that serves POST /v1/storage/mounts
// with the provided handler and returns the server plus a sandbox annotated with
// its URL.
func newMountTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *agentsv1alpha1.Sandbox) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/storage/mounts", handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, sandboxWithURL(server.URL)
}

func sandboxWithURL(url string) *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationRuntimeURL: url,
			},
		},
	}
}

func writeMountResponse(t *testing.T, w http.ResponseWriter, status int, resp CreateMountResponse) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	require.NoError(t, json.NewEncoder(w).Encode(resp))
}

// exactCalls makes "the handler must run exactly n times" expressible in the
// table below, including the n == 0 case that a bare int field cannot tell apart
// from an unset expectation.
func exactCalls(n int32) *int32 { return &n }

// TestStorageMountCallBehavior covers the transport behavior of runtimeClient.call
// through its Storage().Mount caller: retry classification, error surfacing and
// sandbox refreshing. Each case declares the runtime-side response, the client
// wiring and the expected outcome; the shared harness below runs them all.
func TestStorageMountCallBehavior(t *testing.T) {
	tests := []struct {
		name string
		// handler serves POST /v1/storage/mounts. call is the 1-based invocation
		// index so a case can vary its response per attempt. A nil handler means
		// no server is started, leaving the runtime without a resolvable URL.
		handler func(t *testing.T, w http.ResponseWriter, call int32)
		// bind optionally replaces the sandbox the runtime is bound to and
		// contributes extra options (e.g. a refresh hook). serverSbx carries the
		// test server URL. A nil bind binds serverSbx with no extra options.
		bind func(t *testing.T, serverSbx *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, []Option)
		// driver overrides the driver of the sent mount request; defaults to "oss".
		driver string
		// omitPublishRequest sends a request carrying no CSI publish request, so
		// the call must be rejected locally.
		omitPublishRequest bool
		// cancelCtx cancels the context before the call so no retry loop proceeds.
		cancelCtx bool

		// wantErr expects a failed call; wantMountPath is asserted otherwise.
		wantErr       bool
		wantMountPath string
		// wantErrContains / wantErrExcludes are substrings the error must and
		// must not carry.
		wantErrContains []string
		wantErrExcludes []string
		// wantAPIStatus, when non-zero, requires the error to unwrap to an
		// *APIError with this status; wantClientError asserts how that status is
		// classified for retry purposes.
		wantAPIStatus   int
		wantClientError bool
		// wantCalls asserts the exact handler invocation count; wantMinCalls and
		// wantMaxCalls bound it when non-zero.
		wantCalls    *int32
		wantMinCalls int32
		wantMaxCalls int32
	}{
		{
			name: "retries on 5xx then succeeds",
			handler: func(t *testing.T, w http.ResponseWriter, call int32) {
				if call < 3 {
					writeMountResponse(t, w, http.StatusInternalServerError, CreateMountResponse{
						Success: false,
						Message: "transient failure",
					})
					return
				}
				writeMountResponse(t, w, http.StatusOK, CreateMountResponse{
					Success:   true,
					MountPath: "/run/csi/mount-root/oss/abc",
					Message:   "mount completed successfully",
				})
			},
			wantMountPath: "/run/csi/mount-root/oss/abc",
			wantCalls:     exactCalls(3),
		},
		{
			name:   "does not retry on 4xx",
			driver: "bogus",
			handler: func(t *testing.T, w http.ResponseWriter, call int32) {
				writeMountResponse(t, w, http.StatusBadRequest, CreateMountResponse{
					Success: false,
					Message: "unsupported driver: bogus, no registered provider",
				})
			},
			wantErr:         true,
			wantErrContains: []string{"unsupported driver"},
			wantAPIStatus:   http.StatusBadRequest,
			wantClientError: true,
			wantCalls:       exactCalls(1),
		},
		{
			name:      "stops retrying when the context is cancelled",
			cancelCtx: true,
			handler: func(t *testing.T, w http.ResponseWriter, call int32) {
				writeMountResponse(t, w, http.StatusInternalServerError, CreateMountResponse{Success: false, Message: "boom"})
			},
			wantErr: true,
			// A cancelled context must not drive further retries.
			wantMaxCalls: 1,
		},
		{
			name: "refresh resolves the runtime URL then succeeds",
			handler: func(t *testing.T, w http.ResponseWriter, call int32) {
				writeMountResponse(t, w, http.StatusOK, CreateMountResponse{Success: true, MountPath: "/m"})
			},
			bind: func(t *testing.T, serverSbx *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, []Option) {
				// The bound sandbox starts without a runtime URL; the refresh hook
				// only surfaces the ready sandbox from the 2nd invocation onward.
				notReady := sandboxWithURL("")
				var refreshes int32
				t.Cleanup(func() {
					assert.GreaterOrEqual(t, atomic.LoadInt32(&refreshes), int32(2),
						"should refresh again after the URL was not ready")
				})
				refresh := func(ctx context.Context) (*agentsv1alpha1.Sandbox, error) {
					if atomic.AddInt32(&refreshes, 1) == 1 {
						return notReady, nil
					}
					return serverSbx, nil
				}
				return notReady, []Option{WithRefresh(refresh)}
			},
			wantMountPath: "/m",
			// The server is hit once, after the URL resolved.
			wantCalls: exactCalls(1),
		},
		{
			name: "success false on 200 is an error",
			handler: func(t *testing.T, w http.ResponseWriter, call int32) {
				writeMountResponse(t, w, http.StatusOK, CreateMountResponse{Success: false, Message: "provider rejected"})
			},
			wantErr:         true,
			wantErrContains: []string{"provider rejected"},
		},
		{
			// The auth/permission middleware returns {"error": ...} rather than
			// {"message": ...}; extractErrorMessage must handle both shapes.
			name: "error message read from the error field",
			handler: func(t *testing.T, w http.ResponseWriter, call int32) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"Unauthorized access, please provide a valid access token"}`))
			},
			wantErr:         true,
			wantErrContains: []string{"Unauthorized access"},
			wantAPIStatus:   http.StatusUnauthorized,
			wantClientError: true,
		},
		{
			// A 2xx response whose body is cut short mid-transfer must surface the
			// transport read error itself (transient, retryable) rather than a
			// misleading decode error or a silently empty response struct.
			name: "truncated body on 200 is a read error and is retried",
			handler: func(t *testing.T, w http.ResponseWriter, call int32) {
				// Declare more bytes than are actually written so the client hits
				// an unexpected EOF while reading the response body.
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", "1024")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"success":true,"mount`))
			},
			wantErr:         true,
			wantErrContains: []string{"failed to read runtime API"},
			wantErrExcludes: []string{"failed to decode"},
			wantMinCalls:    2,
		},
		{
			// No handler: the sandbox carries no runtime URL, so the call cannot
			// be addressed at all.
			name:            "no runtime url on the sandbox",
			wantErr:         true,
			wantErrContains: []string{"runtime url not found"},
		},
		{
			name:               "missing publish request is rejected locally",
			omitPublishRequest: true,
			handler: func(t *testing.T, w http.ResponseWriter, call int32) {
				writeMountResponse(t, w, http.StatusOK, CreateMountResponse{Success: true, MountPath: "/m"})
			},
			wantErr:         true,
			wantErrContains: []string{"csi publish request is required"},
			// An incomplete request must not reach the runtime.
			wantCalls: exactCalls(0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int32
			sbx := sandboxWithURL("")
			if tt.handler != nil {
				_, sbx = newMountTestServer(t, func(w http.ResponseWriter, r *http.Request) {
					tt.handler(t, w, atomic.AddInt32(&calls, 1))
				})
			}

			opts := []Option{WithRetry(fastBackoff)}
			if tt.bind != nil {
				var extra []Option
				sbx, extra = tt.bind(t, sbx)
				opts = append(opts, extra...)
			}

			ctx := context.Background()
			if tt.cancelCtx {
				cancellable, cancel := context.WithCancel(ctx)
				cancel() // cancel before the call so no retry loop proceeds
				ctx = cancellable
			}

			driver := tt.driver
			if driver == "" {
				driver = "oss"
			}
			req := testMountRequest(driver)
			if tt.omitPublishRequest {
				req = CreateMountRequest{Driver: driver}
			}

			resp, err := NewRuntime(sbx, opts...).Storage().Mount(ctx, req)
			if tt.wantErr {
				require.Error(t, err)
				for _, want := range tt.wantErrContains {
					assert.Contains(t, err.Error(), want)
				}
				for _, unwanted := range tt.wantErrExcludes {
					assert.NotContains(t, err.Error(), unwanted)
				}
				if tt.wantAPIStatus != 0 {
					var apiErr *APIError
					require.True(t, errors.As(err, &apiErr), "error should be an *APIError")
					assert.Equal(t, tt.wantAPIStatus, apiErr.StatusCode)
					assert.Equal(t, tt.wantClientError, apiErr.IsClientError())
				}
			} else {
				require.NoError(t, err)
				assert.True(t, resp.Success)
				assert.Equal(t, tt.wantMountPath, resp.MountPath)
			}

			got := atomic.LoadInt32(&calls)
			if tt.wantCalls != nil {
				assert.Equal(t, *tt.wantCalls, got, "unexpected number of runtime invocations")
			}
			if tt.wantMinCalls > 0 {
				assert.GreaterOrEqual(t, got, tt.wantMinCalls, "the failure must be treated as transient and retried")
			}
			if tt.wantMaxCalls > 0 {
				assert.LessOrEqual(t, got, tt.wantMaxCalls, "the failure must not drive further retries")
			}
		})
	}
}

// TestCallAuthorizationHeader covers the opt-in Basic user credential of the
// shared call transport: absent by default, sent only when the caller supplies
// WithAuthUser at construction time.
func TestCallAuthorizationHeader(t *testing.T) {
	tests := []struct {
		name       string
		opts       []Option
		wantAuth   string // expected Authorization header; empty = header not sent
		wantXToken string // expected X-Access-Token header
	}{
		{
			name:       "default sends no Authorization header",
			wantAuth:   "",
			wantXToken: "", // sandboxWithURL carries no access-token annotation
		},
		{
			name:     "WithAuthUser root sends Basic root credential",
			opts:     []Option{WithAuthUser("root")},
			wantAuth: "Basic cm9vdDo=",
		},
		{
			name:     "WithAuthUser custom user is encoded with empty password",
			opts:     []Option{WithAuthUser("agent")},
			wantAuth: "Basic YWdlbnQ6", // base64("agent:")
		},
		{
			name:     "empty WithAuthUser is ignored",
			opts:     []Option{WithAuthUser("")},
			wantAuth: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotAuth, gotXToken string
			_, sbx := newMountTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				gotXToken = r.Header.Get("X-Access-Token")
				writeMountResponse(t, w, http.StatusOK, CreateMountResponse{Success: true, MountPath: "/m"})
			})

			opts := append([]Option{WithRetry(wait.Backoff{Steps: 1})}, tt.opts...)
			_, err := NewRuntime(sbx, opts...).Storage().Mount(context.Background(), testMountRequest("oss"))
			require.NoError(t, err)
			assert.Equal(t, tt.wantAuth, gotAuth,
				"Authorization header must follow the caller-supplied WithAuthUser (empty when unset)")
			assert.Equal(t, tt.wantXToken, gotXToken)
		})
	}
}
