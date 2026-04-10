package adapters

import (
	"context"
	"testing"

	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var adminKey = "admin-key"

func SetUpE2BAdapter(t *testing.T) *E2BAdapter {
	client := clients.NewFakeClientSet(t)
	_, err := client.K8sClient.CoreV1().Secrets("default").Create(context.Background(), &corev1.Secret{
		func SetUpE2BAdapter(t *testing.T) proxy.RequestAdapter {
		scheme := runtime.NewScheme()
		_ = clientgoscheme.AddToScheme(scheme)
		secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
		Name:      keys.KeySecretName,
		Namespace: "default",
		},
		Data: map[string][]byte{},
		}
		fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
		keyStore := &keys.SecretKeyStorage{
		Namespace: "default",
		AdminKey:  adminKey,
		Client:    fc,
		APIReader: fc,
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
		t.Run(tt.name, func (t *testing.T) {
		adapter := SetUpE2BAdapter(t)
		sandboxID, sandboxPort, headers, err := adapter.Map(&ParsedRequest{
		Scheme:    "http",
		Authority: tt.authority,
		Path:      tt.path,
		Port:      8080,
		Headers:   tt.headers,
		})
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
		t.Run(tt.name, func (t *testing.T) {
		adapter := SetUpE2BAdapter(t)
		isSandbox := adapter.IsSandboxRequest(tt.authority, tt.path, 8080)
		assert.Equal(t, tt.expectIsSandboxRequest, isSandbox)
		})
		}
		}

		func TestParseRequest(t *testing.T) {
		adapter := NewE2BAdapter(8080)

		tests := []struct {
		name            string
		headers         map[string]string
		expectScheme    string
		expectAuthority string
		expectPath      string
		expectPort      int
		}{
		{
		name: "full pseudo-headers with port in authority",
		headers: map[string]string{
		":scheme":    "http",
		":authority": "localhost:9002",
		":path":      "/sandbox",
		},
		expectScheme:    "http",
		expectAuthority: "localhost:9002",
		expectPath:      "/sandbox",
		expectPort:      9002,
		},
		{
		name: "authority without port - http defaults to 80",
		headers: map[string]string{
		":scheme":    "http",
		":authority": "example.com",
		":path":      "/",
		},
		expectScheme:    "http",
		expectAuthority: "example.com",
		expectPath:      "/",
		expectPort:      80,
		},
		{
		name: "authority without port - https defaults to 443",
		headers: map[string]string{
		":scheme":    "https",
		":authority": "example.com",
		":path":      "/",
		},
		expectScheme:    "https",
		expectAuthority: "example.com",
		expectPath:      "/",
		expectPort:      443,
		},
		{
		name: "wss defaults to 443",
		headers: map[string]string{
		":scheme":    "wss",
		":authority": "example.com",
		":path":      "/ws",
		},
		expectScheme:    "wss",
		expectAuthority: "example.com",
		expectPath:      "/ws",
		expectPort:      443,
		},
		{
		name: "ws defaults to 80",
		headers: map[string]string{
		":scheme":    "ws",
		":authority": "example.com",
		":path":      "/ws",
		},
		expectScheme:    "ws",
		expectAuthority: "example.com",
		expectPath:      "/ws",
		expectPort:      80,
		},
		{
		name: "fallback to host when :authority is absent",
		headers: map[string]string{
		":scheme": "http",
		"host":    "fallback-host.com:3000",
		":path":   "/test",
		},
		expectScheme:    "http",
		expectAuthority: "fallback-host.com:3000",
		expectPath:      "/test",
		expectPort:      3000,
		},
		{
		name:            "empty headers",
		headers:         map[string]string{},
		expectScheme:    "",
		expectAuthority: "",
		expectPath:      "",
		expectPort:      0,
		},
		{
		name: ":authority takes precedence over host",
		headers: map[string]string{
		":scheme":    "http",
		":authority": "authority-host.com:8080",
		"host":       "host-header.com:9090",
		":path":      "/",
		},
		expectScheme:    "http",
		expectAuthority: "authority-host.com:8080",
		expectPath:      "/",
		expectPort:      8080,
		},
		{
		name: "authority with non-numeric port part",
		headers: map[string]string{
		":scheme":    "http",
		":authority": "host:abc",
		":path":      "/",
		},
		expectScheme:    "http",
		expectAuthority: "host:abc",
		expectPath:      "/",
		expectPort:      0,
		},
		{
		name: "headers map is preserved in ParsedRequest",
		headers: map[string]string{
		":scheme":        "http",
		":authority":     "example.com:8080",
		":path":          "/test",
		"x-request-id":   "abc-123",
		"e2b-sandbox-id": "my-sandbox",
		},
		expectScheme:    "http",
		expectAuthority: "example.com:8080",
		expectPath:      "/test",
		expectPort:      8080,
		},
		}

		for _, tt := range tests {
		t.Run(tt.name, func (t *testing.T) {
		parsed := adapter.ParseRequest(tt.headers)
		assert.Equal(t, tt.expectScheme, parsed.Scheme)
		assert.Equal(t, tt.expectAuthority, parsed.Authority)
		assert.Equal(t, tt.expectPath, parsed.Path)
		assert.Equal(t, tt.expectPort, parsed.Port)
		assert.Equal(t, tt.headers, parsed.Headers)
		})
		}
		}

		func TestNativeE2BAdapterHeaderBasedMap(t *testing.T) {
		tests := []struct {
		name      string
		adapter   *NativeE2BAdapter
		authority string
		headers   map[string]string

		expectErr         bool
		expectSandboxID   string
		expectSandboxPort int
		}{
		{
		name:              "header-only: sandbox ID and port from headers",
		adapter:           &NativeE2BAdapter{},
		authority:         "some-non-matching-host",
		headers:           map[string]string{DefaultSandboxIDHeader: "ns--sandbox1", DefaultSandboxPortHeader: "8080"},
		expectSandboxID:   "ns--sandbox1",
		expectSandboxPort: 8080,
		},
		{
		name:              "header with default port: sandbox ID from header, no port header",
		adapter:           &NativeE2BAdapter{DefaultPort: 49983},
		authority:         "some-non-matching-host",
		headers:           map[string]string{DefaultSandboxIDHeader: "ns--sandbox1"},
		expectSandboxID:   "ns--sandbox1",
		expectSandboxPort: 49983,
		},
		{
		name:      "header without port and no default port: returns error",
		adapter:   &NativeE2BAdapter{},
		authority: "some-non-matching-host",
		headers:   map[string]string{DefaultSandboxIDHeader: "ns--sandbox1"},
		expectErr: true,
		},
		{
		name:              "header takes priority: both sandbox ID header and valid hostname present",
		adapter:           &NativeE2BAdapter{DefaultPort: 49983},
		authority:         "3000-host--sandbox.example.com",
		headers:           map[string]string{DefaultSandboxIDHeader: "ns--header-sandbox", DefaultSandboxPortHeader: "9090"},
		expectSandboxID:   "ns--header-sandbox",
		expectSandboxPort: 9090,
		},
		{
		name:              "hostname fallback: no sandbox ID header, falls back to hostname parsing",
		adapter:           &NativeE2BAdapter{},
		authority:         "3000-ns--sandbox1.example.com",
		headers:           map[string]string{},
		expectSandboxID:   "ns--sandbox1",
		expectSandboxPort: 3000,
		},
		{
		name:              "hostname fallback with nil headers",
		adapter:           &NativeE2BAdapter{},
		authority:         "3000-ns--sandbox1.example.com",
		headers:           nil,
		expectSandboxID:   "ns--sandbox1",
		expectSandboxPort: 3000,
		},
		{
		name:              "custom host header: reads hostname from custom header",
		adapter:           &NativeE2BAdapter{HostHeader: "x-custom-host"},
		authority:         "some-api-gateway.internal",
		headers:           map[string]string{"x-custom-host": "8080-ns--sandbox2.example.com"},
		expectSandboxID:   "ns--sandbox2",
		expectSandboxPort: 8080,
		},
		{
		name:      "custom host header: custom header not present, authority doesn't match",
		adapter:   &NativeE2BAdapter{HostHeader: "x-custom-host"},
		authority: "some-api-gateway.internal",
		headers:   map[string]string{},
		expectErr: true,
		},
		{
		name:      "neither hostname nor header: returns error",
		adapter:   &NativeE2BAdapter{},
		authority: "invalid-authority",
		headers:   map[string]string{},
		expectErr: true,
		},
		{
		name:      "invalid port in sandbox port header",
		adapter:   &NativeE2BAdapter{},
		authority: "invalid-authority",
		headers:   map[string]string{DefaultSandboxIDHeader: "ns--sandbox1", DefaultSandboxPortHeader: "not-a-number"},
		expectErr: true,
		},
		{
		name:              "custom header names: configurable sandbox ID and port headers",
		adapter:           &NativeE2BAdapter{SandboxIDHeader: "x-sandbox-id", SandboxPortHeader: "x-sandbox-port"},
		authority:         "invalid-authority",
		headers:           map[string]string{"x-sandbox-id": "ns--sandbox1", "x-sandbox-port": "7777"},
		expectSandboxID:   "ns--sandbox1",
		expectSandboxPort: 7777,
		},
		}
		for _, tt := range tests {
		t.Run(tt.name, func (t *testing.T) {
		sandboxID, sandboxPort, _, err := tt.adapter.Map(&ParsedRequest{
		Scheme:    "http",
		Authority: tt.authority,
		Path:      "/",
		Port:      0,
		Headers:   tt.headers,
		})
		if tt.expectErr {
		assert.Error(t, err)
		return
		}
		assert.NoError(t, err)
		assert.Equal(t, tt.expectSandboxID, sandboxID)
		assert.Equal(t, tt.expectSandboxPort, sandboxPort)
		})
		}
		}
