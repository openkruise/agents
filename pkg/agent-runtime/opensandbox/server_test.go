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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/runtime/envd"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
)

func bindingTestRequest(s *Server, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set(executionHeader, token)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func bindingTestServer(t *testing.T, nativeHandler http.Handler, token string) (*Server, string) {
	t.Helper()
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Native Connect handlers decode the unary body before their operation.
		// Reading it also lets HTTP/1 observe connection cancellation while blocked.
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, "invalid native request", http.StatusBadRequest)
			return
		}
		nativeHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(native.Close)
	state := filepath.Join(t.TempDir(), "token")
	if token != "" {
		require.NoError(t, os.WriteFile(state, []byte(token), 0600))
	}
	s, err := New(envd.Client{BaseURL: native.URL, HTTP: native.Client()}, state)
	require.NoError(t, err)
	t.Cleanup(s.Close)
	return s, state
}

func TestInitializationAuthentication(t *testing.T) {
	var initialized atomic.Bool
	var probes atomic.Int32
	native := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != processconnect.ProcessListProcedure {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		probes.Add(1)
		if initialized.Load() && r.Header.Get("X-Access-Token") != "runtime-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	s, state := bindingTestServer(t, native, "")
	for _, tt := range []struct {
		name, method, path, body, token string
		init                            bool
		status                          int
	}{
		{name: "pre-init is closed", method: "POST", path: "/command", body: `{"command":"echo unsafe"}`, status: 503},
		{name: "unprotected envd cannot authorize init", method: "POST", path: "/init", body: `{"accessToken":"runtime-token"}`, status: 503},
		{name: "wrong init token", method: "POST", path: "/init", body: `{"accessToken":"wrong-token"}`, init: true, status: 401},
		{name: "native token binds service", method: "POST", path: "/init", body: `{"accessToken":"runtime-token"}`, init: true, status: 204},
		{name: "missing credential", method: "GET", path: "/policy", init: true, status: 401},
		{name: "wrong credential", method: "GET", path: "/policy", token: "wrong-token", init: true, status: 401},
		{name: "health needs no external token", method: "GET", path: "/ping", init: true, status: 200},
		{name: "ready needs no external token", method: "GET", path: "/ready", init: true, status: 200},
		{name: "explicit unsupported operation", method: "GET", path: "/policy", token: "runtime-token", init: true, status: 501},
		{name: "explicit unsupported vault", method: "POST", path: "/credential-vault", token: "runtime-token", init: true, status: 501},
		{name: "unknown route", method: "GET", path: "/unknown", token: "runtime-token", init: true, status: 404},
	} {
		t.Run(tt.name, func(t *testing.T) {
			initialized.Store(tt.init)
			before := probes.Load()
			w := bindingTestRequest(s, tt.method, tt.path, tt.body, tt.token)
			require.Equal(t, tt.status, w.Code, w.Body.String())
			if tt.path == "/ping" || tt.path == "/ready" {
				require.EqualValues(t, 2, probes.Load()-before, "health must use exactly the shared dual probe")
			}
		})
	}
	data, err := os.ReadFile(state)
	require.NoError(t, err)
	require.Equal(t, "runtime-token", string(data))
	info, err := os.Stat(state)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	restored, err := New(s.backend, state)
	require.NoError(t, err)
	defer restored.Close()
	require.Equal(t, "runtime-token", restored.binding.token)
	initialized.Store(false)
	require.Equal(t, 503, bindingTestRequest(restored, "GET", "/ready", "", "").Code,
		"a saved token does not authorize an uninitialized native runtime after restart")
	initialized.Store(true)
	require.Equal(t, 200, bindingTestRequest(restored, "GET", "/ready", "", "").Code)
}

func TestUnsupportedRouteClassification(t *testing.T) {
	s, _ := bindingTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != processconnect.ProcessListProcedure {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("X-Access-Token") != "runtime-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}), "runtime-token")
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
			w := bindingTestRequest(s, tt.method, tt.path, "", "runtime-token")
			require.Equal(t, tt.status, w.Code, w.Body.String())
			code := "NOT_FOUND"
			if tt.status == http.StatusNotImplemented {
				code = "NOT_SUPPORTED"
			}
			require.Contains(t, w.Body.String(), `"code":"`+code+`"`)
		})
	}
}

func TestCredentialReadiness(t *testing.T) {
	for _, tt := range []struct {
		name             string
		without, with    int
		path, token      string
		status, requests int
	}{
		{name: "uninitialized native", without: 200, with: 200, path: "/ping", status: 503, requests: 1},
		{name: "native token changed", without: 401, with: 401, path: "/ping", status: 503, requests: 2},
		{name: "native unavailable", without: 503, with: 200, path: "/ready", status: 503, requests: 1},
		{name: "native failure after negative probe", without: 401, with: 500, path: "/policy", token: "saved-token", status: 503, requests: 2},
		{name: "business also checks native readiness", without: 200, with: 200, path: "/policy", token: "saved-token", status: 503, requests: 1},
		{name: "wrong external token rejected before probing", without: 401, with: 200, path: "/policy", token: "wrong", status: 401, requests: 0},
		{name: "ready protected operation", without: 401, with: 200, path: "/policy", token: "saved-token", status: 501, requests: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			s, _ := bindingTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				status := tt.without
				if r.Header.Get("X-Access-Token") != "" {
					status = tt.with
				}
				w.WriteHeader(status)
			}), "saved-token")
			w := bindingTestRequest(s, "GET", tt.path, "", tt.token)
			require.Equal(t, tt.status, w.Code, w.Body.String())
			require.EqualValues(t, tt.requests, calls.Load())
		})
	}
}

func TestCredentialProbeSharesDeadline(t *testing.T) {
	var calls atomic.Int32
	s, _ := bindingTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-Access-Token") == "" {
			select {
			case <-time.After(600 * time.Millisecond):
				w.WriteHeader(http.StatusUnauthorized)
			case <-r.Context().Done():
			}
			return
		}
		<-r.Context().Done()
	}), "token")
	started := time.Now()
	w := bindingTestRequest(s, "GET", "/ready", "", "")
	require.Equal(t, 503, w.Code)
	require.EqualValues(t, 2, calls.Load())
	require.Less(t, time.Since(started), 1500*time.Millisecond, "both probes share one second, rather than one second each")
}

func TestCredentialPersistenceFailurePreservesBinding(t *testing.T) {
	s, state := bindingTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Access-Token") == "" {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(200)
	}), "old-token")
	old := s.binding
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocker, []byte("block"), 0600))
	s.stateFile = filepath.Join(blocker, "token")
	w := bindingTestRequest(s, "POST", "/init", `{"accessToken":"new-token"}`, "")
	require.Equal(t, 500, w.Code, w.Body.String())
	require.Same(t, old, s.binding)
	require.NoError(t, old.ctx.Err())
	data, err := os.ReadFile(state)
	require.NoError(t, err)
	require.Equal(t, "old-token", string(data))
}

func TestCredentialRebindingAndCloseReleaseForwarding(t *testing.T) {
	for _, closeService := range []bool{false, true} {
		name := "rebind"
		if closeService {
			name = "close"
		}
		t.Run(name, func(t *testing.T) {
			s, state := bindingTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Access-Token") == "" {
					w.WriteHeader(401)
					return
				}
				w.WriteHeader(200)
			}), "old-token")
			old := s.binding
			started := make(chan string, 1)
			finished := make(chan struct{})
			s.mux.HandleFunc("GET /held-forwarding", func(_ http.ResponseWriter, r *http.Request) {
				r.Header.Set(executionHeader, "mutated-request-header")
				started <- executionToken(r)
				<-r.Context().Done()
			})
			go func() {
				bindingTestRequest(s, "GET", "/held-forwarding", "", "old-token")
				close(finished)
			}()
			select {
			case token := <-started:
				require.Equal(t, "old-token", token, "forwarding uses the admitted snapshot, not a mutable header")
			case <-time.After(2 * time.Second):
				t.Fatal("forwarding did not start")
			}
			same := bindingTestRequest(s, "POST", "/init", `{"accessToken":"old-token"}`, "")
			require.Equal(t, 204, same.Code, same.Body.String())
			require.Same(t, old, s.binding)
			require.NoError(t, old.ctx.Err())
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				if closeService {
					s.Close()
					done <- nil
				} else {
					done <- bindingTestRequest(s, "POST", "/init", `{"accessToken":"new-token"}`, "")
				}
			}()
			select {
			case response := <-done:
				if response != nil {
					require.Equal(t, 204, response.Code, response.Body.String())
				}
			case <-time.After(2 * time.Second):
				t.Fatal("local forwarding did not close after generation cancellation")
			}
			require.ErrorIs(t, old.ctx.Err(), context.Canceled)
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("old request was not joined")
			}
			if closeService {
				require.Equal(t, 503, bindingTestRequest(s, "GET", "/ping", "", "").Code)
				require.Equal(t, 503, bindingTestRequest(s, "POST", "/init", `{"accessToken":"new-token"}`, "").Code)
			} else {
				require.Equal(t, old.generation+1, s.binding.generation)
				require.Equal(t, "new-token", s.binding.token)
				data, err := os.ReadFile(state)
				require.NoError(t, err)
				require.Equal(t, "new-token", string(data))
			}
		})
	}
}

func TestCredentialLateProbeCannotAdmitOldBinding(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	s, _ := bindingTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Access-Token") == "" {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("X-Access-Token") == "old-token" {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		w.WriteHeader(200)
	}), "old-token")
	probe := make(chan *httptest.ResponseRecorder, 1)
	go func() { probe <- bindingTestRequest(s, "GET", "/ready", "", "") }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("old probe did not start")
	}
	newBinding := bindingTestRequest(s, "POST", "/init", `{"accessToken":"new-token"}`, "")
	close(release)
	require.Equal(t, 204, newBinding.Code, newBinding.Body.String())
	select {
	case old := <-probe:
		require.Equal(t, 503, old.Code, old.Body.String())
	case <-time.After(time.Second):
		t.Fatal("old probe was not released")
	}
}

func TestCredentialInitializationIsSerialized(t *testing.T) {
	firstEntered, releaseFirst := make(chan struct{}), make(chan struct{})
	secondEntered := make(chan struct{}, 1)
	s, state := bindingTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("X-Access-Token") {
		case "":
			w.WriteHeader(401)
			return
		case "first":
			close(firstEntered)
			select {
			case <-releaseFirst:
			case <-r.Context().Done():
			}
		case "second":
			secondEntered <- struct{}{}
		}
		w.WriteHeader(200)
	}), "old-token")
	first, second := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- bindingTestRequest(s, "POST", "/init", `{"accessToken":"first"}`, "") }()
	select {
	case <-firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first initializer did not reach validation")
	}
	go func() { second <- bindingTestRequest(s, "POST", "/init", `{"accessToken":"second"}`, "") }()
	select {
	case <-secondEntered:
		t.Fatal("second initializer validated before the first completed")
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseFirst)
	for _, done := range []<-chan *httptest.ResponseRecorder{first, second} {
		select {
		case response := <-done:
			require.Equal(t, 204, response.Code, response.Body.String())
		case <-time.After(2 * time.Second):
			t.Fatal("initialization did not finish")
		}
	}
	require.EqualValues(t, 3, s.binding.generation)
	require.Equal(t, "second", s.binding.token)
	data, err := os.ReadFile(state)
	require.NoError(t, err)
	require.Equal(t, "second", string(data))
	require.Empty(t, credentialTemporaryFiles(t, filepath.Dir(state)), "temporary credential files must be removed")
}

func credentialTemporaryFiles(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	require.NoError(t, err)
	var temporary []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".token-") {
			temporary = append(temporary, entry.Name())
		}
	}
	return temporary
}
