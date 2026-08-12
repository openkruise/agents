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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/sandboxroute/refresh"
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
		State:           v1alpha1.SandboxStatePaused,
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
