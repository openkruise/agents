package proxyutils

import (
	"net"
	"net/url"
	"strconv"
	"testing"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
			expectedRoute: proxy.Route{
				IP:    "",
				ID:    "default--running-sandbox",
				Owner: "",
				State: v1alpha1.SandboxStateCreating,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route := getRouteFromSandbox(tt.sandbox)
			assert.Equal(t, tt.expectedRoute, route)
		})
	}
}

//goland:noinspection DuplicatedCode
func TestRequestSandbox(t *testing.T) {
	// Create test servers using httptest

	testServer := NewTestServer()
	defer testServer.Close()

	// 解析 testServer.URL 来获取 IP 和端口
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
