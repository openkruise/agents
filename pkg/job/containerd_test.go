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

package job

import (
	"testing"
)

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		name         string
		endpoint     string
		wantProtocol string
		wantAddr     string
		wantErr      bool
	}{
		{
			name:         "unix socket with scheme",
			endpoint:     "unix:///run/containerd/containerd.sock",
			wantProtocol: "unix",
			wantAddr:     "/run/containerd/containerd.sock",
		},
		{
			name:         "tcp endpoint",
			endpoint:     "tcp://127.0.0.1:2376",
			wantProtocol: "tcp",
			wantAddr:     "127.0.0.1:2376",
		},
		{
			name:    "empty scheme",
			endpoint: "",
			wantErr: true,
		},
		{
			name:    "unsupported scheme",
			endpoint: "http://localhost:8080",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			protocol, addr, err := parseEndpoint(tt.endpoint)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if protocol != tt.wantProtocol {
				t.Errorf("protocol = %s, want %s", protocol, tt.wantProtocol)
			}
			if addr != tt.wantAddr {
				t.Errorf("addr = %s, want %s", addr, tt.wantAddr)
			}
		})
	}
}

func TestGetAddressAndDialer(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantAddr string
		wantErr  bool
	}{
		{
			name:     "unix socket with scheme",
			endpoint: "unix:///var/run/containerd/containerd.sock",
			wantAddr: "/var/run/containerd/containerd.sock",
		},
		{
			name:     "bare path (fallback to unix://)",
			endpoint: "/run/containerd/containerd.sock",
			wantAddr: "/run/containerd/containerd.sock",
		},
		{
			name:    "tcp rejected",
			endpoint: "tcp://127.0.0.1:2376",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, dialer, err := getAddressAndDialer(tt.endpoint)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if addr != tt.wantAddr {
				t.Errorf("addr = %s, want %s", addr, tt.wantAddr)
			}
			if dialer == nil {
				t.Error("expected non-nil dialer")
			}
		})
	}
}
