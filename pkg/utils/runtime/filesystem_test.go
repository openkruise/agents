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
	"encoding/pem"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/proto/envd/filesystem"
	"github.com/openkruise/agents/proto/envd/filesystem/filesystemconnect"
)

func TestReadFile(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		maxBytes int64
		username string
		authUser string
		token    string
		sentinel error
	}{
		{name: "binary at limit with auth", status: http.StatusOK, body: "\x00\xff\x80\r\n", maxBytes: 5, authUser: "root", token: "runtime-token"},
		{name: "custom username and escaped path", status: http.StatusOK, body: "file", maxBytes: 8, username: "agent user", authUser: "agent", token: "runtime-token"},
		{name: "empty file without auth", status: http.StatusOK, maxBytes: 1},
		{name: "oversized file", status: http.StatusOK, body: "secret-content", maxBytes: 3, sentinel: ErrRuntimeFileTooLarge},
		{name: "not found", status: http.StatusNotFound, body: "secret-content", maxBytes: 3, sentinel: ErrRuntimePathNotFound},
		{name: "unauthorized", status: http.StatusUnauthorized, body: "secret-content", maxBytes: 3},
		{name: "forbidden", status: http.StatusForbidden, body: "secret-content", maxBytes: 3},
		{name: "server error", status: http.StatusInternalServerError, body: "secret-content", maxBytes: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			captured := &capturedFilesRequest{}
			server := newRuntimeFilesServer(t, tt.status, tt.body, captured)
			t.Cleanup(server.Close)
			const filePath = "/tmp/a b&c.bin"
			content, err := NewRuntime(newWriteFileSandbox(server.URL+"/", tt.token)).Filesystem().ReadFile(
				context.Background(), ReadFileRequest{
					FilePath: filePath, Username: tt.username, AuthUser: tt.authUser, MaxBytes: tt.maxBytes,
				})
			if tt.sentinel != nil || tt.status >= http.StatusBadRequest {
				require.Error(t, err)
				assert.Nil(t, content)
				assert.NotContains(t, err.Error(), tt.body)
				if tt.sentinel != nil {
					assert.ErrorIs(t, err, tt.sentinel)
				}
				if tt.status >= http.StatusBadRequest {
					var apiErr *APIError
					require.ErrorAs(t, err, &apiErr)
					assert.Equal(t, tt.status, apiErr.StatusCode)
					assert.Equal(t, "/files", apiErr.Path)
					assert.Empty(t, apiErr.Message)
					assert.Equal(t, tt.status < 500, apiErr.IsClientError())
					assert.NotErrorIs(t, err, ErrRuntimeFileTooLarge)
					if tt.status != http.StatusNotFound {
						assert.NotErrorIs(t, err, ErrRuntimePathNotFound)
					}
				}
			} else {
				require.NoError(t, err)
				assert.Equal(t, []byte(tt.body), content)
			}
			assert.Equal(t, http.MethodGet, captured.method)
			assert.Equal(t, "/files", captured.path)
			assert.Equal(t, filePath, captured.query.Get("path"))
			username := tt.username
			if username == "" {
				username = "root"
			}
			assert.Equal(t, username, captured.query.Get("username"))
			assert.Equal(t, tt.token, captured.accessToken)
			auth := ""
			if tt.authUser != "" {
				auth = basicAuthHeader(tt.authUser)
			}
			assert.Equal(t, auth, captured.authorization)
			assert.Empty(t, captured.contentType)
		})
	}
}

func TestReadFile_InputValidation(t *testing.T) {
	tests := []struct {
		name        string
		filePath    string
		maxBytes    int64
		runtimeURL  string
		opts        []Option
		expectError string
	}{
		{name: "missing file path", maxBytes: 1, runtimeURL: "http://unused", expectError: "filePath is required"},
		{name: "zero limit", filePath: "/tmp/f", runtimeURL: "http://unused", expectError: "maxBytes must be positive"},
		{name: "negative limit", filePath: "/tmp/f", maxBytes: -1, runtimeURL: "http://unused", expectError: "maxBytes must be positive"},
		{name: "overflowing limit", filePath: "/tmp/f", maxBytes: math.MaxInt64, runtimeURL: "http://unused", expectError: "maxBytes must be positive"},
		{name: "missing runtime URL", filePath: "/tmp/f", maxBytes: 1, expectError: "runtime url not found on sandbox"},
		{name: "invalid runtime URL", filePath: "/tmp/f", maxBytes: 1, runtimeURL: "://invalid", expectError: "failed to build runtime files read request"},
		{name: "invalid TLS bundle", filePath: "/tmp/f", maxBytes: 1, runtimeURL: "http://unused", opts: []Option{WithTLS(TLSBundle{CABundle: []byte("not pem")})}, expectError: "invalid runtime TLS configuration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := NewRuntime(newWriteFileSandbox(tt.runtimeURL, ""), tt.opts...).Filesystem().ReadFile(
				context.Background(), ReadFileRequest{FilePath: tt.filePath, MaxBytes: tt.maxBytes})
			require.ErrorContains(t, err, tt.expectError)
			assert.Nil(t, content)
		})
	}
}

func TestReadFile_ContextTimeout(t *testing.T) {
	tests := []struct {
		name           string
		flushBody      bool
		parentDeadline bool
	}{
		{name: "before response headers"},
		{name: "during response body", flushBody: true},
		{name: "parent deadline wins", parentDeadline: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.flushBody {
					_, _ = w.Write([]byte("x"))
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			t.Cleanup(server.Close)
			ctx := context.Background()
			timeout := 50 * time.Millisecond
			if tt.parentDeadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
				timeout = time.Second
			}
			start := time.Now()
			content, err := NewRuntime(newWriteFileSandbox(server.URL, "")).Filesystem().ReadFile(
				ctx, ReadFileRequest{FilePath: "/tmp/f", MaxBytes: 16, Timeout: timeout})
			assert.ErrorIs(t, err, context.DeadlineExceeded)
			assert.Nil(t, content)
			assert.Less(t, time.Since(start), time.Second)
		})
	}
}

func TestReadFile_TLS(t *testing.T) {
	requests := make(chan *http.Request, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r
		_, _ = w.Write([]byte{0, 255})
	}))
	t.Cleanup(server.Close)
	host, portStr, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	sbx := sandboxWithPodIP(host)
	sbx.Annotations = map[string]string{agentsv1alpha1.AnnotationRuntimeAccessToken: "tls-token"}
	content, err := NewRuntime(sbx, WithTLS(TLSBundle{CABundle: caPEM}), WithAuthority("example.com"), WithTLSPort(port)).
		Filesystem().ReadFile(context.Background(), ReadFileRequest{FilePath: "/tmp/f", MaxBytes: 2, AuthUser: "root"})
	require.NoError(t, err)
	assert.Equal(t, []byte{0, 255}, content)
	req := <-requests
	assert.Equal(t, http.MethodGet, req.Method)
	assert.Equal(t, "/files", req.URL.Path)
	assert.Equal(t, "/tmp/f", req.URL.Query().Get("path"))
	assert.Equal(t, "root", req.URL.Query().Get("username"))
	assert.Equal(t, net.JoinHostPort("example.com", portStr), req.Host)
	assert.Equal(t, "example.com", req.TLS.ServerName)
	assert.Equal(t, "tls-token", req.Header.Get(accessTokenHeader))
	assert.Equal(t, basicAuthHeader("root"), req.Header.Get("Authorization"))
}

type runtimeFilesRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn runtimeFilesRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type runtimeFilesReadBody struct {
	io.Reader
	bytesRead int
	closed    bool
}

func (b *runtimeFilesReadBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.bytesRead += n
	return n, err
}

func (b *runtimeFilesReadBody) Close() error {
	b.closed = true
	return nil
}

func TestReadFile_BoundedBodyAndClientSeam(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		timeout   time.Duration
		bytesRead int
	}{
		{name: "only max plus one bytes", status: http.StatusOK, bytesRead: 4},
		{name: "negative timeout defaults", status: http.StatusOK, timeout: -time.Second, bytesRead: 4},
		{name: "large timeout remains uncapped", status: http.StatusOK, timeout: 30 * time.Second, bytesRead: 4},
		{name: "404 body is not read", status: http.StatusNotFound},
		{name: "500 body is not read", status: http.StatusInternalServerError},
		{name: "redirect body is not read", status: http.StatusTemporaryRedirect},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := &runtimeFilesReadBody{Reader: strings.NewReader("secret-content")}
			var requestCtx context.Context
			originalClient := runtimeFilesHTTPClient
			runtimeFilesHTTPClient = &http.Client{Transport: runtimeFilesRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				requestCtx = req.Context()
				deadline, ok := requestCtx.Deadline()
				require.True(t, ok)
				timeout := tt.timeout
				if timeout <= 0 {
					timeout = defaultRuntimeWriteTimeout
				}
				assert.InDelta(t, timeout.Seconds(), time.Until(deadline).Seconds(), 1)
				return &http.Response{StatusCode: tt.status, Body: body, Header: make(http.Header)}, nil
			})}
			t.Cleanup(func() { runtimeFilesHTTPClient = originalClient })
			content, err := NewRuntime(newWriteFileSandbox("http://unused", "")).Filesystem().ReadFile(
				context.Background(), ReadFileRequest{FilePath: "/tmp/f", MaxBytes: 3, Timeout: tt.timeout})
			require.Error(t, err)
			assert.Nil(t, content)
			assert.NotContains(t, err.Error(), "secret-content")
			assert.Equal(t, tt.bytesRead, body.bytesRead)
			assert.True(t, body.closed)
			require.NotNil(t, requestCtx)
			assert.ErrorIs(t, requestCtx.Err(), context.Canceled)
			if tt.status == http.StatusOK {
				assert.ErrorIs(t, err, ErrRuntimeFileTooLarge)
			} else {
				var apiErr *APIError
				require.ErrorAs(t, err, &apiErr)
				assert.Equal(t, tt.status, apiErr.StatusCode)
			}
		})
	}
}

type runtimeFilesReadError struct{}

func (runtimeFilesReadError) Read(p []byte) (int, error) {
	return copy(p, "pod-uid-extra"), io.ErrUnexpectedEOF
}

func TestReadFile_OversizedResponsePreservesReadError(t *testing.T) {
	for _, publication := range []bool{false, true} {
		t.Run(fmt.Sprintf("publication=%t", publication), func(t *testing.T) {
			body := &runtimeFilesReadBody{Reader: runtimeFilesReadError{}}
			original := runtimeFilesHTTPClient
			runtimeFilesHTTPClient = &http.Client{Transport: runtimeFilesRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
			})}
			t.Cleanup(func() { runtimeFilesHTTPClient = original })
			box := newWriteFileSandbox("http://unused", "")
			box.Status.PodInfo.PodUID = "pod-uid"
			var err error
			if publication {
				var published bool
				published, err = StartupDataPublished(t.Context(), box)
				assert.False(t, published)
			} else {
				_, err = NewRuntime(box).Filesystem().ReadFile(t.Context(), ReadFileRequest{FilePath: "/tmp/f", MaxBytes: 3})
			}
			require.ErrorIs(t, err, io.ErrUnexpectedEOF)
			assert.NotErrorIs(t, err, ErrRuntimeFileTooLarge)
			assert.True(t, body.closed)
		})
	}
}

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
