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
	"net"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
)

func TestGetRouteFromSandbox(t *testing.T) {
	tests := []struct {
		name          string
		sandbox       *v1alpha1.Sandbox
		expectedRoute Route
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
			expectedRoute: Route{
				IP:    "10.0.0.2",
				ID:    "default--running-sandbox",
				Owner: "",
				State: v1alpha1.SandboxStateRunning,
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
			expectedRoute: Route{
				IP:    "",
				ID:    "default--running-sandbox",
				Owner: "",
				State: v1alpha1.SandboxStateCreating,
			},
		},
		{
			name: "sandbox with runtime access token annotation",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "token-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						v1alpha1.AnnotationRuntimeAccessToken: "secret-token-123",
					},
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
						PodIP: "10.0.0.3",
					},
				},
			},
			expectedRoute: Route{
				IP:          "10.0.0.3",
				ID:          "default--token-sandbox",
				Owner:       "",
				State:       v1alpha1.SandboxStateRunning,
				AccessToken: "secret-token-123",
			},
		},
		{
			name: "sandbox requiring traffic authentication",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "jwt-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						annotationEnableJWTAuth: v1alpha1.True,
					},
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue},
					},
					PodInfo: v1alpha1.PodInfo{PodIP: "10.0.0.4"},
				},
			},
			expectedRoute: Route{
				IP:                 "10.0.0.4",
				ID:                 "default--jwt-sandbox",
				State:              v1alpha1.SandboxStateRunning,
				RequireTrafficAuth: true,
			},
		},
		{
			name: "traffic authentication annotation requires exact true value",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "invalid-jwt-annotation-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						annotationEnableJWTAuth: "True",
					},
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue},
					},
					PodInfo: v1alpha1.PodInfo{PodIP: "10.0.0.5"},
				},
			},
			expectedRoute: Route{
				IP:    "10.0.0.5",
				ID:    "default--invalid-jwt-annotation-sandbox",
				State: v1alpha1.SandboxStateRunning,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route := GetRouteFromSandbox(tt.sandbox)
			assert.Equal(t, tt.expectedRoute, route)
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
