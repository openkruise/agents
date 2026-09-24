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

package runtimeprobe

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProbeEnvd(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()

	tests := []struct {
		name    string
		address string
		timeout time.Duration
		prepare func(t *testing.T)
		wantErr string
	}{
		{name: "accepting listener", address: address, timeout: time.Second},
		{name: "empty address", timeout: time.Second, wantErr: "address must not be empty"},
		{name: "non-positive timeout", address: address, wantErr: "timeout must be positive"},
		{
			name:    "closed listener",
			address: address,
			timeout: time.Second,
			prepare: func(t *testing.T) {
				if err := listener.Close(); err != nil {
					t.Fatalf("close listener: %v", err)
				}
			},
			wantErr: "connect to envd",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.prepare != nil {
				tt.prepare(t)
			}
			err := ProbeEnvd(context.Background(), tt.address, tt.timeout)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ProbeEnvd() unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ProbeEnvd() error = %v, want marker %q", err, tt.wantErr)
			}
		})
	}
}

func TestProbeHelper(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		redirect   bool
		timeout    time.Duration
		wantErr    string
	}{
		{name: "serving", statusCode: http.StatusOK, body: `{"status":"SERVING"}`, timeout: time.Second},
		{name: "non-200 status", statusCode: http.StatusServiceUnavailable, body: `{"status":"SERVING"}`, timeout: time.Second, wantErr: "status 503"},
		{name: "redirect is not followed", statusCode: http.StatusOK, body: `{"status":"SERVING"}`, redirect: true, timeout: time.Second, wantErr: "status 302"},
		{name: "invalid JSON", statusCode: http.StatusOK, body: `{`, timeout: time.Second, wantErr: "decode helper health response"},
		{name: "trailing JSON is rejected", statusCode: http.StatusOK, body: `{"status":"SERVING"}{}`, timeout: time.Second, wantErr: "decode helper health response"},
		{name: "oversized response", statusCode: http.StatusOK, body: `{"status":"SERVING","padding":"` + strings.Repeat("x", maxHelperHealthBodyBytes) + `"}`, timeout: time.Second, wantErr: "response exceeds"},
		{name: "non-serving status", statusCode: http.StatusOK, body: `{"status":"NOT_SERVING"}`, timeout: time.Second, wantErr: `unhealthy status "NOT_SERVING"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			socketDir, err := os.MkdirTemp("/tmp", "runtime-probe-")
			if err != nil {
				t.Fatalf("create socket directory: %v", err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(socketDir); err != nil {
					t.Errorf("remove socket directory: %v", err)
				}
			})
			socketPath := filepath.Join(socketDir, "helper.sock")
			listener, err := net.Listen("unix", socketPath)
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.redirect && r.URL.Path == helperHealthPath {
					http.Redirect(w, r, "/ready", http.StatusFound)
					return
				}
				w.WriteHeader(tt.statusCode)
				_, _ = fmt.Fprint(w, tt.body)
			})}
			serveDone := make(chan error, 1)
			go func() { serveDone <- server.Serve(listener) }()
			t.Cleanup(func() {
				if err := server.Close(); err != nil {
					t.Errorf("close server: %v", err)
				}
				if err := <-serveDone; err != nil && err != http.ErrServerClosed {
					t.Errorf("serve: %v", err)
				}
			})

			err = ProbeHelper(context.Background(), socketPath, tt.timeout)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ProbeHelper() unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ProbeHelper() error = %v, want marker %q", err, tt.wantErr)
			}
		})
	}
}

func TestProbeHelperRejectsInvalidArguments(t *testing.T) {
	tests := []struct {
		name       string
		socketPath string
		timeout    time.Duration
		wantErr    string
	}{
		{name: "empty socket", timeout: time.Second, wantErr: "socket path must not be empty"},
		{name: "non-positive timeout", socketPath: "/tmp/helper.sock", wantErr: "timeout must be positive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ProbeHelper(context.Background(), tt.socketPath, tt.timeout)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ProbeHelper() error = %v, want marker %q", err, tt.wantErr)
			}
		})
	}
}
