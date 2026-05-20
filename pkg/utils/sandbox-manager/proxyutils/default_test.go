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

package proxyutils

import (
	"encoding/json"
	"net"
	"net/url"
	"strconv"
	"testing"

	"github.com/openkruise/agents/pkg/utils/sandboxutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
)

func TestGetRouteFromSandbox(t *testing.T) {
	tests := []struct {
		name          string
		sandbox       *v1alpha1.Sandbox
		expectedRoute proxy.Route
	}{
		{
			name: "running sandbox with ip",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "running-sandbox",
					Namespace: "default",
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
					PodInfo: v1alpha1.PodInfo{
						PodIP: "10.0.0.2",
					},
				},
			},
			expectedRoute: proxy.Route{
				Namespace: "default",
				IP:        "10.0.0.2",
				ID:        "default--running-sandbox",
				Owner:     "",
				State:     v1alpha1.SandboxStateRunning,
			},
		},
		{
			name: "running sandbox without ip",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "running-sandbox",
					Namespace: "default",
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
					PodInfo: v1alpha1.PodInfo{},
				},
			},
			expectedRoute: proxy.Route{
				Namespace: "default",
				IP:        "",
				ID:        "default--running-sandbox",
				Owner:     "",
				State:     v1alpha1.SandboxStateCreating,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route := sandboxutils.GetRouteFromSandbox(tt.sandbox)
			assert.Equal(t, tt.expectedRoute, route)
		})
	}
}

func TestGetRouteFromSandbox_IncludesNamespace(t *testing.T) {
	tests := []struct {
		name              string
		sandbox           *v1alpha1.Sandbox
		expectedNamespace string
	}{
		{
			name: "running sandbox includes namespace in marshalled route",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx-1",
					Namespace: "team--blue",
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
					PodInfo: v1alpha1.PodInfo{
						PodIP: "10.0.0.9",
					},
				},
			},
			expectedNamespace: "team--blue",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route := sandboxutils.GetRouteFromSandbox(tt.sandbox)

			payload, err := json.Marshal(route)
			require.NoError(t, err)
			var got map[string]any
			require.NoError(t, json.Unmarshal(payload, &got))
			assert.Equal(t, tt.expectedNamespace, got["namespace"])
		})
	}
}

//goland:noinspection DuplicatedCode
func TestRequestSandbox(t *testing.T) {
	// Create test servers using httptest

	testServer := NewTestServer()
	defer testServer.Close()

	// Parse testServer.URL to get IP and port
	parsedURL, err := url.Parse(testServer.URL)
	require.NoError(t, err)
	host, portStr, err := net.SplitHostPort(parsedURL.Host)
	require.NoError(t, err)
	port, _ := strconv.Atoi(portStr)

	tests := []struct {
		name    string
		sandbox *v1alpha1.Sandbox
		wantErr bool
	}{
		{
			name: "running sandbox",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "running-sandbox",
					Namespace: "default",
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
					PodInfo: v1alpha1.PodInfo{
						PodIP: host,
					},
				},
			},
		},
		{
			name: "paused sandbox",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "running-sandbox",
					Namespace: "default",
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
					PodInfo: v1alpha1.PodInfo{
						PodIP: testServer.URL,
					},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := requestSandbox(t.Context(), tt.sandbox, "GET", "/", port, nil)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
