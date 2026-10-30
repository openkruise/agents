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

package secureprovider

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/utils/runtime"
)

// ---------------------------------------------------------------------------
// resolveFilePath
// ---------------------------------------------------------------------------

func TestResolveFilePath(t *testing.T) {
	tests := []struct {
		name        string
		explicit    string
		certDir     string
		defaultName string
		expected    string
	}{
		{
			name:        "explicit path takes precedence",
			explicit:    "/custom/path/my.crt",
			certDir:     "/etc/certs",
			defaultName: "ca.crt",
			expected:    "/custom/path/my.crt",
		},
		{
			name:        "certDir with default file name",
			explicit:    "",
			certDir:     "/etc/certs",
			defaultName: "ca.crt",
			expected:    "/etc/certs/ca.crt",
		},
		{
			name:        "empty explicit and empty certDir returns empty",
			explicit:    "",
			certDir:     "",
			defaultName: "ca.crt",
			expected:    "",
		},
		{
			name:        "explicit empty string with certDir joins correctly",
			explicit:    "",
			certDir:     "/mnt/secrets",
			defaultName: "client.key",
			expected:    "/mnt/secrets/client.key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := resolveFilePath(tt.explicit, tt.certDir, tt.defaultName)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// ---------------------------------------------------------------------------
// resolveAuthToken
// ---------------------------------------------------------------------------

func TestResolveAuthToken(t *testing.T) {
	tests := []struct {
		name        string
		envValue    string
		setupDir    func(t *testing.T) string // returns tokenDir path
		expected    string
		expectError string
	}{
		{
			name:     "reads token from file when tokenDir is set and file exists",
			envValue: "env-fallback-token",
			setupDir: func(t *testing.T) string {
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, DefaultAuthTokenFileName), []byte("secret-from-file"), 0644))
				return dir
			},
			expected: "secret-from-file",
		},
		{
			name:     "trims whitespace from token file",
			envValue: "env-fallback",
			setupDir: func(t *testing.T) string {
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, DefaultAuthTokenFileName), []byte("  trimmed-token  \n"), 0644))
				return dir
			},
			expected: "trimmed-token",
		},
		{
			name:     "falls back to env value when file is empty",
			envValue: "env-token",
			setupDir: func(t *testing.T) string {
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, DefaultAuthTokenFileName), []byte(""), 0644))
				return dir
			},
			expected: "env-token",
		},
		{
			name:     "falls back to env value when file does not exist",
			envValue: "env-token",
			setupDir: func(t *testing.T) string {
				return t.TempDir() // dir exists but no token file
			},
			expected: "env-token",
		},
		{
			name:     "falls back to env value when tokenDir is empty",
			envValue: "env-token",
			setupDir: func(t *testing.T) string {
				return ""
			},
			expected: "env-token",
		},
		{
			name:     "returns empty when both file and env are empty",
			envValue: "",
			setupDir: func(t *testing.T) string {
				return ""
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokenDir := tt.setupDir(t)
			result := resolveAuthToken(tt.envValue, tokenDir)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// ---------------------------------------------------------------------------
// identityProviderOptionsFromEnv
// ---------------------------------------------------------------------------

func TestIdentityProviderOptionsFromEnv(t *testing.T) {
	tests := []struct {
		name     string
		envVars  map[string]string
		validate func(t *testing.T, opts IdentityProviderOptions)
	}{
		{
			name: "minimal config with only endpoint",
			envVars: map[string]string{
				EnvIdentityProviderEndpoint: "https://idp.example.com/token",
			},
			validate: func(t *testing.T, opts IdentityProviderOptions) {
				assert.Equal(t, "https://idp.example.com/token", opts.Endpoint)
				assert.Empty(t, opts.AuthToken)
				assert.Empty(t, opts.CAFile)
				assert.Empty(t, opts.CertFile)
				assert.Empty(t, opts.KeyFile)
				assert.Zero(t, opts.Timeout)
				assert.False(t, opts.InsecureSkipVerify)
			},
		},
		{
			name: "auth token from env var when no token dir",
			envVars: map[string]string{
				EnvIdentityProviderEndpoint:  "https://idp.example.com/token",
				EnvIdentityProviderAuthToken: "bearer-token-123",
			},
			validate: func(t *testing.T, opts IdentityProviderOptions) {
				assert.Equal(t, "bearer-token-123", opts.AuthToken)
			},
		},
		{
			name: "cert dir resolves all file paths",
			envVars: map[string]string{
				EnvIdentityProviderEndpoint: "https://idp.example.com/token",
				EnvIdentityProviderCertDir:  "/etc/certs",
			},
			validate: func(t *testing.T, opts IdentityProviderOptions) {
				assert.Equal(t, "/etc/certs/ca.crt", opts.CAFile)
				assert.Equal(t, "/etc/certs/client.crt", opts.CertFile)
				assert.Equal(t, "/etc/certs/client.key", opts.KeyFile)
			},
		},
		{
			name: "individual file env vars override cert dir",
			envVars: map[string]string{
				EnvIdentityProviderEndpoint: "https://idp.example.com/token",
				EnvIdentityProviderCertDir:  "/etc/certs",
				EnvIdentityProviderCAFile:   "/custom/ca.pem",
				EnvIdentityProviderCertFile: "/custom/client.pem",
				EnvIdentityProviderKeyFile:  "/custom/client.key",
			},
			validate: func(t *testing.T, opts IdentityProviderOptions) {
				assert.Equal(t, "/custom/ca.pem", opts.CAFile)
				assert.Equal(t, "/custom/client.pem", opts.CertFile)
				assert.Equal(t, "/custom/client.key", opts.KeyFile)
			},
		},
		{
			name: "timeout parsed correctly",
			envVars: map[string]string{
				EnvIdentityProviderEndpoint: "https://idp.example.com/token",
				EnvIdentityProviderTimeout:  "30s",
			},
			validate: func(t *testing.T, opts IdentityProviderOptions) {
				assert.Equal(t, 30*time.Second, opts.Timeout)
			},
		},
		{
			name: "invalid timeout keeps zero",
			envVars: map[string]string{
				EnvIdentityProviderEndpoint: "https://idp.example.com/token",
				EnvIdentityProviderTimeout:  "not-a-duration",
			},
			validate: func(t *testing.T, opts IdentityProviderOptions) {
				assert.Zero(t, opts.Timeout)
			},
		},
		{
			name: "insecure skip verify set to true",
			envVars: map[string]string{
				EnvIdentityProviderEndpoint:           "https://idp.example.com/token",
				EnvIdentityProviderInsecureSkipVerify: "true",
			},
			validate: func(t *testing.T, opts IdentityProviderOptions) {
				assert.True(t, opts.InsecureSkipVerify)
			},
		},
		{
			name: "insecure skip verify invalid value keeps false",
			envVars: map[string]string{
				EnvIdentityProviderEndpoint:           "https://idp.example.com/token",
				EnvIdentityProviderInsecureSkipVerify: "not-bool",
			},
			validate: func(t *testing.T, opts IdentityProviderOptions) {
				assert.False(t, opts.InsecureSkipVerify)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set all env vars for this test case.
			for k, v := range tt.envVars {
				t.Setenv(k, v)
			}
			// Clear env vars not specified to avoid leaking from test to test.
			allEnvVars := []string{
				EnvIdentityProviderEndpoint, EnvIdentityProviderAuthToken,
				EnvIdentityProviderCAFile, EnvIdentityProviderCertFile, EnvIdentityProviderKeyFile,
				EnvIdentityProviderCertDir, EnvIdentityProviderTokenDir,
				EnvIdentityProviderTimeout, EnvIdentityProviderInsecureSkipVerify,
			}
			for _, envKey := range allEnvVars {
				if _, ok := tt.envVars[envKey]; !ok {
					t.Setenv(envKey, "")
				}
			}

			opts := identityProviderOptionsFromEnv(tt.envVars[EnvIdentityProviderEndpoint])
			tt.validate(t, opts)
		})
	}
}

func TestIdentityProviderOptionsFromEnv_TokenDirPrecedence(t *testing.T) {
	// When IDENTITY_PROVIDER_TOKEN_DIR is set and file exists, it takes precedence over env var.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, DefaultAuthTokenFileName), []byte("file-token"), 0644))

	t.Setenv(EnvIdentityProviderEndpoint, "https://idp.example.com/token")
	t.Setenv(EnvIdentityProviderAuthToken, "env-token")
	t.Setenv(EnvIdentityProviderTokenDir, dir)
	t.Setenv(EnvIdentityProviderCertDir, "")

	opts := identityProviderOptionsFromEnv("https://idp.example.com/token")
	assert.Equal(t, "file-token", opts.AuthToken, "token from file should take precedence over env var")
}

// ---------------------------------------------------------------------------
// buildSecureProvider
// ---------------------------------------------------------------------------

func TestBuildSecureProvider(t *testing.T) {
	tests := []struct {
		name          string
		envVars       map[string]string
		expectFailing bool
		expectSecure  bool
	}{
		{
			name:          "no endpoint returns failingProvider",
			envVars:       map[string]string{EnvIdentityProviderEndpoint: ""},
			expectFailing: true,
		},
		{
			name: "endpoint set but invalid cert file returns failingProvider",
			envVars: map[string]string{
				EnvIdentityProviderEndpoint: "https://idp.example.com",
				EnvIdentityProviderCAFile:   "/non-existent/ca.crt",
			},
			expectFailing: true,
		},
		{
			name: "endpoint set with no TLS issues returns secureIdentityProvider",
			envVars: map[string]string{
				EnvIdentityProviderEndpoint: "https://idp.example.com",
			},
			expectSecure: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.envVars {
				t.Setenv(k, v)
			}
			// Clear unset env vars.
			clearEnvVars := []string{
				EnvIdentityProviderAuthToken, EnvIdentityProviderCAFile,
				EnvIdentityProviderCertFile, EnvIdentityProviderKeyFile,
				EnvIdentityProviderCertDir, EnvIdentityProviderTokenDir,
				EnvIdentityProviderTimeout, EnvIdentityProviderInsecureSkipVerify,
			}
			for _, k := range clearEnvVars {
				if _, ok := tt.envVars[k]; !ok {
					t.Setenv(k, "")
				}
			}

			result := buildSecureProvider()
			require.NotNil(t, result)

			if tt.expectFailing {
				fp, ok := result.(*failingProvider)
				assert.True(t, ok, "expected failingProvider")
				assert.NotEmpty(t, fp.reason)
			}
			if tt.expectSecure {
				_, ok := result.(*secureIdentityProvider)
				assert.True(t, ok, "expected secureIdentityProvider")
			}
		})
	}
}

func TestFailingProvider_IssueToken(t *testing.T) {
	fp := &failingProvider{reason: "endpoint not configured"}
	resp, err := fp.IssueToken(context.Background(), nil, identity.TokenOptions{Kind: identity.TokenKindIDToken})
	assert.Nil(t, resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "remote identity provider unavailable")
	assert.Contains(t, err.Error(), "endpoint not configured")
}

func TestFailingProvider_PropagateSecurityToken(t *testing.T) {
	fp := &failingProvider{reason: "cert read failure"}
	sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"}}
	err := fp.PropagateSecurityToken(context.Background(), sbx, &identity.TokenResponse{AccessToken: "tok"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "remote identity provider unavailable")
	assert.Contains(t, err.Error(), "cert read failure")
}

// ---------------------------------------------------------------------------
// newIdentityProviderClient: strict transport for the Secret-backed modes
// ---------------------------------------------------------------------------

func TestNewIdentityProviderClient_StrictTransport(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedPEM(t)

	tests := []struct {
		name        string
		opts        IdentityProviderOptions
		expectError string
		mustNotLeak string
	}{
		{
			name: "https with mTLS material is accepted",
			opts: IdentityProviderOptions{
				Endpoint:        "https://agent-identity-provider",
				StrictTransport: true,
				CAData:          certPEM,
				CertData:        certPEM,
				KeyData:         keyPEM,
			},
		},
		{
			name:        "http endpoint is rejected",
			opts:        IdentityProviderOptions{Endpoint: "http://agent-identity-provider", StrictTransport: true},
			expectError: "must use https",
		},
		{
			name:        "endpoint without a scheme is rejected",
			opts:        IdentityProviderOptions{Endpoint: "agent-identity-provider", StrictTransport: true},
			expectError: "must use https",
		},
		{
			// url.Parse accepts an empty host and net/http only rejects it when a request
			// is sent, so without this check a locally unusable address would pass startup
			// and fail the first business call instead.
			name:        "https endpoint without a host is rejected",
			opts:        IdentityProviderOptions{Endpoint: "https:///token", StrictTransport: true},
			expectError: "must include a host",
		},
		{
			// Host is ":443" here, not empty, so only a Hostname check catches it. Startup
			// performs a TokenRequest against the user cluster and never probes the identity
			// provider, so an undialable address would otherwise survive startup and fail
			// every later business call at dial time.
			name:        "https endpoint with a port but no hostname is rejected",
			opts:        IdentityProviderOptions{Endpoint: "https://:443/token", StrictTransport: true},
			expectError: "must include a host",
		},
		{
			name: "https endpoint with a valid explicit port is accepted",
			opts: IdentityProviderOptions{
				Endpoint:        "https://agent-identity-provider:8443",
				StrictTransport: true,
				CAData:          certPEM,
				CertData:        certPEM,
				KeyData:         keyPEM,
			},
		},
		{
			name:        "port above the valid range is rejected",
			opts:        IdentityProviderOptions{Endpoint: "https://agent-identity-provider:65536", StrictTransport: true},
			expectError: "port outside 1-65535",
		},
		{
			name:        "port zero is rejected",
			opts:        IdentityProviderOptions{Endpoint: "https://agent-identity-provider:0", StrictTransport: true},
			expectError: "port outside 1-65535",
		},
		{
			// url.Parse only guarantees the port is all digits, so a digit string long
			// enough to overflow Atoi must be rejected rather than silently truncated.
			name:        "port that overflows the numeric range is rejected",
			opts:        IdentityProviderOptions{Endpoint: "https://agent-identity-provider:99999999999999999999", StrictTransport: true},
			expectError: "port outside 1-65535",
		},
		{
			name: "skipped TLS verification is rejected",
			opts: IdentityProviderOptions{
				Endpoint:           "https://agent-identity-provider",
				StrictTransport:    true,
				InsecureSkipVerify: true,
			},
			expectError: EnvIdentityProviderInsecureSkipVerify,
		},
		{
			name:        "userinfo in the endpoint is rejected",
			opts:        IdentityProviderOptions{Endpoint: "https://user:pass@agent-identity-provider", StrictTransport: true},
			expectError: "must not embed userinfo",
		},
		{
			// url.Parse wraps a failure in a *url.Error that carries the raw endpoint, so
			// a malformed value with embedded credentials must not be echoed back into a
			// startup log.
			name:        "malformed endpoint is rejected without echoing credentials",
			opts:        IdentityProviderOptions{Endpoint: "https://user:sup3rs3cret@agent-identity-provider:notaport", StrictTransport: true},
			expectError: "identity provider endpoint is not a valid URL",
			mustNotLeak: "sup3rs3cret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := newIdentityProviderClient(tt.opts)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				if tt.mustNotLeak != "" {
					assert.NotContains(t, err.Error(), tt.mustNotLeak)
				}
				return
			}
			require.NoError(t, err)
			require.NotNil(t, client)
			tlsConfig := client.httpClient.Transport.(*http.Transport).TLSClientConfig
			assert.NotNil(t, tlsConfig.RootCAs)
			assert.Len(t, tlsConfig.Certificates, 1)
		})
	}
}

// TestLoggableEndpoint covers the sink every identity provider endpoint reaches. The
// address is operator-supplied and userinfo, query and fragment are each a place a
// credential can sit. The endpoint is logged before validateStrictTransport runs, so
// rejecting those forms cannot substitute for redacting what actually reaches the log.
func TestLoggableEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     string
	}{
		{
			name:     "credentials in userinfo, query and fragment are all dropped",
			endpoint: "https://user:sup3rs3cret@idp.example.com/token?token=qsecret#fsecret",
			want:     "https://idp.example.com/token",
		},
		{
			// A query is a legitimate part of an endpoint, so it cannot simply be
			// rejected at validation; it has to be dropped from the logged form.
			name:     "a credential carried only in the query is dropped",
			endpoint: "https://idp.example.com/token?token=qsecret",
			want:     "https://idp.example.com/token",
		},
		{
			name:     "host, port and path survive for diagnostics",
			endpoint: "https://idp.example.com:8443/token",
			want:     "https://idp.example.com:8443/token",
		},
		{
			name:     "an IPv6 host keeps its brackets",
			endpoint: "https://[::1]:8080/token",
			want:     "https://[::1]:8080/token",
		},
		{
			name:     "an unparsable endpoint is not echoed back",
			endpoint: "https://idp.example.com/%zz?token=qsecret",
			want:     "<unparsable endpoint>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, loggableEndpoint(tt.endpoint))
		})
	}
}

// TestNewIdentityProviderClient_NonStrictKeepsExistingTransport pins the controller's
// volume-mounted deployment: I1 tightens the sandbox-manager Secret modes only, so the
// non-strict path keeps following redirects and honoring its escape hatches.
func TestNewIdentityProviderClient_NonStrictKeepsExistingTransport(t *testing.T) {
	client, err := newIdentityProviderClient(IdentityProviderOptions{
		Endpoint:           "http://idp.example.com/token",
		InsecureSkipVerify: true,
	})
	require.NoError(t, err)
	assert.Nil(t, client.httpClient.CheckRedirect)
	assert.True(t, client.httpClient.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify)
}

// TestIdentityProviderClient_Invoke_RejectsRedirectWithoutLeakingTarget drives a real 3xx
// through Invoke over TLS. net/http reports a rejected redirect as a *url.Error carrying
// the raw Location, and an identity provider redirect commonly carries a credential in its
// query, so the wrapper must be dropped instead of reaching a caller that logs the error.
func TestIdentityProviderClient_Invoke_RejectsRedirectWithoutLeakingTarget(t *testing.T) {
	const redirectCredential = "REDIRECT-CREDENTIAL"

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://attacker.example/callback?access_token="+redirectCredential)
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	client, err := newIdentityProviderClient(IdentityProviderOptions{
		Endpoint:        server.URL,
		StrictTransport: true,
		CAData:          caPEM,
		AuthToken:       "test-token",
	})
	require.NoError(t, err)

	err = client.Invoke(context.Background(), ActionIssueToken, map[string]string{}, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errIdentityProviderRedirectRejected)
	assert.NotContains(t, err.Error(), redirectCredential, "a redirect target may carry a credential")
	assert.NotContains(t, err.Error(), "attacker.example", "a redirect target must not reach a log")
}

// TestIdentityProviderClient_Invoke_TransportFailureDoesNotLeakEndpointCredentials covers
// the general transport branch. net/http reports the failure as a *url.Error whose message
// embeds the full request URL and masks only a userinfo password, so a credential carried in
// the query or the fragment would otherwise reach the caller that logs the error.
func TestIdentityProviderClient_Invoke_TransportFailureDoesNotLeakEndpointCredentials(t *testing.T) {
	const endpointCredential = "ENDPOINT-CREDENTIAL"

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := server.URL + "/token?api_key=" + endpointCredential + "#" + endpointCredential
	server.Close()

	client := &identityProviderClient{
		endpoint:   endpoint,
		authToken:  "tok",
		httpClient: &http.Client{},
	}

	err := client.Invoke(context.Background(), ActionIssueToken, map[string]string{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to call identity provider")
	assert.NotContains(t, err.Error(), endpointCredential,
		"the endpoint query and fragment must not reach a log")
	// The cause is what an operator acts on, so redacting the URL must not discard it.
	assert.Contains(t, err.Error(), "connection refused")
}

// ---------------------------------------------------------------------------
// buildTLSConfig
// ---------------------------------------------------------------------------

func TestBuildTLSConfig(t *testing.T) {
	tests := []struct {
		name        string
		opts        IdentityProviderOptions
		setupFiles  func(t *testing.T) // setup temp cert files
		expectError string
	}{
		{
			name:        "empty options produces valid config",
			opts:        IdentityProviderOptions{},
			expectError: "",
		},
		{
			name:        "InsecureSkipVerify is propagated",
			opts:        IdentityProviderOptions{InsecureSkipVerify: true},
			expectError: "",
		},
		{
			name:        "non-existent CA file returns error",
			opts:        IdentityProviderOptions{CAFile: "/non-existent/ca.crt"},
			expectError: "failed to read CA file",
		},
		{
			name: "non-existent cert file returns error",
			opts: IdentityProviderOptions{
				CertFile: "/non-existent/client.crt",
				KeyFile:  "/non-existent/client.key",
			},
			expectError: "failed to load client certificate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setupFiles != nil {
				tt.setupFiles(t)
			}

			tlsConfig, err := buildTLSConfig(tt.opts)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, tlsConfig)

			if tt.opts.InsecureSkipVerify {
				assert.True(t, tlsConfig.InsecureSkipVerify)
			}
		})
	}
}

func TestBuildTLSConfig_InvalidCAPEM(t *testing.T) {
	dir := t.TempDir()
	caFile := filepath.Join(dir, "bad-ca.crt")
	require.NoError(t, os.WriteFile(caFile, []byte("not-a-valid-pem"), 0644))

	_, err := buildTLSConfig(IdentityProviderOptions{CAFile: caFile})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse CA certificate")
}

func TestBuildTLSConfig_PartialMTLS_OnlyCertFile(t *testing.T) {
	// When only CertFile is set without KeyFile, no error but warning is logged.
	opts := IdentityProviderOptions{CertFile: "/some/client.crt"}
	tlsConfig, err := buildTLSConfig(opts)
	require.NoError(t, err)
	require.NotNil(t, tlsConfig)
	assert.Empty(t, tlsConfig.Certificates, "should not load certificates when only CertFile is set")
}

func TestBuildTLSConfig_PartialMTLS_OnlyKeyFile(t *testing.T) {
	opts := IdentityProviderOptions{KeyFile: "/some/client.key"}
	tlsConfig, err := buildTLSConfig(opts)
	require.NoError(t, err)
	require.NotNil(t, tlsConfig)
	assert.Empty(t, tlsConfig.Certificates, "should not load certificates when only KeyFile is set")
}

// ---------------------------------------------------------------------------
// NewIdentityProviderTokenProvider / newIdentityProviderClient
// ---------------------------------------------------------------------------

func TestNewIdentityProviderTokenProvider(t *testing.T) {
	tests := []struct {
		name        string
		opts        IdentityProviderOptions
		expectError string
	}{
		{
			name: "valid options succeed",
			opts: IdentityProviderOptions{
				Endpoint:  "https://idp.example.com/token",
				AuthToken: "test-token",
			},
			expectError: "",
		},
		{
			name: "invalid CA file fails",
			opts: IdentityProviderOptions{
				Endpoint: "https://idp.example.com/token",
				CAFile:   "/non-existent/ca.crt",
			},
			expectError: "failed to build TLS config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, err := NewIdentityProviderTokenProvider(tt.opts)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Nil(t, provider)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, provider)
		})
	}
}

func TestNewIdentityProviderClient_DefaultTimeout(t *testing.T) {
	client, err := newIdentityProviderClient(IdentityProviderOptions{
		Endpoint: "https://idp.example.com",
	})
	require.NoError(t, err)
	require.NotNil(t, client)
	assert.Equal(t, 10*time.Second, client.httpClient.Timeout, "default timeout should be 10s")
}

func TestNewIdentityProviderClient_CustomTimeout(t *testing.T) {
	client, err := newIdentityProviderClient(IdentityProviderOptions{
		Endpoint: "https://idp.example.com",
		Timeout:  5 * time.Second,
	})
	require.NoError(t, err)
	require.NotNil(t, client)
	assert.Equal(t, 5*time.Second, client.httpClient.Timeout)
}

// ---------------------------------------------------------------------------
// identityProviderClient.Invoke
// ---------------------------------------------------------------------------

func TestIdentityProviderClient_Invoke(t *testing.T) {
	tests := []struct {
		name        string
		handler     http.HandlerFunc
		expectError string
		mustNotLeak string
	}{
		{
			name: "success response",
			handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
				assert.Equal(t, "GetIdentityToken", r.Header.Get("X-Api-Action-Name"))
				assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "issued-token"})
			},
			expectError: "",
		},
		{
			name: "non-200 returns error without the response body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte("access denied for bearer sensitive-material"))
			},
			expectError: "returned status 403",
			// Callers log this error, so an identity provider error page must not ride
			// along with it.
			mustNotLeak: "sensitive-material",
		},
		{
			name: "invalid JSON response returns error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("not-json"))
			},
			expectError: "failed to unmarshal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			client := &identityProviderClient{
				endpoint:   server.URL,
				authToken:  "test-token",
				httpClient: server.Client(),
			}

			var resp identity.TokenResponse
			err := client.Invoke(context.Background(), ActionIssueToken, identity.TokenRequest{TokenType: identity.TokenTypeAgent}, &resp)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				if tt.mustNotLeak != "" {
					assert.NotContains(t, err.Error(), tt.mustNotLeak)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "issued-token", resp.AccessToken)
		})
	}
}

func TestIdentityProviderClient_Invoke_NilRespOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"some":"data"}`))
	}))
	defer server.Close()

	client := &identityProviderClient{
		endpoint:   server.URL,
		authToken:  "test-token",
		httpClient: server.Client(),
	}

	// Pass nil respOut — should succeed without unmarshal.
	err := client.Invoke(context.Background(), "TestAction", map[string]string{"key": "val"}, nil)
	require.NoError(t, err)
}

func TestIdentityProviderClient_Invoke_NoAuthToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"), "Authorization header should be empty when no token")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "tok"})
	}))
	defer server.Close()

	client := &identityProviderClient{
		endpoint:   server.URL,
		authToken:  "", // no token
		httpClient: server.Client(),
	}

	var resp identity.TokenResponse
	err := client.Invoke(context.Background(), ActionIssueToken, identity.TokenRequest{}, &resp)
	require.NoError(t, err)
}

func TestIdentityProviderClient_Invoke_ContextCanceled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &identityProviderClient{
		endpoint:   server.URL,
		authToken:  "tok",
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	var resp identity.TokenResponse
	err := client.Invoke(ctx, ActionIssueToken, identity.TokenRequest{}, &resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to call identity provider")
	// Callers log a cancellation below the error threshold, so redacting the URL out of the
	// transport error must not break the sentinel chain that carries it.
	assert.ErrorIs(t, err, context.Canceled)
}

// ---------------------------------------------------------------------------
// identityProviderTokenProvider.IssueToken
// ---------------------------------------------------------------------------

func TestIdentityProviderTokenProvider_IssueToken(t *testing.T) {
	tests := []struct {
		name        string
		handler     http.HandlerFunc
		expectError string
	}{
		{
			name: "successful token issuance",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(identity.TokenResponse{
					RequestID:   "req-1",
					AccessToken: "issued-token-123",
				})
			},
			expectError: "",
		},
		{
			name: "empty access token returns error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(identity.TokenResponse{
					RequestID:   "req-2",
					AccessToken: "",
				})
			},
			expectError: "identity provider returned empty access token",
		},
		{
			name: "server error propagates",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("internal error"))
			},
			expectError: "returned status 500",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			provider := &identityProviderTokenProvider{
				client: &identityProviderClient{
					endpoint:   server.URL,
					authToken:  "test-token",
					httpClient: server.Client(),
				},
			}

			resp, err := provider.IssueToken(context.Background(), identity.TokenRequest{TokenType: identity.TokenTypeAgent})
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Nil(t, resp)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, resp)
			assert.Equal(t, "issued-token-123", resp.AccessToken)
			assert.Equal(t, "req-1", resp.RequestID)
		})
	}
}

// ---------------------------------------------------------------------------
// identityProviderTokenProvider.IssueTrafficAccessToken
// ---------------------------------------------------------------------------

func TestIdentityProviderTokenProvider_IssueTrafficAccessToken(t *testing.T) {
	tests := []struct {
		name        string
		handler     http.HandlerFunc
		expectError string
	}{
		{
			name: "successful issuance maps token and expiry",
			handler: func(w http.ResponseWriter, r *http.Request) {
				// The gateway access token must be routed under its own action.
				assert.Equal(t, ActionIssueGenericAccessToken, r.Header.Get("X-Api-Action-Name"))
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(trafficAccessTokenResponse{
					RequestID: "req-1",
					Token:     "jwt-abc",
					ExpiresAt: "2026-06-18T10:19:37Z",
					ExpiresIn: 3600,
				})
			},
			expectError: "",
		},
		{
			name: "empty token returns error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(trafficAccessTokenResponse{RequestID: "req-2"})
			},
			expectError: "identity provider returned empty traffic access token",
		},
		{
			name: "server error propagates",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("internal error"))
			},
			expectError: "returned status 500",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			provider := &identityProviderTokenProvider{
				client: &identityProviderClient{
					endpoint:   server.URL,
					authToken:  "test-token",
					httpClient: server.Client(),
				},
			}

			resp, err := provider.IssueTrafficAccessToken(context.Background(), trafficAccessTokenRequest{
				Subject:       "e2b:controlplane:x",
				Audience:      "sandbox-gateway",
				TokenValidity: "1h",
			})
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Nil(t, resp)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, resp)
			// The distinct response shape is mapped onto identity.TokenResponse.
			assert.Equal(t, "jwt-abc", resp.AccessToken)
			assert.Equal(t, "req-1", resp.RequestID)
			assert.Equal(t, "2026-06-18T10:19:37Z", resp.AccessTokenExpiration)
		})
	}
}

// ---------------------------------------------------------------------------
// buildAgentTokenRequest
// ---------------------------------------------------------------------------

func TestBuildAgentTokenRequest(t *testing.T) {
	const prefix = identity.SecurityMetadataPrefix
	tests := []struct {
		name             string
		sbx              *agentsv1alpha1.Sandbox
		expectNilSandbox bool
		expectSandbox    *identity.SandboxInfo
		// expectMetadata nil means req.Metadata must be nil; a non-nil map
		// (including an empty one) means req.Metadata must deep-equal it.
		expectMetadata map[string]string
	}{
		{
			name:             "nil sandbox yields bare Agent request",
			sbx:              nil,
			expectNilSandbox: true,
			expectMetadata:   nil,
		},
		{
			name: "sandbox without annotations yields empty non-nil metadata",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "ns-1", UID: "uid-1"},
			},
			expectSandbox: &identity.SandboxInfo{
				PodName:      "sbx-1",
				PodNamespace: "ns-1",
				SandboxID:    "ns-1--sbx-1",
				SandboxName:  "sbx-1",
				SandboxUID:   "uid-1",
			},
			expectMetadata: map[string]string{},
		},
		{
			name: "only security-prefixed annotations are kept",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx-2",
					Namespace: "ns-2",
					UID:       "uid-2",
					Annotations: map[string]string{
						prefix + "team":           "payments",
						prefix + "env":            "prod",
						"other.kruise.io/ignored": "nope",
						"random":                  "value",
					},
				},
			},
			expectSandbox: &identity.SandboxInfo{
				PodName:      "sbx-2",
				PodNamespace: "ns-2",
				SandboxID:    "ns-2--sbx-2",
				SandboxName:  "sbx-2",
				SandboxUID:   "uid-2",
			},
			expectMetadata: map[string]string{
				prefix + "team": "payments",
				prefix + "env":  "prod",
			},
		},
		{
			name: "non-security annotations only yields empty metadata",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "sbx-3",
					Namespace:   "ns-3",
					UID:         "uid-3",
					Annotations: map[string]string{"foo": "bar"},
				},
			},
			expectSandbox: &identity.SandboxInfo{
				PodName:      "sbx-3",
				PodNamespace: "ns-3",
				SandboxID:    "ns-3--sbx-3",
				SandboxName:  "sbx-3",
				SandboxUID:   "uid-3",
			},
			expectMetadata: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := buildAgentTokenRequest(tt.sbx)

			assert.Equal(t, identity.TokenTypeAgent, req.TokenType)

			if tt.expectNilSandbox {
				assert.Nil(t, req.Sandbox)
			} else {
				require.NotNil(t, req.Sandbox)
				assert.Equal(t, tt.expectSandbox, req.Sandbox)
			}

			if tt.expectMetadata == nil {
				assert.Nil(t, req.Metadata)
			} else {
				assert.Equal(t, tt.expectMetadata, req.Metadata)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// buildTrafficAccessTokenRequest
// ---------------------------------------------------------------------------

func TestBuildTrafficAccessTokenRequest(t *testing.T) {
	const prefix = identity.SecurityMetadataPrefix
	tests := []struct {
		name             string
		sbx              *agentsv1alpha1.Sandbox
		validity         time.Duration
		expectValidity   time.Duration
		expectNilSandbox bool
		expectSandbox    *identity.SandboxInfo
		expectSubject    string
		expectMetadata   map[string]string
	}{
		{
			name:             "nil sandbox yields policy defaults only",
			sbx:              nil,
			expectValidity:   defaultTrafficTokenValidity,
			expectNilSandbox: true,
			expectSubject:    "",
			expectMetadata:   nil,
		},
		{
			name:             "non-positive validity falls back to the identity provider default",
			sbx:              nil,
			validity:         -time.Hour,
			expectValidity:   defaultTrafficTokenValidity,
			expectNilSandbox: true,
			expectSubject:    "",
			expectMetadata:   nil,
		},
		{
			name: "sandbox projection, subject and metadata are populated",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx-1",
					Namespace: "ns-1",
					UID:       "uid-1",
					Annotations: map[string]string{
						prefix + "role": "readonly",
						"random":        "value",
					},
				},
			},
			expectValidity: defaultTrafficTokenValidity,
			expectSandbox: &identity.SandboxInfo{
				PodName:      "sbx-1",
				PodNamespace: "ns-1",
				SandboxID:    "ns-1--sbx-1",
				SandboxName:  "sbx-1",
				SandboxUID:   "uid-1",
			},
			expectSubject:  "e2b:controlplane:ns-1--sbx-1",
			expectMetadata: map[string]string{prefix + "role": "readonly"},
		},
		{
			name: "manager-resolved validity overrides the identity provider default",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx-2",
					Namespace: "ns-2",
					UID:       "uid-2",
				},
			},
			validity:       2 * time.Hour,
			expectValidity: 2 * time.Hour,
			expectSandbox: &identity.SandboxInfo{
				PodName:      "sbx-2",
				PodNamespace: "ns-2",
				SandboxID:    "ns-2--sbx-2",
				SandboxName:  "sbx-2",
				SandboxUID:   "uid-2",
			},
			expectSubject: "e2b:controlplane:ns-2--sbx-2",
			// A sandbox without security annotations yields an empty (non-nil) map.
			expectMetadata: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := buildTrafficAccessTokenRequest(tt.sbx, tt.validity)

			// Policy fields are always present, even for a nil sandbox.
			assert.Equal(t, defaultTrafficTokenAudience, req.Audience)
			assert.Equal(t, tt.expectValidity.String(), req.TokenValidity)

			if tt.expectNilSandbox {
				assert.Nil(t, req.Sandbox)
			} else {
				require.NotNil(t, req.Sandbox)
				assert.Equal(t, tt.expectSandbox, req.Sandbox)
			}
			assert.Equal(t, tt.expectSubject, req.Subject)

			if tt.expectMetadata == nil {
				assert.Nil(t, req.Metadata)
			} else {
				assert.Equal(t, tt.expectMetadata, req.Metadata)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// secureIdentityProvider
// ---------------------------------------------------------------------------

func TestSecureIdentityProvider_IssueToken(t *testing.T) {
	mockTP := &mockTokenProvider{
		resp: &identity.TokenResponse{RequestID: "r1", AccessToken: "secure-token"},
	}
	sp := newSecureIdentityProvider(mockTP)

	resp, err := sp.IssueToken(context.Background(), nil, identity.TokenOptions{Kind: identity.TokenKindIDToken})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "secure-token", resp.AccessToken)
}

func TestSecureIdentityProvider_IssueToken_Error(t *testing.T) {
	mockTP := &mockTokenProvider{
		err: fmt.Errorf("upstream failed"),
	}
	sp := newSecureIdentityProvider(mockTP)

	_, err := sp.IssueToken(context.Background(), nil, identity.TokenOptions{Kind: identity.TokenKindIDToken})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upstream failed")
}

func TestSecureIdentityProvider_IssueToken_AccessTokenKind(t *testing.T) {
	mockTP := &mockTokenProvider{
		resp:        &identity.TokenResponse{RequestID: "id-r", AccessToken: "id-token"},
		trafficResp: &identity.TokenResponse{RequestID: "traffic-r", AccessToken: "traffic-token", AccessTokenExpiration: "2026-06-18T10:19:37Z"},
	}
	sp := newSecureIdentityProvider(mockTP)

	sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "ns", UID: "uid"}}
	resp, err := sp.IssueToken(context.Background(), sbx, identity.TokenOptions{
		Kind:              identity.TokenKindAccessToken,
		RequestedValidity: 90 * time.Minute,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	// TokenKindAccessToken must route to IssueTrafficAccessToken, not IssueToken.
	assert.Equal(t, "traffic-token", resp.AccessToken)
	assert.Equal(t, "traffic-r", resp.RequestID)
	assert.Equal(t, "2026-06-18T10:19:37Z", resp.AccessTokenExpiration)
	// The manager-resolved validity must reach the identity provider request untouched.
	assert.Equal(t, (90 * time.Minute).String(), mockTP.trafficReq.TokenValidity)
}

func TestSecureIdentityProvider_IssueToken_AccessTokenKind_Error(t *testing.T) {
	mockTP := &mockTokenProvider{trafficErr: fmt.Errorf("traffic upstream failed")}
	sp := newSecureIdentityProvider(mockTP)

	_, err := sp.IssueToken(context.Background(), nil, identity.TokenOptions{Kind: identity.TokenKindAccessToken})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "traffic upstream failed")
}

func TestSecureIdentityProvider_PropagateSecurityToken(t *testing.T) {
	// gotPropOpts records how many transport options the propagator under test
	// received, which is how the table observes that the resolved transport is
	// forwarded rather than silently dropped on the way to the propagators.
	var gotPropOpts int
	tests := []struct {
		name        string
		propagators []SecurityTokenPropagator
		// rtOpts is the transport the caller resolved for the sandbox.
		rtOpts []runtime.Option
		// expectPropOpts is the number of options the propagator must observe.
		expectPropOpts int
		expectError    string
		expectAllCalls bool
	}{
		{
			name:        "no propagators returns nil",
			propagators: nil,
			expectError: "",
		},
		{
			name: "single propagator success",
			propagators: []SecurityTokenPropagator{
				func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *identity.TokenResponse,
					_ ...runtime.Option) error {
					return nil
				},
			},
			expectError: "",
		},
		{
			name: "first propagator error is returned and all propagators are called",
			propagators: []SecurityTokenPropagator{
				func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *identity.TokenResponse,
					_ ...runtime.Option) error {
					return fmt.Errorf("propagator-1 failed")
				},
				func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *identity.TokenResponse,
					_ ...runtime.Option) error {
					return nil // still called
				},
			},
			expectError:    "propagator-1 failed",
			expectAllCalls: true,
		},
		{
			name: "all errors accumulated when multiple propagators fail",
			propagators: []SecurityTokenPropagator{
				func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *identity.TokenResponse,
					_ ...runtime.Option) error {
					return fmt.Errorf("first-error")
				},
				func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *identity.TokenResponse,
					_ ...runtime.Option) error {
					return fmt.Errorf("second-error")
				},
			},
			expectError: "first-error",
		},
		{
			// A propagator that reaches the sandbox runtime can only deliver the
			// credential over the caller's transport if it actually receives it, so
			// dropping rtOpts here would silently fall back to plaintext.
			name: "resolved transport is forwarded to the propagators",
			propagators: []SecurityTokenPropagator{
				func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *identity.TokenResponse,
					rtOpts ...runtime.Option) error {
					gotPropOpts = len(rtOpts)
					return nil
				},
			},
			rtOpts:         []runtime.Option{runtime.WithRequestTimeout(time.Second), runtime.WithTLSPort(49984)},
			expectPropOpts: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Save and restore package propagators slice.
			saved := propagators
			defer func() { propagators = saved }()
			propagators = tt.propagators
			gotPropOpts = 0

			sp := newSecureIdentityProvider(&mockTokenProvider{
				resp: &identity.TokenResponse{AccessToken: "tok"},
			})

			sbx := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			}
			err := sp.PropagateSecurityToken(context.Background(), sbx, &identity.TokenResponse{AccessToken: "tok"}, tt.rtOpts...)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				if tt.name == "all errors accumulated when multiple propagators fail" {
					assert.Contains(t, err.Error(), "second-error", "joined error must contain all propagator failures")
				}
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.expectPropOpts, gotPropOpts,
				"every propagator must receive exactly the transport resolved by the caller")
		})
	}
}

func TestSecureIdentityProvider_PropagateSecurityToken_AllPropagatorsCalled(t *testing.T) {
	saved := propagators
	defer func() { propagators = saved }()

	callCount := 0
	propagators = []SecurityTokenPropagator{
		func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *identity.TokenResponse,
			_ ...runtime.Option) error {
			callCount++
			return fmt.Errorf("err-1")
		},
		func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *identity.TokenResponse,
			_ ...runtime.Option) error {
			callCount++
			return nil
		},
		func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *identity.TokenResponse,
			_ ...runtime.Option) error {
			callCount++
			return fmt.Errorf("err-3")
		},
	}

	sp := newSecureIdentityProvider(&mockTokenProvider{resp: &identity.TokenResponse{AccessToken: "tok"}})
	sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"}}

	err := sp.PropagateSecurityToken(context.Background(), sbx, &identity.TokenResponse{AccessToken: "tok"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "err-1", "joined error must contain first propagator failure")
	assert.Contains(t, err.Error(), "err-3", "joined error must contain third propagator failure")
	assert.Equal(t, 3, callCount, "all propagators should be called even if one fails")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type mockTokenProvider struct {
	resp *identity.TokenResponse
	err  error

	trafficResp *identity.TokenResponse
	trafficErr  error
	// trafficReq records the last request seen by IssueTrafficAccessToken so
	// tests can assert the issuance policy forwarded by the provider.
	trafficReq trafficAccessTokenRequest
}

func (m *mockTokenProvider) IssueToken(_ context.Context, _ identity.TokenRequest) (*identity.TokenResponse, error) {
	return m.resp, m.err
}

func (m *mockTokenProvider) IssueTrafficAccessToken(_ context.Context, req trafficAccessTokenRequest) (*identity.TokenResponse, error) {
	m.trafficReq = req
	return m.trafficResp, m.trafficErr
}
