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
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
)

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

	var (
		ipv6Port       int
		ipv6SkipReason string
	)
	ipv6Listener, ipv6Err := net.Listen("tcp6", "[::1]:0")
	if ipv6Err != nil {
		ipv6SkipReason = "IPv6 loopback is unavailable: " + ipv6Err.Error()
	} else {
		ipv6Server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		ipv6Server.Listener = ipv6Listener
		ipv6Server.Start()
		defer ipv6Server.Close()

		_, ipv6PortStr, splitErr := net.SplitHostPort(ipv6Server.Listener.Addr().String())
		require.NoError(t, splitErr)
		ipv6Port, err = strconv.Atoi(ipv6PortStr)
		require.NoError(t, err)
	}

	tests := []struct {
		name       string
		sandbox    *v1alpha1.Sandbox
		port       int
		skipReason string
		wantErr    bool
	}{
		{
			name: "running sandbox",
			port: port,
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
			name:       "running sandbox with IPv6 PodIP",
			port:       ipv6Port,
			skipReason: ipv6SkipReason,
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "running-ipv6-sandbox",
					Namespace: "default",
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					PodInfo: v1alpha1.PodInfo{
						PodIP: "::1",
					},
				},
			},
		},
		{
			name: "paused sandbox",
			port: port,
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
			if tt.skipReason != "" {
				t.Skip(tt.skipReason)
			}
			_, err := requestSandbox(t.Context(), tt.sandbox, "GET", "/", tt.port, nil)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
