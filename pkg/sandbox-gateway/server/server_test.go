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

package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/sandboxroute/refresh"
)

func TestServeMuxRoutes(t *testing.T) {
	server, routeRegistry := newTestGatewayServer()
	routeRegistry.SetReady(true)
	mux := server.newServeMux(false)

	tests := []struct {
		name         string
		path         string
		method       string
		expectStatus int
	}{
		{name: "health route", path: HealthAPI, method: http.MethodGet, expectStatus: http.StatusOK},
		{name: "health method rejected", path: HealthAPI, method: http.MethodPost, expectStatus: http.StatusMethodNotAllowed},
		{name: "readiness route", path: ReadyAPI, method: http.MethodGet, expectStatus: http.StatusOK},
		{name: "readiness method rejected", path: ReadyAPI, method: http.MethodPost, expectStatus: http.StatusMethodNotAllowed},
		{name: "refresh route rejects GET", path: refresh.Path, method: http.MethodGet, expectStatus: http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(tt.method, tt.path, nil)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			assert.Equal(t, tt.expectStatus, response.Code)
		})
	}
}

func TestReadiness(t *testing.T) {
	extraErr := errors.New("initializing")
	tests := []struct {
		name               string
		registryReady      bool
		readinessErrors    []error
		includeNilCheck    bool
		expectCalls        int
		expectReadinessErr error
		expectStatus       int
	}{
		{
			name:               "extra check fails before registry check",
			readinessErrors:    []error{extraErr},
			expectCalls:        1,
			expectReadinessErr: extraErr,
			expectStatus:       http.StatusServiceUnavailable,
		},
		{
			name:               "registry not ready after extra check succeeds",
			readinessErrors:    []error{nil},
			expectCalls:        1,
			expectReadinessErr: registry.ErrNotReady,
			expectStatus:       http.StatusServiceUnavailable,
		},
		{
			name:            "all checks ready",
			registryReady:   true,
			readinessErrors: []error{nil, nil},
			expectCalls:     2,
			expectStatus:    http.StatusOK,
		},
		{
			name:            "nil extra check is ignored",
			registryReady:   true,
			readinessErrors: []error{nil},
			includeNilCheck: true,
			expectCalls:     1,
			expectStatus:    http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routeRegistry := registry.NewRegistry()
			routeRegistry.SetReady(tt.registryReady)
			calls := 0
			checks := make([]ReadinessCheck, 0, len(tt.readinessErrors)+1)
			if tt.includeNilCheck {
				checks = append(checks, nil)
			}
			for _, readinessErr := range tt.readinessErrors {
				checkErr := readinessErr
				checks = append(checks, func() error {
					calls++
					return checkErr
				})
			}

			server := NewServer(nil, routeRegistry, 0, checks...)
			assert.ErrorIs(t, server.readinessCheck(), tt.expectReadinessErr)
			calls = 0

			request := httptest.NewRequest(http.MethodGet, ReadyAPI, nil)
			response := httptest.NewRecorder()
			server.handleReady(response, request)
			assert.Equal(t, tt.expectStatus, response.Code)
			assert.Equal(t, tt.expectCalls, calls)
		})
	}
}

func TestRefreshWritesInjectedRegistry(t *testing.T) {
	server, routeRegistry := newTestGatewayServer()
	route := sandboxroute.Route{
		ID:              "short-a",
		Namespace:       "ns",
		Name:            "a",
		UID:             types.UID("uid-a"),
		ResourceVersion: "1",
		State:           v1alpha1.SandboxStatePaused,
		IP:              "10.0.0.1",
	}
	body, err := json.Marshal(route)
	require.NoError(t, err)

	request := httptest.NewRequest(http.MethodPost, refresh.Path, bytes.NewReader(body))
	response := httptest.NewRecorder()
	server.newServeMux(false).ServeHTTP(response, request)

	assert.Equal(t, http.StatusNoContent, response.Code)
	stored, present := routeRegistry.Get(route.ID)
	require.True(t, present)
	assert.Equal(t, route, stored)
}

func TestGetMemberlistBindPort(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		want     int
	}{
		{name: "default when unset", want: config.DefaultMemberlistBindPort},
		{name: "valid port", envValue: "8080", want: 8080},
		{name: "invalid port", envValue: "invalid", want: config.DefaultMemberlistBindPort},
		{name: "negative port", envValue: "-1", want: config.DefaultMemberlistBindPort},
		{name: "zero port", envValue: "0", want: config.DefaultMemberlistBindPort},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvMemberlistBindPort, tt.envValue)
			assert.Equal(t, tt.want, getMemberlistBindPort())
		})
	}
}

func TestNewServer(t *testing.T) {
	tests := []struct {
		name         string
		port         int
		envPort      string
		wantPort     int
		wantBindPort int
	}{
		{name: "custom port", port: 9090, wantPort: 9090, wantBindPort: config.DefaultMemberlistBindPort},
		{name: "zero uses default", wantPort: refresh.DefaultPort, wantBindPort: config.DefaultMemberlistBindPort},
		{name: "negative uses default", port: -1, wantPort: refresh.DefaultPort, wantBindPort: config.DefaultMemberlistBindPort},
		{name: "custom memberlist port", port: 8080, envPort: "9000", wantPort: 8080, wantBindPort: 9000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvMemberlistBindPort, tt.envPort)
			routeRegistry := registry.NewRegistry()
			server := NewServer(nil, routeRegistry, tt.port)
			assert.Equal(t, tt.wantPort, server.port)
			assert.Equal(t, tt.wantBindPort, server.memberlistBindPort)
			assert.Nil(t, server.client)
			assert.Same(t, routeRegistry, server.registry)
		})
	}
}

func TestServerStopWithoutStart(t *testing.T) {
	server, _ := newTestGatewayServer()
	assert.NoError(t, server.Stop(nil))
}

func TestStartWithoutNodeNameFailsAndStopCleansUp(t *testing.T) {
	t.Setenv("HOSTNAME", "")
	t.Setenv("POD_NAME", "")
	server, _ := newTestGatewayServer()

	err := server.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HOSTNAME or POD_NAME")
	require.NotNil(t, server.httpServer, "Start must prepare the HTTP server before env validation")
	assert.NoError(t, server.Stop(context.Background()))
}

func TestRefreshRequiresVerifiedClientCert(t *testing.T) {
	server, routeRegistry := newTestGatewayServer()
	routeRegistry.SetReady(true)
	mux := server.newServeMux(true)

	tests := []struct {
		name         string
		tls          *tls.ConnectionState
		expectStatus int
	}{
		{name: "missing tls state", expectStatus: http.StatusForbidden},
		{name: "no verified chains", tls: &tls.ConnectionState{}, expectStatus: http.StatusForbidden},
		{
			name:         "verified client cert",
			tls:          &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{}}},
			expectStatus: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, refresh.Path, bytes.NewReader([]byte("{}")))
			request.TLS = tt.tls
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			assert.Equal(t, tt.expectStatus, response.Code)
		})
	}

	for _, path := range []string{HealthAPI, ReadyAPI} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		assert.Equal(t, http.StatusOK, response.Code, path)
	}
}

func TestStartListenFailureIsSynchronous(t *testing.T) {
	occupied, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = occupied.Close() })
	port := occupied.Addr().(*net.TCPAddr).Port

	t.Setenv("HOSTNAME", "gw-test")
	t.Setenv("POD_IP", "127.0.0.1")
	server := NewServer(nil, registry.NewRegistry(), port)
	err = server.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to listen")
}

func newTestGatewayServer(checks ...ReadinessCheck) (*Server, *registry.Registry) {
	routeRegistry := registry.NewRegistry()
	return NewServer(nil, routeRegistry, 0, checks...), routeRegistry
}

func TestPeerSecurityFromEnv(t *testing.T) {
	tests := []struct {
		name            string
		env             map[string]string
		wantErr         string
		wantKeySecret   types.NamespacedName
		wantCertDataKey string
		wantKeyDataKey  string
	}{
		{name: "empty is plaintext", wantCertDataKey: "client.crt", wantKeyDataKey: "client.key"},
		{
			name:            "key secret",
			env:             map[string]string{envPeerKeySecret: "sandbox-system/peer-key-secret"},
			wantKeySecret:   types.NamespacedName{Namespace: "sandbox-system", Name: "peer-key-secret"},
			wantCertDataKey: "client.crt",
			wantKeyDataKey:  "client.key",
		},
		{
			name:            "explicit client data keys win",
			env:             map[string]string{envPeerTLSClientCertKey: "tls.crt", envPeerTLSClientKeyKey: "tls.key"},
			wantCertDataKey: "tls.crt",
			wantKeyDataKey:  "tls.key",
		},
		{
			name:    "one TLS ref",
			env:     map[string]string{envPeerTLSServerSecret: "ns/server"},
			wantErr: "invalid peer security environment variables: peer TLS requires both server and client",
		},
		{
			name:    "invalid ref names the variable",
			env:     map[string]string{envPeerKeySecret: "not-a-ref"},
			wantErr: "invalid " + envPeerKeySecret + ": secret reference",
		},
		{
			name:    "invalid TLS ref names the variable",
			env:     map[string]string{envPeerTLSServerSecret: "not-a-ref"},
			wantErr: "invalid " + envPeerTLSServerSecret + ": secret reference",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, key := range []string{
				envPeerKeySecret, envPeerKeySecretKey,
				envPeerTLSServerSecret, envPeerTLSClientSecret,
				envPeerTLSServerCAKey, envPeerTLSServerCertKey, envPeerTLSServerKeyKey,
				envPeerTLSClientCAKey, envPeerTLSClientCertKey, envPeerTLSClientKeyKey,
			} {
				t.Setenv(key, "")
			}
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			in, err := peerSecurityFromEnv()
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantKeySecret, in.PeerKeySecret)
			assert.Equal(t, "key", in.PeerKeyDataKey)
			assert.Equal(t, tt.wantCertDataKey, in.ClientCertDataKey)
			assert.Equal(t, tt.wantKeyDataKey, in.ClientKeyDataKey)
		})
	}
}
