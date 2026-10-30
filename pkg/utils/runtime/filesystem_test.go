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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/proto/envd/filesystem"
	"github.com/openkruise/agents/proto/envd/filesystem/filesystemconnect"
)

// mockFilesystemHandler serves the Filesystem service and records the headers and
// request payload of the last call, so tests can assert both the wire contract and
// the error classification.
type mockFilesystemHandler struct {
	filesystemconnect.UnimplementedFilesystemHandler

	listFn   func(req *connect.Request[filesystem.ListDirRequest]) (*connect.Response[filesystem.ListDirResponse], error)
	removeFn func(req *connect.Request[filesystem.RemoveRequest]) (*connect.Response[filesystem.RemoveResponse], error)

	gotAccessToken string
	gotAuth        string
}

func (m *mockFilesystemHandler) ListDir(_ context.Context, req *connect.Request[filesystem.ListDirRequest],
) (*connect.Response[filesystem.ListDirResponse], error) {
	m.recordHeaders(req.Header())
	if m.listFn == nil {
		return connect.NewResponse(&filesystem.ListDirResponse{}), nil
	}
	return m.listFn(req)
}

func (m *mockFilesystemHandler) Remove(_ context.Context, req *connect.Request[filesystem.RemoveRequest],
) (*connect.Response[filesystem.RemoveResponse], error) {
	m.recordHeaders(req.Header())
	if m.removeFn == nil {
		return connect.NewResponse(&filesystem.RemoveResponse{}), nil
	}
	return m.removeFn(req)
}

func (m *mockFilesystemHandler) recordHeaders(h http.Header) {
	m.gotAccessToken = h.Get(accessTokenHeader)
	m.gotAuth = h.Get("Authorization")
}

// newMockFilesystemSandbox starts handler and returns a Sandbox annotated with its
// URL and an access token.
func newMockFilesystemSandbox(t *testing.T, handler *mockFilesystemHandler) *agentsv1alpha1.Sandbox {
	t.Helper()
	_, h := filesystemconnect.NewFilesystemHandler(handler)
	mux := http.NewServeMux()
	mux.Handle("/", h)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-fs",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationRuntimeURL:         server.URL,
				agentsv1alpha1.AnnotationRuntimeAccessToken: "runtime-access-token",
			},
		},
	}
}

// sandboxWithoutRuntimeURL builds a Sandbox that exposes no runtime endpoint, so a
// call fails before reaching the wire.
func sandboxWithoutRuntimeURL() *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "no-url", Namespace: "default"},
	}
}

func TestListDirWithRuntime(t *testing.T) {
	const dir = "/var/opt/sandbox/agent-token"

	tests := []struct {
		name string
		// noRuntimeURL replaces the served sandbox with one that has no endpoint.
		noRuntimeURL   bool
		path           string
		depth          uint32
		authUser       string
		listFn         func(req *connect.Request[filesystem.ListDirRequest]) (*connect.Response[filesystem.ListDirResponse], error)
		expectNames    []string
		expectError    string
		expectSentinel error
	}{
		{
			name:     "entries are returned and the request carries path, depth and auth",
			path:     dir,
			depth:    1,
			authUser: "root",
			listFn: func(req *connect.Request[filesystem.ListDirRequest]) (*connect.Response[filesystem.ListDirResponse], error) {
				if got := req.Msg.GetPath(); got != dir {
					return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unexpected path %q", got))
				}
				if got := req.Msg.GetDepth(); got != 1 {
					return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unexpected depth %d", got))
				}
				return connect.NewResponse(&filesystem.ListDirResponse{Entries: []*filesystem.EntryInfo{
					{Name: "a.token", Path: dir + "/a.token", Type: filesystem.FileType_FILE_TYPE_FILE},
					{Name: "nested", Path: dir + "/nested", Type: filesystem.FileType_FILE_TYPE_DIRECTORY},
				}}), nil
			},
			expectNames: []string{"a.token", "nested"},
		},
		{
			name:        "empty directory yields no entries and no error",
			path:        dir,
			authUser:    "root",
			expectNames: nil,
		},
		{
			name:     "missing directory wraps ErrRuntimePathNotFound",
			path:     dir,
			authUser: "root",
			listFn: func(*connect.Request[filesystem.ListDirRequest]) (*connect.Response[filesystem.ListDirResponse], error) {
				return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("directory not found"))
			},
			expectError:    "path not found in sandbox runtime",
			expectSentinel: ErrRuntimePathNotFound,
		},
		{
			// This is exactly how an agent-runtime predating the Filesystem service
			// answers, and the signal callers degrade on.
			name:     "unimplemented service wraps ErrRuntimeFilesystemUnsupported",
			path:     dir,
			authUser: "root",
			listFn: func(*connect.Request[filesystem.ListDirRequest]) (*connect.Response[filesystem.ListDirResponse], error) {
				return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("unknown service"))
			},
			expectError:    "filesystem service unsupported by sandbox runtime",
			expectSentinel: ErrRuntimeFilesystemUnsupported,
		},
		{
			name:     "path that is not a directory stays a generic failure",
			path:     dir,
			authUser: "root",
			listFn: func(*connect.Request[filesystem.ListDirRequest]) (*connect.Response[filesystem.ListDirResponse], error) {
				return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("path is not a directory"))
			},
			expectError: "runtime filesystem ListDir",
		},
		{
			name:        "empty path is rejected before any call",
			path:        "",
			authUser:    "root",
			expectError: "path is required",
		},
		{
			// The service resolves paths per user and answers Unauthenticated without
			// one, so the helper refuses to spend a round trip.
			name:        "empty auth user is rejected before any call",
			path:        dir,
			authUser:    "",
			expectError: "authUser is required",
		},
		{
			name:         "missing runtime URL is rejected before any call",
			noRuntimeURL: true,
			path:         dir,
			authUser:     "root",
			expectError:  "runtime url not found on sandbox",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &mockFilesystemHandler{listFn: tt.listFn}
			sbx := newMockFilesystemSandbox(t, handler)
			if tt.noRuntimeURL {
				sbx = sandboxWithoutRuntimeURL()
			}

			entries, err := ListDirWithRuntime(context.Background(), ListDirArgs{
				Sbx:      sbx,
				Path:     tt.path,
				Depth:    tt.depth,
				AuthUser: tt.authUser,
			})

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Nil(t, entries)
				if tt.expectSentinel != nil {
					assert.ErrorIs(t, err, tt.expectSentinel)
				} else {
					assert.NotErrorIs(t, err, ErrRuntimePathNotFound)
					assert.NotErrorIs(t, err, ErrRuntimeFilesystemUnsupported)
				}
				return
			}

			require.NoError(t, err)
			var names []string
			for _, entry := range entries {
				names = append(names, entry.GetName())
			}
			assert.Equal(t, tt.expectNames, names)
			assert.Equal(t, "runtime-access-token", handler.gotAccessToken,
				"the sandbox access token must gate the RPC")
			assert.Equal(t, basicAuthHeader(tt.authUser), handler.gotAuth,
				"the auth user must be sent as a Basic credential")
		})
	}
}

func TestRemovePathWithRuntime(t *testing.T) {
	const filePath = "/var/opt/sandbox/agent-token/a.token"

	tests := []struct {
		name           string
		noRuntimeURL   bool
		path           string
		authUser       string
		removeFn       func(req *connect.Request[filesystem.RemoveRequest]) (*connect.Response[filesystem.RemoveResponse], error)
		expectError    string
		expectSentinel error
	}{
		{
			name:     "path is removed and the request carries it verbatim",
			path:     filePath,
			authUser: "root",
			removeFn: func(req *connect.Request[filesystem.RemoveRequest]) (*connect.Response[filesystem.RemoveResponse], error) {
				if got := req.Msg.GetPath(); got != filePath {
					return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unexpected path %q", got))
				}
				return connect.NewResponse(&filesystem.RemoveResponse{}), nil
			},
		},
		{
			name:     "removal failure is surfaced",
			path:     filePath,
			authUser: "root",
			removeFn: func(*connect.Request[filesystem.RemoveRequest]) (*connect.Response[filesystem.RemoveResponse], error) {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("error removing file or directory"))
			},
			expectError: "runtime filesystem Remove",
		},
		{
			// Removal is not idempotent: a caller that retries a cleanup, or that
			// treats "already gone" as success, has to tell this apart from a real
			// failure with errors.Is. Pin the mapping so that contract cannot
			// regress silently.
			name:     "missing path wraps ErrRuntimePathNotFound",
			path:     filePath,
			authUser: "root",
			removeFn: func(*connect.Request[filesystem.RemoveRequest]) (*connect.Response[filesystem.RemoveResponse], error) {
				return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("path not found"))
			},
			expectError:    "path not found in sandbox runtime",
			expectSentinel: ErrRuntimePathNotFound,
		},
		{
			name:     "unimplemented service wraps ErrRuntimeFilesystemUnsupported",
			path:     filePath,
			authUser: "root",
			removeFn: func(*connect.Request[filesystem.RemoveRequest]) (*connect.Response[filesystem.RemoveResponse], error) {
				return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("unknown service"))
			},
			expectError:    "filesystem service unsupported by sandbox runtime",
			expectSentinel: ErrRuntimeFilesystemUnsupported,
		},
		{
			name:        "empty path is rejected before any call",
			path:        "",
			authUser:    "root",
			expectError: "path is required",
		},
		{
			name:        "empty auth user is rejected before any call",
			path:        filePath,
			authUser:    "",
			expectError: "authUser is required",
		},
		{
			name:         "missing runtime URL is rejected before any call",
			noRuntimeURL: true,
			path:         filePath,
			authUser:     "root",
			expectError:  "runtime url not found on sandbox",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &mockFilesystemHandler{removeFn: tt.removeFn}
			sbx := newMockFilesystemSandbox(t, handler)
			if tt.noRuntimeURL {
				sbx = sandboxWithoutRuntimeURL()
			}

			err := RemovePathWithRuntime(context.Background(), RemovePathArgs{
				Sbx:      sbx,
				Path:     tt.path,
				AuthUser: tt.authUser,
			})

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				if tt.expectSentinel != nil {
					assert.ErrorIs(t, err, tt.expectSentinel)
				} else {
					assert.NotErrorIs(t, err, ErrRuntimeFilesystemUnsupported)
				}
				return
			}

			require.NoError(t, err)
			assert.Equal(t, "runtime-access-token", handler.gotAccessToken)
			assert.Equal(t, basicAuthHeader(tt.authUser), handler.gotAuth)
		})
	}
}
