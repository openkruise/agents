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

package opensandbox

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/runtime/envd"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
)

// omitExecutionHeader leaves the credential header off entirely, which is a
// different request from carrying an empty value.
const omitExecutionHeader = "\x00omit"

const handshakeToken = "runtime-token"

// nativeStub counts every native call the sidecar makes. The handshake and
// per-request authentication contracts below depend on those counts: a single
// read-only List during /init and none afterwards is the agreed boundary.
type nativeStub struct {
	calls     atomic.Int32
	listCalls atomic.Int32
	seenToken atomic.Value
	files     http.Handler
	process   http.Handler
}

func (n *nativeStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n.calls.Add(1)
	if token := r.Header.Get("X-Access-Token"); token != "" {
		n.seenToken.Store(token)
	}
	switch r.URL.Path {
	case processconnect.ProcessListProcedure:
		n.listCalls.Add(1)
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, "invalid native request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	case "/files":
		n.files.ServeHTTP(w, r)
	default:
		n.process.ServeHTTP(w, r)
	}
}

// stubServer wires the sidecar to one counting native endpoint. The handler
// drains request bodies so HTTP/1 cancellation stays observable.
func stubServer(t *testing.T, processHandler, filesHandler http.Handler) (*Server, *nativeStub) {
	t.Helper()
	stub := &nativeStub{files: filesHandler, process: processHandler}
	native := httptest.NewServer(stub)
	t.Cleanup(native.Close)
	s := New(envd.Client{BaseURL: native.URL, HTTP: native.Client()})
	t.Cleanup(s.Close)
	return s, stub
}

// stubProcessHandler serves the native process procedures used by business routes.
func stubProcessHandler(t *testing.T, pid uint32) (*commandTestProcess, http.Handler) {
	t.Helper()
	proc := &commandTestProcess{start: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
		if err := stream.Send(commandStartEvent(pid)); err != nil {
			return err
		}
		return stream.Send(commandEndEvent(0, true, nil))
	}}
	_, handler := processconnect.NewProcessHandler(proc)
	return proc, handler
}

func executionTestRequest(s *Server, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != omitExecutionHeader {
		r.Header.Set(executionHeader, token)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestInitializationHandshake(t *testing.T) {
	for _, tt := range []struct {
		name          string
		body          string
		listStatus    int
		unreachable   bool
		wantStatus    int
		wantListCalls int32
	}{
		{name: "absent access token", body: `{}`, wantStatus: 400},
		{name: "empty access token", body: `{"accessToken":""}`, wantStatus: 400},
		{name: "malformed body", body: `not-json`, wantStatus: 400},
		{name: "native accepts the token", body: `{"accessToken":"runtime-token"}`, listStatus: 200, wantStatus: 204, wantListCalls: 1},
		{name: "native rejects the token", body: `{"accessToken":"wrong-token"}`, listStatus: 401, wantStatus: 401, wantListCalls: 1},
		{name: "native not ready", body: `{"accessToken":"runtime-token"}`, listStatus: 500, wantStatus: 503, wantListCalls: 1},
		{name: "native unreachable", body: `{"accessToken":"runtime-token"}`, unreachable: true, wantStatus: 503},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var listed atomic.Int32
			var s *Server
			if tt.unreachable {
				closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				url := closed.URL
				closed.Close()
				s = New(envd.Client{BaseURL: url, HTTP: closed.Client()})
			} else {
				native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != processconnect.ProcessListProcedure {
						t.Errorf("unexpected native endpoint %s during the handshake", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
						return
					}
					listed.Add(1)
					w.WriteHeader(tt.listStatus)
				}))
				t.Cleanup(native.Close)
				s = New(envd.Client{BaseURL: native.URL, HTTP: native.Client()})
			}
			defer s.Close()
			w := executionTestRequest(s, "POST", "/init", tt.body, omitExecutionHeader)
			require.Equal(t, tt.wantStatus, w.Code, w.Body.String())
			require.EqualValues(t, tt.wantListCalls, listed.Load(),
				"the handshake makes exactly one read-only List call and nothing else")
		})
	}
}

func TestHandshakeCannotProveEnvdInstalledTheToken(t *testing.T) {
	// A fresh envd without a configured token answers List regardless of the
	// token it is given, so a successful handshake cannot be the deployment's
	// evidence that authentication is active. This is why the proposal assigns
	// pre-initialization isolation to the deployment rather than the sidecar.
	var unauthenticated atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Access-Token") == "" {
			unauthenticated.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer native.Close()
	s := New(envd.Client{BaseURL: native.URL, HTTP: native.Client()})
	defer s.Close()
	require.Equal(t, 204, executionTestRequest(s, "POST", "/init", `{"accessToken":"runtime-token"}`, omitExecutionHeader).Code)
	require.Zero(t, unauthenticated.Load(), "the handshake must not probe envd without a credential")
}

func TestPerRequestAuthentication(t *testing.T) {
	upload := &fileUploadCapture{response: `[{"path":"/tmp/a"}]`}
	files := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			upload.ServeHTTP(w, r)
			return
		}
		if r.URL.Query().Get("path") == "missing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, "content")
	})
	_, processHandler := stubProcessHandler(t, 42)
	s, stub := stubServer(t, processHandler, files)
	// Command streaming needs a writer that supports flushing and deadlines, so
	// these cases run over a loopback listener instead of a recorder.
	api := httptest.NewServer(s)
	t.Cleanup(api.Close)

	absolute := func(t *testing.T, target string) *url.URL {
		t.Helper()
		parsed, err := url.Parse(target)
		require.NoError(t, err)
		return parsed
	}
	for _, tt := range []struct {
		name       string
		request    func(*testing.T, string) *http.Request
		wantStatus int
	}{
		{name: "foreground command", wantStatus: http.StatusOK, request: func(t *testing.T, base string) *http.Request {
			r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/command", strings.NewReader(`{"command":"true"}`))
			require.NoError(t, err)
			return r
		}},
		{name: "explicit interrupt", wantStatus: http.StatusOK, request: func(t *testing.T, base string) *http.Request {
			r, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, base+"/command?id=42", nil)
			require.NoError(t, err)
			return r
		}},
		{name: "file upload", wantStatus: http.StatusOK, request: func(t *testing.T, base string) *http.Request {
			r := fileUploadRequest(t, []string{`{"path":"/tmp/a"}`})
			r.URL = absolute(t, base+"/files/upload")
			r.RequestURI = ""
			return r
		}},
		{name: "file download", wantStatus: http.StatusOK, request: func(t *testing.T, base string) *http.Request {
			r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/files/download?path=/tmp/a", nil)
			require.NoError(t, err)
			return r
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, variant := range []struct {
				name  string
				apply func(http.Header)
			}{
				{name: "missing credential", apply: func(h http.Header) { h.Del(executionHeader) }},
				{name: "empty credential", apply: func(h http.Header) { h.Set(executionHeader, "") }},
			} {
				t.Run(variant.name, func(t *testing.T) {
					before, lists := stub.calls.Load(), stub.listCalls.Load()
					r := tt.request(t, api.URL)
					variant.apply(r.Header)
					resp, err := api.Client().Do(r)
					require.NoError(t, err)
					require.NoError(t, resp.Body.Close())
					require.Equal(t, 401, resp.StatusCode)
					require.EqualValues(t, 0, stub.calls.Load()-before, "rejection must not reach envd")
					require.EqualValues(t, lists, stub.listCalls.Load())
				})
			}
			t.Run("credential is forwarded without preflight", func(t *testing.T) {
				// The first and a later request must take the same path: the value
				// travels on the operation itself and nothing lists processes.
				lists := stub.listCalls.Load()
				for attempt := 1; attempt <= 2; attempt++ {
					before := stub.calls.Load()
					r := tt.request(t, api.URL)
					r.Header.Set(executionHeader, handshakeToken)
					resp, err := api.Client().Do(r)
					require.NoError(t, err)
					_, _ = io.Copy(io.Discard, resp.Body)
					require.NoError(t, resp.Body.Close())
					require.Equal(t, tt.wantStatus, resp.StatusCode, "attempt %d", attempt)
					require.Greater(t, stub.calls.Load(), before, "attempt %d must reach envd", attempt)
					require.EqualValues(t, lists, stub.listCalls.Load(), "business requests add no List call")
					require.Equal(t, handshakeToken, stub.seenToken.Load(), "attempt %d must forward the request token", attempt)
				}
			})
		})
	}
}

func TestPingConfirmsSidecarReachabilityOnly(t *testing.T) {
	s, stub := stubServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("health check must not reach the native process procedures")
	}), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("health check must not reach the native file endpoint")
	}))
	for _, token := range []string{omitExecutionHeader, "", handshakeToken} {
		w := executionTestRequest(s, "GET", "/ping", "", token)
		require.Equal(t, 200, w.Code, w.Body.String())
	}
	require.Zero(t, stub.calls.Load(), "the default health check makes no native call")
}

func TestUnsupportedRouteClassification(t *testing.T) {
	s, _ := stubServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("unsupported routes must not reach envd")
	}), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("unsupported routes must not reach envd")
	}))
	for _, tt := range []struct {
		method, path string
		status       int
	}{
		{"POST", "/code", 501},
		{"DELETE", "/code/contexts/context-1", 501},
		{"POST", "/session/session-1/run", 501},
		{"GET", "/command/status/123", 501},
		{"GET", "/command/123/logs", 501},
		{"GET", "/command/status/logs", 501},
		{"DELETE", "/files", 501},
		{"GET", "/files/info?path=%2Ftmp%2Fa", 501},
		{"POST", "/files/permissions", 501},
		{"GET", "/directories/list", 501},
		{"GET", "/metrics/watch", 501},
		{"GET", "/pty/session-1/ws", 501},
		{"POST", "/v1/isolated/session", 501},
		{"GET", "/v1/isolated/session/session-1/runs/run-1/logs", 501},
		{"POST", "/v1/isolated/session/session-1/files/upload", 501},
		{"GET", "/v1/isolated/session/session-1/directories/list", 501},
		{"PATCH", "/policy", 501},
		{"POST", "/credential-vault", 501},
		{"GET", "/credential-vault/credentials/key-1", 501},
		{"GET", "/credential-vault/bindings/binding-1", 501},
		{"GET", "/code-typo", 404},
		{"GET", "/files/not-a-route", 404},
		{"GET", "/files/info/extra", 404},
		{"POST", "/files/info", 404},
		{"HEAD", "/files/info", 404},
		{"GET", "/command/123/status", 404},
		{"GET", "/command/123/logs/extra", 404},
		{"GET", "/pty/session-1/ws/extra", 404},
		{"POST", "/v1/isolated/session/session-1/files/upload/extra", 404},
		{"GET", "/policy/extra", 404},
		{"PUT", "/policy", 404},
		{"GET", "/credential-vault/credentials/key-1/extra", 404},
		{"GET", "/credential-vault/_active", 404},
		{"POST", "/internal/init", 404},
	} {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			w := executionTestRequest(s, tt.method, tt.path, "", handshakeToken)
			require.Equal(t, tt.status, w.Code, w.Body.String())
			code := "NOT_FOUND"
			if tt.status == http.StatusNotImplemented {
				code = "NOT_SUPPORTED"
			}
			require.Contains(t, w.Body.String(), `"code":"`+code+`"`)
		})
	}
}

func TestCloseStopsAdmissionAndJoinsForwarding(t *testing.T) {
	s, _ := stubServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	reached := make(chan struct{})
	s.mux.HandleFunc("GET /held", func(_ http.ResponseWriter, r *http.Request) {
		close(reached)
		<-r.Context().Done()
	})
	forwarded := make(chan struct{})
	go func() {
		executionTestRequest(s, "GET", "/held", "", handshakeToken)
		close(forwarded)
	}()
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("forwarding did not start")
	}
	s.Close()
	select {
	case <-forwarded:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not release in-flight forwarding")
	}
	for _, path := range []string{"/ping", "/policy"} {
		require.Equal(t, 503, executionTestRequest(s, "GET", path, "", handshakeToken).Code, path)
	}
}
