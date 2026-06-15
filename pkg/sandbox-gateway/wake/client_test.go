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

package wake

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConnectClient_Connect(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		expectStatus int
	}{
		{name: "ok", status: http.StatusOK, expectStatus: http.StatusOK},
		{name: "created", status: http.StatusCreated, expectStatus: http.StatusCreated},
		{name: "conflict", status: http.StatusConflict, expectStatus: http.StatusConflict},
		{name: "bad request", status: http.StatusBadRequest, expectStatus: http.StatusBadRequest},
		{name: "unauthorized", status: http.StatusUnauthorized, expectStatus: http.StatusUnauthorized},
		{name: "forbidden", status: http.StatusForbidden, expectStatus: http.StatusForbidden},
		{name: "not found", status: http.StatusNotFound, expectStatus: http.StatusNotFound},
		{name: "server error", status: http.StatusInternalServerError, expectStatus: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath, gotKey, gotHost string
			var gotBody struct {
				Timeout int `json:"timeout"`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotKey = r.Header.Get(apiKeyHeader)
				gotHost = r.Host
				require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
				w.WriteHeader(tt.status)
			}))
			defer server.Close()
			client, err := NewConnectClient(server.URL, "system-key")
			require.NoError(t, err)

			status, err := client.Connect(context.Background(), "default--sandbox", 300)

			require.NoError(t, err)
			assert.Equal(t, tt.expectStatus, status)
			assert.Equal(t, "/sandboxes/default--sandbox/connect", gotPath)
			assert.Equal(t, "system-key", gotKey)
			assert.Equal(t, server.Listener.Addr().String(), gotHost)
			assert.Equal(t, 300, gotBody.Timeout)
		})
	}
}

func TestConnectClient_UsesDefaultHTTPClient(t *testing.T) {
	client, err := NewConnectClient("http://example.com", "key")
	require.NoError(t, err)
	assert.Same(t, http.DefaultClient, client.httpClient)
}

func TestConnectClient_TransportError(t *testing.T) {
	client, err := NewConnectClient("http://127.0.0.1:1", "key")
	require.NoError(t, err)

	status, err := client.Connect(context.Background(), "sandbox", 1)

	assert.Zero(t, status)
	assert.Error(t, err)
}

func TestNewConnectClient_InvalidManagerURL(t *testing.T) {
	tests := []struct {
		name       string
		managerURL string
	}{
		{name: "missing host", managerURL: "http://"},
		{name: "relative", managerURL: "/manager"},
		{name: "malformed escape", managerURL: "http://%zz"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewConnectClient(tt.managerURL, "key")
			assert.Error(t, err)
		})
	}
}

func TestConnectClient_BuildRequestError(t *testing.T) {
	client, err := NewConnectClient("http://example.com", "key")
	require.NoError(t, err)
	client.baseURL.Scheme = ":"

	status, err := client.Connect(context.Background(), "sandbox", 1)

	assert.Zero(t, status)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "build connect request")
}
