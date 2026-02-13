package adapters

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
)

var adminKey = "admin-key"

func SetUpE2BAdapter(t *testing.T) proxy.RequestAdapter {
	client := clients.NewFakeClientSet()
	_, err := client.K8sClient.CoreV1().Secrets("default").Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: keys.KeySecretName,
		},
		Data: map[string][]byte{},
	}, metav1.CreateOptions{})
	assert.NoError(t, err)
	keyStore := &keys.SecretKeyStorage{
		Namespace: "default",
		AdminKey:  adminKey,
		Client:    client.K8sClient,
		Stop:      make(chan struct{}),
	}
	assert.NoError(t, keyStore.Init(context.Background()))
	return NewE2BAdapter(8080)
}

func TestMap(t *testing.T) {
	tests := []struct {
		name      string
		authority string
		path      string
		headers   map[string]string

		expectErr         bool
		expectSandboxID   string
		expectSandboxPort int
		expectHeaders     map[string]string
	}{
		{
			name:              "native e2b adapter - valid authority with CDP port",
			authority:         "9222-sandbox1234.example.com",
			path:              "/",
			headers:           map[string]string{},
			expectSandboxID:   "sandbox1234",
			expectSandboxPort: 9222,
			expectErr:         false,
		},
		{
			name:              "native e2b adapter - valid authority with regular port and valid token",
			authority:         "3000-sandbox5678.example.com",
			path:              "/",
			headers:           map[string]string{"x-access-token": adminKey},
			expectSandboxID:   "sandbox5678",
			expectSandboxPort: 3000,
			expectErr:         false,
		},
		{
			name:              "native e2b adapter - valid authority with regular port and invalid token",
			authority:         "3000-sandbox5678.example.com",
			path:              "/",
			headers:           map[string]string{"x-access-token": "invalid-token"},
			expectSandboxID:   "sandbox5678",
			expectSandboxPort: 3000,
			expectErr:         false,
		},
		{
			name:      "native e2b adapter - invalid authority format",
			authority: "invalid-authority",
			path:      "/",
			headers:   map[string]string{},
			expectErr: true,
		},
		{
			name:              "customized e2b adapter - valid path with CDP port",
			authority:         "",
			path:              "/kruise/sandbox1234/9222/some/path",
			headers:           map[string]string{},
			expectSandboxID:   "sandbox1234",
			expectSandboxPort: 9222,
			expectHeaders:     map[string]string{":path": "/some/path"},
			expectErr:         false,
		},
		{
			name:              "customized e2b adapter - valid path with regular port and valid token",
			authority:         "",
			path:              "/kruise/sandbox1234/3000/some/path",
			headers:           map[string]string{"x-access-token": adminKey},
			expectSandboxID:   "sandbox1234",
			expectSandboxPort: 3000,
			expectHeaders:     map[string]string{":path": "/some/path"},
			expectErr:         false,
		},
		{
			name:              "customized e2b adapter - valid path with regular port and invalid token",
			authority:         "",
			path:              "/kruise/sandbox1234/3000/some/path",
			headers:           map[string]string{"x-access-token": "invalid-token"},
			expectSandboxID:   "sandbox1234",
			expectSandboxPort: 3000,
			expectHeaders:     map[string]string{":path": "/some/path"},
			expectErr:         false,
		},
		{
			name:      "customized e2b adapter - invalid path (too short)",
			authority: "",
			path:      "/kruise/",
			headers:   map[string]string{},
			expectErr: true,
		},
		{
			name:      "customized e2b adapter - invalid path (missing components)",
			authority: "",
			path:      "/kruise/sandbox1234",
			headers:   map[string]string{},
			expectErr: true,
		},
		{
			name:      "customized e2b adapter - invalid port number",
			authority: "",
			path:      "/kruise/sandbox1234/invalid-port/some/path",
			headers:   map[string]string{},
			expectErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter := SetUpE2BAdapter(t)
			sandboxID, sandboxPort, headers, err := adapter.Map("http", tt.authority, tt.path, 8080, tt.headers)
			if tt.expectErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			if err == nil {
				assert.Equal(t, tt.expectSandboxID, sandboxID)
				assert.Equal(t, tt.expectSandboxPort, sandboxPort)
				if tt.expectHeaders == nil {
					assert.Nil(t, headers)
				} else {
					assert.NotNil(t, headers)
					assert.Equal(t, len(tt.expectHeaders), len(headers))
					for k, v := range tt.expectHeaders {
						assert.Equal(t, v, headers[k])
					}
				}
			}
		})
	}
}

func TestIsSandboxRequest(t *testing.T) {
	tests := []struct {
		name      string
		authority string
		path      string

		expectIsSandboxRequest bool
	}{
		// Native E2B Adapter Tests
		{
			name:                   "native e2b adapter - regular sandbox request",
			authority:              "3000-sandbox1234.example.com",
			path:                   "/",
			expectIsSandboxRequest: true,
		},
		{
			name:                   "native e2b adapter - api request",
			authority:              "api.example.com",
			path:                   "/",
			expectIsSandboxRequest: false,
		},
		{
			name:                   "native e2b adapter - api subdomain request",
			authority:              "api.something.example.com",
			path:                   "/",
			expectIsSandboxRequest: false,
		},

		// Customized E2B Adapter Tests
		{
			name:                   "customized e2b adapter - regular sandbox request",
			authority:              "",
			path:                   "/kruise/sandbox1234/3000/some/path",
			expectIsSandboxRequest: true,
		},
		{
			name:                   "customized e2b adapter - api request",
			authority:              "",
			path:                   "/kruise/api/something",
			expectIsSandboxRequest: false,
		},
		{
			name:                   "customized e2b adapter - api-like path but not under /kruise/api",
			authority:              "",
			path:                   "/kruise/apiserver/something",
			expectIsSandboxRequest: false,
		},
		{
			name:                   "customized e2b adapter - non-api request under kruise prefix",
			authority:              "",
			path:                   "/kruise/some/other/path",
			expectIsSandboxRequest: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter := SetUpE2BAdapter(t)
			isSandbox := adapter.IsSandboxRequest(tt.authority, tt.path, 8080)
			assert.Equal(t, tt.expectIsSandboxRequest, isSandbox)
		})
	}
}
