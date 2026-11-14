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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/runtime/envd"
)

const executionHeader = "X-EXECD-ACCESS-TOKEN"
const executionUser = "user"

// Server owns the execution protocol boundary. It stores no credential: every
// request carries its own, so envd stays the only authentication authority and a
// sidecar restart cannot disagree about what the workload container accepts.
type Server struct {
	backend        envd.Client
	mu             sync.Mutex
	closed         bool
	requests       sync.WaitGroup
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
	mux            *http.ServeMux
}

// executionToken returns the credential the caller supplied for this request.
func executionToken(r *http.Request) string {
	return r.Header.Get(executionHeader)
}

func New(backend envd.Client) *Server {
	if backend.HTTP == nil {
		backend.HTTP = &http.Client{}
	}
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	s := &Server{backend: backend, shutdownCtx: shutdownCtx, shutdownCancel: shutdownCancel, mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /init", s.initialize)
	s.mux.HandleFunc("GET /ping", s.ping)
	s.mux.HandleFunc("POST /command", s.runCommand)
	s.mux.HandleFunc("DELETE /command", s.interruptCommand)
	s.mux.HandleFunc("POST /files/upload", s.upload)
	s.mux.HandleFunc("GET /files/download", s.download)
	registerUnsupportedRoutes(s.mux)
	s.mux.HandleFunc("/", routeNotFound)
	return s
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
	// The handshake carries its own token in the body and the SDK's default
	// health check only asks whether the sidecar is reachable, so neither needs
	// the execution header. Business routes are rejected on a missing or empty
	// value before any native call; otherwise the value is forwarded as-is and
	// envd authenticates it on the requested operation.
	health := r.Method == http.MethodGet && r.URL.Path == "/ping"
	handshake := r.Method == http.MethodPost && r.URL.Path == "/init"
	if !health && !handshake && r.Header.Get(executionHeader) == "" {
		writeError(w, 401, "UNAUTHORIZED", "missing execution credential")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	stopShutdown := context.AfterFunc(s.shutdownCtx, cancel)
	defer func() { stopShutdown(); cancel() }()

	// Registration under the lock keeps Close from draining the wait group while
	// a request is about to start forwarding.
	s.mu.Lock()
	if s.closed || ctx.Err() != nil {
		s.mu.Unlock()
		writeError(w, 503, "SERVICE_UNAVAILABLE", "execution service is shutting down")
		return
	}
	s.requests.Add(1)
	s.mu.Unlock()
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
	s.mux.ServeHTTP(w, r.WithContext(ctx))
}

// Close prevents new admission and waits only for local forwarding to stop.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.shutdownCancel()
	s.requests.Wait()
}

// initProbeTimeout bounds the handshake probe to the caller's per-attempt /init
// budget in pkg/utils/runtime, so an unreachable envd fails initialization
// instead of holding the request open.
const initProbeTimeout = time.Second

// initialize proves that envd accepts the handshake token with one read-only
// native call, then discards it. The sidecar keeps no credential of its own, so
// neither a sidecar-only restart nor an envd-only restart can leave it
// disagreeing with the workload container about the active token.
func (s *Server) initialize(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil || input.AccessToken == "" {
		writeError(w, 400, codeInvalidRequestBody, "accessToken is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), initProbeTimeout)
	defer cancel()
	status, err := s.backend.ProbeList(ctx, input.AccessToken)
	if err != nil || ctx.Err() != nil {
		writeError(w, 503, "SERVICE_UNAVAILABLE", "native runtime is not reachable")
		return
	}
	switch status {
	case http.StatusOK:
		w.WriteHeader(http.StatusNoContent)
	case http.StatusUnauthorized:
		writeError(w, 401, "UNAUTHORIZED", "invalid native runtime credential")
	default:
		writeError(w, 503, "SERVICE_UNAVAILABLE", "native runtime is not ready")
	}
}

// ping only reports that the sidecar is reachable and calls no native endpoint.
func (s *Server) ping(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

const (
	codeInvalidRequestBody  = "INVALID_REQUEST_BODY"
	codeInvalidFileMetadata = "INVALID_FILE_METADATA"
)

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
		status, code = 400, codeInvalidRequestBody
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
