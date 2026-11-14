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

// Package opensandbox adapts sandbox-side HTTP contracts to native envd calls.
// It has no dependency on control-plane APIs, Manager, or Kubernetes clients.
package opensandbox

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"connectrpc.com/connect"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/runtime/envd"
)

const executionHeader = "X-EXECD-ACCESS-TOKEN"
const executionUser = "user"

// Server owns the execution protocol boundary and sandbox-local credential binding.
type Server struct {
	backend   envd.Client
	stateFile string
	initMu    sync.Mutex
	mu        sync.RWMutex
	binding   *credentialBinding
	closed    bool
	requests  sync.WaitGroup
	mux       *http.ServeMux
}

// A binding's identity and cancellation scope never change after publication.
// The wait group tracks local forwarding, not native process identity or state.
type credentialBinding struct {
	token      string
	generation uint64
	ctx        context.Context
	cancel     context.CancelFunc
	requests   sync.WaitGroup
}

type bindingContextKey struct{}

func newCredentialBinding(token string, generation uint64) *credentialBinding {
	ctx, cancel := context.WithCancel(context.Background())
	return &credentialBinding{token: token, generation: generation, ctx: ctx, cancel: cancel}
}

// executionToken returns the immutable credential admitted for this request.
func executionToken(r *http.Request) string {
	binding, _ := r.Context().Value(bindingContextKey{}).(*credentialBinding)
	if binding == nil {
		return ""
	}
	return binding.token
}

// New restores only a candidate credential. Native authentication is checked
// for every health and business request; a saved file never implies readiness.
func New(backend envd.Client, stateFile string) (*Server, error) {
	if backend.HTTP == nil {
		backend.HTTP = &http.Client{}
	}
	s := &Server{backend: backend, stateFile: stateFile, mux: http.NewServeMux()}
	if stateFile != "" {
		data, err := os.ReadFile(stateFile) // #nosec G304 -- Selected by process configuration, not request input.
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read execution credential: %w", err)
		}
		if len(data) != 0 {
			s.binding = newCredentialBinding(string(data), 1)
		}
	}
	s.mux.HandleFunc("POST /init", s.initialize)
	s.mux.HandleFunc("GET /ping", s.ping)
	s.mux.HandleFunc("GET /ready", s.ping)
	s.mux.HandleFunc("POST /command", s.runCommand)
	s.mux.HandleFunc("DELETE /command", s.interruptCommand)
	s.mux.HandleFunc("POST /files/upload", s.upload)
	s.mux.HandleFunc("GET /files/download", s.download)
	registerUnsupportedRoutes(s.mux)
	s.mux.HandleFunc("/", routeNotFound)
	return s, nil
}

func registerUnsupportedRoutes(mux *http.ServeMux) {
	unsupported := func(w http.ResponseWriter, r *http.Request) {
		// ServeMux also matches HEAD to GET. Only the public methods below
		// receive the unsupported response.
		if r.Method == http.MethodHead {
			routeNotFound(w, r)
			return
		}
		writeError(w, http.StatusNotImplemented, "NOT_SUPPORTED", "operation is not supported by this runtime")
	}
	for _, route := range []string{
		// OpenSandbox release-1.1.0 specs/execd-api.yaml.
		"POST /code",
		"DELETE /code",
		"POST /code/context",
		"GET /code/contexts",
		"DELETE /code/contexts",
		"GET /code/contexts/{id}",
		"DELETE /code/contexts/{id}",
		"POST /session",
		"POST /session/{id}/run",
		"DELETE /session/{id}",
		"DELETE /files",
		"GET /files/info",
		"POST /files/mv",
		"POST /files/permissions",
		"GET /files/search",
		"POST /files/replace",
		"GET /directories/list",
		"POST /directories",
		"DELETE /directories",
		"GET /metrics",
		"GET /metrics/watch",
		"POST /v1/isolated/session",
		"GET /v1/isolated/sessions",
		"GET /v1/isolated/capabilities",
		"GET /v1/isolated/session/{id}",
		"DELETE /v1/isolated/session/{id}",
		"POST /v1/isolated/session/{id}/run",
		"GET /v1/isolated/session/{id}/runs/{runId}",
		"GET /v1/isolated/session/{id}/runs/{runId}/logs",
		"GET /v1/isolated/session/{id}/diff",
		"POST /v1/isolated/session/{id}/commit",
		"GET /v1/isolated/session/{id}/files/info",
		"GET /v1/isolated/session/{id}/files/download",
		"POST /v1/isolated/session/{id}/files/upload",
		"DELETE /v1/isolated/session/{id}/files",
		"POST /v1/isolated/session/{id}/files/mv",
		"POST /v1/isolated/session/{id}/files/permissions",
		"POST /v1/isolated/session/{id}/files/replace",
		"GET /v1/isolated/session/{id}/files/search",
		"GET /v1/isolated/session/{id}/directories/list",
		"POST /v1/isolated/session/{id}/directories",
		"DELETE /v1/isolated/session/{id}/directories",
		// PTY is deferred in the proposal. These paths come from the pinned
		// execd router at components/execd/pkg/web/router.go.
		"POST /pty",
		"GET /pty/{id}",
		"DELETE /pty/{id}",
		"GET /pty/{id}/ws",
		// OpenSandbox release-1.1.0 specs/egress-api.yaml.
		"GET /policy",
		"PATCH /policy",
		"DELETE /policy",
		"POST /credential-vault",
		"GET /credential-vault",
		"PATCH /credential-vault",
		"DELETE /credential-vault",
		"GET /credential-vault/credentials",
		"GET /credential-vault/credentials/{name}",
		"GET /credential-vault/bindings",
		"GET /credential-vault/bindings/{name}",
	} {
		mux.HandleFunc(route, unsupported)
	}
	// /command/status/{id} and /command/{id}/logs intersect at
	// /command/status/logs, so ServeMux cannot register them independently.
	mux.HandleFunc("GET /command/{first}/{second}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("first") == "status" || r.PathValue("second") == "logs" {
			unsupported(w, r)
			return
		}
		routeNotFound(w, r)
	})
}

func routeNotFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "NOT_FOUND", "route not found")
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodPost && r.URL.Path == "/init" {
		s.mux.ServeHTTP(w, r)
		return
	}
	s.mu.RLock()
	binding, closed := s.binding, s.closed
	s.mu.RUnlock()
	if binding == nil || closed {
		writeError(w, 503, "SERVICE_UNAVAILABLE", "runtime initialization is incomplete")
		return
	}
	health := r.Method == http.MethodGet && (r.URL.Path == "/ping" || r.URL.Path == "/ready")
	if !health && subtle.ConstantTimeCompare([]byte(r.Header.Get(executionHeader)), []byte(binding.token)) != 1 {
		writeError(w, 401, "UNAUTHORIZED", "invalid execution credential")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	stopBinding := context.AfterFunc(binding.ctx, cancel)
	defer func() { stopBinding(); cancel() }()
	if s.checkNativeCredential(ctx, binding.token) != http.StatusOK {
		writeError(w, 503, "SERVICE_UNAVAILABLE", "native runtime is not ready")
		return
	}

	// Admission and registration share a read lock so a replaced generation
	// cannot gain requests after publication starts waiting for its cleanup.
	s.mu.RLock()
	if s.closed || s.binding != binding || binding.ctx.Err() != nil || ctx.Err() != nil {
		s.mu.RUnlock()
		writeError(w, 503, "SERVICE_UNAVAILABLE", "execution credential changed")
		return
	}
	binding.requests.Add(1)
	s.requests.Add(1)
	s.mu.RUnlock()
	defer binding.requests.Done()
	defer s.requests.Done()

	// Cancellation must also wake network body reads and response writes, not
	// just the native HTTP client. Join the callback before returning the writer.
	ioDone := make(chan struct{})
	stopIO := context.AfterFunc(ctx, func() {
		defer close(ioDone)
		controller := http.NewResponseController(w)
		for _, err := range []error{controller.SetReadDeadline(time.Now()), controller.SetWriteDeadline(time.Now())} {
			if err != nil && !errors.Is(err, http.ErrNotSupported) {
				klog.FromContext(ctx).V(4).Info("interrupt local execution I/O", "error", err)
			}
		}
		if r.Body != nil {
			if err := r.Body.Close(); err != nil {
				klog.FromContext(ctx).V(4).Info("close interrupted execution body", "error", err)
			}
		}
	})
	defer func() {
		if !stopIO() {
			<-ioDone
		}
	}()
	s.mux.ServeHTTP(w, r.WithContext(context.WithValue(ctx, bindingContextKey{}, binding)))
}

// Close prevents new admission and waits only for local forwarding to stop.
func (s *Server) Close() {
	s.initMu.Lock()
	s.mu.Lock()
	s.closed = true
	binding := s.binding
	s.mu.Unlock()
	s.initMu.Unlock()
	if binding != nil {
		binding.cancel()
	}
	s.requests.Wait()
}

// checkNativeCredential shares one budget across both status-only List calls.
// A rejected candidate is distinguished for /init; business calls map all
// failed observations to not ready rather than trusting a stale local token.
func (s *Server) checkNativeCredential(ctx context.Context, token string) int {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	status, err := s.backend.ProbeList(ctx, "")
	if err != nil || status != http.StatusUnauthorized {
		return http.StatusServiceUnavailable
	}
	status, err = s.backend.ProbeList(ctx, token)
	if err != nil || ctx.Err() != nil {
		return http.StatusServiceUnavailable
	}
	if status == http.StatusUnauthorized {
		return http.StatusUnauthorized
	}
	if status != http.StatusOK {
		return http.StatusServiceUnavailable
	}
	return http.StatusOK
}

func (s *Server) initialize(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil || input.AccessToken == "" {
		writeError(w, 400, "INVALID_REQUEST_BODY", "accessToken is required")
		return
	}
	s.initMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.initMu.Unlock()
		}
	}()
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed || r.Context().Err() != nil {
		writeError(w, 503, "SERVICE_UNAVAILABLE", "execution service is shutting down")
		return
	}
	if status := s.checkNativeCredential(r.Context(), input.AccessToken); status != http.StatusOK {
		if status == http.StatusUnauthorized {
			writeError(w, 401, "UNAUTHORIZED", "invalid native runtime credential")
		} else {
			writeError(w, 503, "SERVICE_UNAVAILABLE", "native runtime is not ready")
		}
		return
	}
	if err := s.saveCredential(input.AccessToken); err != nil {
		klog.FromContext(r.Context()).Error(err, "persist execution credential")
		writeError(w, 500, "INTERNAL_ERROR", "execution credential persistence failed")
		return
	}
	s.mu.Lock()
	old := s.binding
	if old != nil && old.token == input.AccessToken {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	generation := uint64(1)
	if old != nil {
		generation = old.generation + 1
	}
	s.binding = newCredentialBinding(input.AccessToken, generation)
	s.mu.Unlock()
	s.initMu.Unlock()
	locked = false
	if old != nil {
		old.cancel()
		old.requests.Wait()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) saveCredential(token string) error {
	if s.stateFile == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.stateFile), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.stateFile), ".token-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() {
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			klog.Background().Error(err, "remove temporary runtime credential")
		}
	}()
	_, writeErr := f.WriteString(token)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return os.Rename(name, s.stateFile)
}

// ping is reached only after the shared binding and native readiness check.
func (s *Server) ping(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{code, message}); err != nil {
		klog.Background().Error(err, "write execution API error", "status", status)
	}
}

func runtimeError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	code := "RUNTIME_ERROR"
	switch connect.CodeOf(err) {
	case connect.CodeNotFound:
		status, code = 404, "FILE_NOT_FOUND"
	case connect.CodeUnauthenticated:
		status, code = 401, "UNAUTHORIZED"
	case connect.CodePermissionDenied:
		status, code = 403, "FORBIDDEN"
	case connect.CodeInvalidArgument:
		status, code = 400, "INVALID_REQUEST_BODY"
	case connect.CodeUnavailable:
		status, code = 503, "SERVICE_UNAVAILABLE"
	case connect.CodeDeadlineExceeded:
		status = 504
	}
	// Transport errors can contain credential-bearing URLs or headers. Keep
	// diagnostic context server-side and expose a stable boundary error.
	klog.Background().Error(err, "native execution operation failed", "status", status)
	writeError(w, status, code, "native execution operation failed")
}

func nativeHTTPError(status int) error {
	code := connect.CodeInternal
	switch status {
	case 400:
		code = connect.CodeInvalidArgument
	case 401:
		code = connect.CodeUnauthenticated
	case 403:
		code = connect.CodePermissionDenied
	case 404:
		code = connect.CodeNotFound
	case 504:
		code = connect.CodeDeadlineExceeded
	}
	return connect.NewError(code, fmt.Errorf("native runtime returned HTTP %d", status))
}
