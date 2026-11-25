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

package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/sandboxroute/refresh"
	"github.com/openkruise/agents/pkg/sandboxstate"
)

func TestHealthServer(t *testing.T) {
	hs := &healthServer{}

	t.Run("check serving", func(t *testing.T) {
		resp, err := hs.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{})
		require.NoError(t, err)
		assert.Equal(t, grpc_health_v1.HealthCheckResponse_SERVING, resp.Status)
	})

	t.Run("list contains ext-proc", func(t *testing.T) {
		resp, err := hs.List(context.Background(), &grpc_health_v1.HealthListRequest{})
		require.NoError(t, err)
		require.Contains(t, resp.Statuses, "envoy-ext-proc")
		assert.Equal(t, grpc_health_v1.HealthCheckResponse_SERVING, resp.Statuses["envoy-ext-proc"].Status)
	})

	t.Run("watch unimplemented", func(t *testing.T) {
		err := hs.Watch(&grpc_health_v1.HealthCheckRequest{}, nil)
		require.Error(t, err)
		st, ok := status.FromError(err)
		require.True(t, ok)
		assert.Equal(t, codes.Unimplemented, st.Code())
	})
}

func TestNewServeMuxRefresh(t *testing.T) {
	valid := sandboxroute.Route{
		ID:              "short-a",
		Namespace:       "ns",
		Name:            "a",
		UID:             types.UID("uid-a"),
		ResourceVersion: "1",
		State:           sandboxstate.Paused,
		IP:              "10.0.0.1",
	}
	invalid := valid
	invalid.Name = ""

	tests := []struct {
		name         string
		route        sandboxroute.Route
		replay       bool
		presetGauge  float64
		expectStatus int
		expectStored bool
		expectGauge  float64
	}{
		{
			name:         "applied refresh writes store and updates the gauge",
			route:        valid,
			expectStatus: http.StatusNoContent,
			expectStored: true,
			expectGauge:  1,
		},
		{
			name:         "invalid refresh does not touch the gauge",
			route:        invalid,
			presetGauge:  7,
			expectStatus: http.StatusBadRequest,
			expectGauge:  7,
		},
		{
			name:         "same RV replay returns 204 and refreshes the gauge",
			route:        valid,
			replay:       true,
			presetGauge:  -1,
			expectStatus: http.StatusNoContent,
			expectStored: true,
			expectGauge:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := NewServer(config.SandboxManagerOptions{})
			mux := server.newServeMux()
			body, err := json.Marshal(tt.route)
			require.NoError(t, err)

			if tt.replay {
				first := httptest.NewRecorder()
				mux.ServeHTTP(first, httptest.NewRequest(http.MethodPost, refresh.Path, bytes.NewReader(body)))
				require.Equal(t, http.StatusNoContent, first.Code)
			}
			routeCount.Set(tt.presetGauge)

			request := httptest.NewRequest(http.MethodPost, refresh.Path, bytes.NewReader(body))
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)

			assert.Equal(t, tt.expectStatus, response.Code)
			stored, present := server.LoadRoute(tt.route.ID)
			assert.Equal(t, tt.expectStored, present)
			if tt.expectStored {
				assert.Equal(t, tt.route, stored)
			}
			assert.Equal(t, tt.expectGauge, testutil.ToFloat64(routeCount))
		})
	}
}

func TestRefreshReceiverScope(t *testing.T) {
	tests := []struct {
		name       string
		namespace  string
		selector   string
		labels     map[string]string
		delete     bool
		wantStored bool
		wantStatus int
	}{
		{name: "same namespace", namespace: "ns", wantStored: true},
		{name: "other namespace is ignored", namespace: "other"},
		{name: "matching label", selector: "app=sandbox", labels: map[string]string{"app": "sandbox"}, wantStored: true},
		{name: "different label is ignored", selector: "app=sandbox", labels: map[string]string{"app": "other"}},
		{name: "unknown legacy labels are ignored", selector: "!excluded"},
		{name: "known empty labels match negative selector", selector: "!excluded", labels: map[string]string{}, wantStored: true},
		{name: "excluded delete preserves local record", namespace: "other", delete: true, wantStored: true},
		{name: "out-of-scope label delete preserves local record", selector: "app=sandbox", labels: map[string]string{"app": "other"}, delete: true, wantStored: true},
		{name: "in-scope delete removes local record", selector: "app=sandbox", labels: map[string]string{"app": "sandbox"}, delete: true},
		{name: "invalid selector fails closed", selector: "app in (", wantStatus: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := NewServer(config.SandboxManagerOptions{SandboxNamespace: tt.namespace, SandboxLabelSelector: tt.selector})
			route := testProxyRoute("sb", "10.0.0.1", "2")
			route.State, route.Labels = sandboxstate.Running, tt.labels
			if tt.delete {
				previous := route
				previous.ResourceVersion = "1"
				server.SetRoute(previous)
				route.State = sandboxstate.Dead
			}
			routeCount.Set(7)
			body, err := json.Marshal(route)
			require.NoError(t, err)
			response := httptest.NewRecorder()
			server.newServeMux().ServeHTTP(response, httptest.NewRequest(http.MethodPost, refresh.Path, bytes.NewReader(body)))
			wantStatus := tt.wantStatus
			if wantStatus == 0 {
				wantStatus = http.StatusNoContent
			}
			assert.Equal(t, wantStatus, response.Code)
			_, ok := server.LoadRoute(route.ID)
			assert.Equal(t, tt.wantStored, ok)
			if (!tt.delete && !tt.wantStored) || (tt.delete && tt.wantStored) {
				assert.Equal(t, float64(7), testutil.ToFloat64(routeCount), "rejection must not invoke mutation callback")
			}
		})
	}
}

func TestRunWrapsPeerTLSListener(t *testing.T) {
	server := NewServer(config.SandboxManagerOptions{DisableEnvoyExtProc: true})
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	server.SetPeerServerTLS(tlsConfig)
	server.SetPeerOutbound(nil)
	require.NotNil(t, server.peerOutbound, "nil TLS must keep a configured plaintext client")
	server.SetPeerOutbound(&tls.Config{MinVersion: tls.VersionTLS12})
	t.Cleanup(func() { server.Stop(context.Background()) })

	require.NoError(t, server.Run())
	require.Same(t, tlsConfig, server.peerServerTLS)
	require.NotNil(t, server.peerOutbound)

	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(refresh.DefaultPort)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	assert.NoError(t, conn.SetDeadline(time.Now().Add(time.Second)))
	tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	err = tlsConn.Handshake()
	require.Error(t, err, "listener must be TLS-wrapped even without a certificate")
}
