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

package secureprovider

import (
	"context"
	"crypto/md5" //nolint:gosec // mirrors the production identifier hashing
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/utils/runtime"
)

// newSandboxWithRuntimeURL builds a minimal Sandbox CR carrying the runtime URL
// annotation expected by WriteFileWithRuntime, plus a runtime access token annotation
// so the test can assert that runtime.WriteFileWithRuntime forwards it as the
// X-Access-Token header.
func newSandboxWithRuntimeURL(runtimeURL string) *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns-a",
			Name:      "sbx-a",
			UID:       types.UID("uid-a"),
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationRuntimeURL:         runtimeURL,
				agentsv1alpha1.AnnotationRuntimeAccessToken: "runtime-access-token",
			},
		},
	}
}

// expectedTokenFileName replicates the production file-naming logic so the test asserts
// both the written path and the name handed to the stale-file prune step.
func expectedTokenFileName(sbx *agentsv1alpha1.Sandbox) string {
	identStr := fmt.Sprintf("%s-%s", sbx.Namespace, sbx.Name)
	hash := fmt.Sprintf("%x", md5.Sum([]byte(identStr))) //nolint:gosec
	return fmt.Sprintf("%s.token", hash)
}

// expectedTokenFilePath replicates the production path-naming logic so the test asserts
// the exact file path the propagator must produce for a given sandbox identity.
func expectedTokenFilePath(sbx *agentsv1alpha1.Sandbox) string {
	return DefaultSecurityTokenFilePath + expectedTokenFileName(sbx)
}

func TestWriteSecurityTokenToRuntime(t *testing.T) {
	type serverBehavior struct {
		statusCode int
		body       string
	}
	tests := []struct {
		name string
		// sbxFactory builds the input sandbox lazily so the test can plug in the
		// httptest.Server URL after it has been started.
		sbxFactory func(serverURL string) *agentsv1alpha1.Sandbox
		tokenResp  *identity.TokenResponse
		server     *serverBehavior // nil means HTTP server should NOT be hit
		pruneErr   error           // error returned by the stubbed stale-file prune step
		// rtOpts is the transport the caller resolved. Transport-neutral options are
		// used on purpose: they must reach the prune step unchanged without diverting
		// the write away from the plain httptest server.
		rtOpts      []runtime.Option
		expectError string
		// mustNotLeak is a substring that must never reach the returned error. It pins the
		// credential redaction on the write path, whose request body is the sandbox token.
		mustNotLeak string
	}{
		{
			name: "happy path writes file via runtime files API",
			sbxFactory: func(serverURL string) *agentsv1alpha1.Sandbox {
				return newSandboxWithRuntimeURL(serverURL)
			},
			tokenResp:   &identity.TokenResponse{AccessToken: "tok-1", RequestID: "req-1"},
			server:      &serverBehavior{statusCode: http.StatusOK, body: ""},
			expectError: "",
		},
		{
			name: "nil sandbox is rejected",
			sbxFactory: func(string) *agentsv1alpha1.Sandbox {
				return nil
			},
			tokenResp:   &identity.TokenResponse{AccessToken: "tok-1"},
			server:      nil,
			expectError: "sandbox is nil",
		},
		{
			name: "nil token response is rejected",
			sbxFactory: func(serverURL string) *agentsv1alpha1.Sandbox {
				return newSandboxWithRuntimeURL(serverURL)
			},
			tokenResp:   nil,
			server:      nil,
			expectError: "tokenResp is nil or AccessToken is empty",
		},
		{
			name: "empty access token is rejected",
			sbxFactory: func(serverURL string) *agentsv1alpha1.Sandbox {
				return newSandboxWithRuntimeURL(serverURL)
			},
			tokenResp:   &identity.TokenResponse{AccessToken: ""},
			server:      nil,
			expectError: "tokenResp is nil or AccessToken is empty",
		},
		{
			name: "missing runtime URL annotation is a soft skip",
			sbxFactory: func(string) *agentsv1alpha1.Sandbox {
				sbx := newSandboxWithRuntimeURL("")
				delete(sbx.Annotations, agentsv1alpha1.AnnotationRuntimeURL)
				return sbx
			},
			tokenResp:   &identity.TokenResponse{AccessToken: "tok-1"},
			server:      nil,
			expectError: "",
		},
		{
			// The runtime files API copies up to 1 KiB of a failing response body into its
			// error, and the upload this call site makes is the sandbox access token. A
			// runtime that echoes the rejected content back must not put it into the error
			// PropagateSecurityToken logs; the HTTP status is the safe context that survives.
			name: "runtime HTTP error keeps the status and drops the response body",
			sbxFactory: func(serverURL string) *agentsv1alpha1.Sandbox {
				return newSandboxWithRuntimeURL(serverURL)
			},
			tokenResp:   &identity.TokenResponse{AccessToken: "tok-1"},
			server:      &serverBehavior{statusCode: http.StatusInternalServerError, body: `rejected upload, content was {"accessToken":"tok-1"}`},
			expectError: "failed to write security token to runtime: the server returned status 500",
			mustNotLeak: "tok-1",
		},
		{
			name: "stale file prune failure surfaces as error",
			sbxFactory: func(serverURL string) *agentsv1alpha1.Sandbox {
				return newSandboxWithRuntimeURL(serverURL)
			},
			tokenResp:   &identity.TokenResponse{AccessToken: "tok-1"},
			server:      &serverBehavior{statusCode: http.StatusOK, body: ""},
			pruneErr:    fmt.Errorf("exit code 1"),
			expectError: "failed to prune stale security token files",
		},
		{
			// An older agent-runtime without the Filesystem service cannot prune, so its
			// absence must not fail propagation — the token itself is already written.
			name: "unsupported filesystem service is tolerated",
			sbxFactory: func(serverURL string) *agentsv1alpha1.Sandbox {
				return newSandboxWithRuntimeURL(serverURL)
			},
			tokenResp:   &identity.TokenResponse{AccessToken: "tok-1"},
			server:      &serverBehavior{statusCode: http.StatusOK, body: ""},
			pruneErr:    fmt.Errorf("%w: ListDir: unknown service", runtime.ErrRuntimeFilesystemUnsupported),
			expectError: "",
		},
		{
			// The credential write and the prune that follows it must ride the very
			// transport the caller resolved: a prune left on the plaintext path would
			// silently talk to a different endpoint than the write it cleans up after.
			name: "resolved transport options reach the prune step",
			sbxFactory: func(serverURL string) *agentsv1alpha1.Sandbox {
				return newSandboxWithRuntimeURL(serverURL)
			},
			tokenResp:   &identity.TokenResponse{AccessToken: "tok-1"},
			server:      &serverBehavior{statusCode: http.StatusOK, body: ""},
			rtOpts:      []runtime.Option{runtime.WithRequestTimeout(5 * time.Second)},
			expectError: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				hits         atomic.Int32
				gotPath      atomic.Value
				gotUsername  atomic.Value
				gotToken     atomic.Value
				gotBody      atomic.Value
				pruneCalls   atomic.Int32
				gotKeepName  atomic.Value
				gotPruneOpts atomic.Int32
			)

			// Stub the prune step: it talks to the runtime over ConnectRPC, which the
			// plain httptest server below cannot serve, and the assertions here only
			// concern whether it runs after a successful write and with which file name.
			// The stub is a package-level variable, so this test cannot run in parallel.
			savedPrune := pruneStaleSecurityTokenFilesFn
			t.Cleanup(func() { pruneStaleSecurityTokenFilesFn = savedPrune })
			pruneStaleSecurityTokenFilesFn = func(_ context.Context, _ *agentsv1alpha1.Sandbox, keepFileName string,
				rtOpts ...runtime.Option) error {
				pruneCalls.Add(1)
				gotKeepName.Store(keepFileName)
				gotPruneOpts.Store(int32(len(rtOpts)))
				return tt.pruneErr
			}

			var serverURL string
			if tt.server != nil {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					gotPath.Store(r.URL.Query().Get("path"))
					gotUsername.Store(r.URL.Query().Get("username"))
					gotToken.Store(r.Header.Get("X-Access-Token"))
					body, _ := io.ReadAll(r.Body)
					gotBody.Store(string(body))
					w.WriteHeader(tt.server.statusCode)
					_, _ = w.Write([]byte(tt.server.body))
				}))
				defer srv.Close()
				serverURL = srv.URL
			}

			sbx := tt.sbxFactory(serverURL)
			err := WriteSecurityTokenToRuntime(context.Background(), sbx, tt.tokenResp, tt.rtOpts...)

			if tt.expectError == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				if tt.mustNotLeak != "" {
					assert.NotContains(t, err.Error(), tt.mustNotLeak,
						"a sandbox access token must never reach errors or logs")
				}
				if tt.pruneErr != nil {
					// The prune failure must stay classifiable through the wrap chain.
					require.ErrorIs(t, err, tt.pruneErr)
				}
			}

			// The HTTP server should be hit only when both the sandbox and the runtime
			// URL annotation are present and the token response carries an access token.
			expectHit := tt.server != nil && sbx != nil && tt.tokenResp != nil &&
				tt.tokenResp.AccessToken != "" &&
				runtime.GetRuntimeURL(sbx) != ""
			if expectHit {
				assert.Equal(t, int32(1), hits.Load(), "runtime files API should be called exactly once")
				assert.Equal(t, expectedTokenFilePath(sbx), gotPath.Load(), "request path query must match the deterministic identity hash")
				assert.Equal(t, "root", gotUsername.Load(), "request must default to root username")
				assert.Equal(t, "runtime-access-token", gotToken.Load(), "X-Access-Token must carry the runtime access token annotation")
				body, _ := gotBody.Load().(string)
				assert.Contains(t, body, `"accessToken":"tok-1"`,
					"multipart body must embed the JSON-marshaled token")
			} else {
				assert.Equal(t, int32(0), hits.Load(), "runtime files API must not be called when preconditions are not met")
			}

			// Stale token files are pruned only after the write succeeded, so a failed
			// or skipped write must leave the existing files untouched.
			if expectHit && tt.server.statusCode < http.StatusBadRequest {
				assert.Equal(t, int32(1), pruneCalls.Load(), "stale token files must be pruned once after a successful write")
				assert.Equal(t, expectedTokenFileName(sbx), gotKeepName.Load(),
					"the file just written must be kept by the prune step")
				assert.Equal(t, int32(len(tt.rtOpts)), gotPruneOpts.Load(),
					"the prune must run over exactly the transport resolved for the sandbox")
			} else {
				assert.Equal(t, int32(0), pruneCalls.Load(), "prune must not run when the token file was not written")
			}
		})
	}
}

func TestRegisterPropagator(t *testing.T) {
	saved := propagators
	defer func() { propagators = saved }()
	propagators = nil

	RegisterPropagator(func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *identity.TokenResponse,
		_ ...runtime.Option) error {
		return nil
	})
	assert.Len(t, propagators, 1)

	RegisterPropagator(func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *identity.TokenResponse,
		_ ...runtime.Option) error {
		return fmt.Errorf("err")
	})
	assert.Len(t, propagators, 2)
}
