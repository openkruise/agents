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
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/runtime/envd"
)

const executionHeader = "X-EXECD-ACCESS-TOKEN"
const executionUser = "user"

// Server owns one sandbox's execution protocol boundary and active requests.
type Server struct {
	backend   envd.Client
	stateFile string
	mu        sync.RWMutex
	token     string
	active    map[string]*activeCommand
	mux       *http.ServeMux
}

type activeCommand struct {
	cancel     context.CancelFunc
	done       chan struct{}
	cleanupErr error // Published by closing done after remote cleanup.
}

// New restores only the sandbox-local credential. Completed command state and
// background logs are intentionally not retained.
func New(backend envd.Client, stateFile string) (*Server, error) {
	if backend.HTTP == nil {
		backend.HTTP = &http.Client{}
	}
	s := &Server{backend: backend, stateFile: stateFile, active: make(map[string]*activeCommand), mux: http.NewServeMux()}
	if stateFile != "" {
		data, err := os.ReadFile(stateFile) // #nosec G304 -- Selected by process configuration, not request input.
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read execution credential: %w", err)
		}
		s.token = string(data)
	}
	s.mux.HandleFunc("POST /init", s.initialize)
	s.mux.HandleFunc("GET /ping", s.ping)
	s.mux.HandleFunc("GET /ready", s.ping)
	s.mux.HandleFunc("POST /command", s.runCommand)
	s.mux.HandleFunc("DELETE /command", s.interruptCommand)
	s.mux.HandleFunc("POST /files/upload", s.upload)
	s.mux.HandleFunc("GET /files/download", s.download)
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		for _, prefix := range []string{"/code", "/command/", "/session", "/files", "/directories", "/metrics", "/pty", "/policy", "/credential-vault"} {
			if strings.HasPrefix(r.URL.Path, prefix) {
				writeError(w, 501, "NOT_SUPPORTED", "operation is not supported by this runtime")
				return
			}
		}
		writeError(w, 404, "NOT_FOUND", "route not found")
	})
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodPost && r.URL.Path == "/init" {
		s.mux.ServeHTTP(w, r)
		return
	}
	s.mu.RLock()
	token := s.token
	s.mu.RUnlock()
	if token == "" {
		writeError(w, 503, "SERVICE_UNAVAILABLE", "runtime initialization is incomplete")
		return
	}
	if r.URL.Path != "/ping" && r.URL.Path != "/ready" && subtle.ConstantTimeCompare([]byte(r.Header.Get(executionHeader)), []byte(token)) != 1 {
		writeError(w, 401, "UNAUTHORIZED", "invalid execution credential")
		return
	}
	s.mux.ServeHTTP(w, r)
}

// Close cancels in-flight operations so shutdown can complete remote cleanup.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, command := range s.active {
		command.cancel()
	}
}

func (s *Server) initialize(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil || input.AccessToken == "" {
		writeError(w, 400, "INVALID_REQUEST_BODY", "accessToken is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	// Verify both that native authentication is enabled and that the proposed
	// credential is the one already accepted by envd. An uninitialized envd
	// accepts arbitrary tokens and must never authorize this handshake.
	status, err := s.backend.Probe(ctx, "/envs", "")
	if err != nil || status != http.StatusUnauthorized {
		writeError(w, 503, "SERVICE_UNAVAILABLE", "native runtime is not initialized")
		return
	}
	status, err = s.backend.Probe(ctx, "/envs", input.AccessToken)
	if err != nil {
		writeError(w, 502, "RUNTIME_ERROR", "native credential validation failed")
		return
	}
	if status != http.StatusOK {
		writeError(w, 401, "UNAUTHORIZED", "invalid native runtime credential")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stateFile != "" {
		if err := os.MkdirAll(filepath.Dir(s.stateFile), 0700); err != nil {
			runtimeError(w, err)
			return
		}
		f, err := os.CreateTemp(filepath.Dir(s.stateFile), ".token-*")
		if err != nil {
			runtimeError(w, err)
			return
		}
		name := f.Name()
		defer func() {
			if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				klog.FromContext(r.Context()).Error(err, "remove temporary runtime credential")
			}
		}()
		_, writeErr := f.WriteString(input.AccessToken)
		closeErr := f.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			runtimeError(w, err)
			return
		}
		if err := os.Rename(name, s.stateFile); err != nil {
			runtimeError(w, err)
			return
		}
	}
	if s.token != input.AccessToken {
		for _, command := range s.active {
			command.cancel()
		}
	}
	s.token = input.AccessToken
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) ping(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	status, err := s.backend.Probe(ctx, "/health", "")
	if err != nil || status != http.StatusNoContent && status != http.StatusOK {
		writeError(w, 503, "SERVICE_UNAVAILABLE", "native runtime is unavailable")
		return
	}
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
