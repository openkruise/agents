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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/utils"
)

// generateSelfSignedPEM produces a self-signed certificate and its EC private key in
// PEM form. The certificate is a CA so it can double as both the RootCAs entry and the
// client certificate in mTLS byte-path tests.
func generateSelfSignedPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "identity-provider-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalECPrivateKey(priv)
	require.NoError(t, err)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// ---------------------------------------------------------------------------
// buildTLSConfig: in-memory byte material (sandbox-manager credential source)
// ---------------------------------------------------------------------------

func TestBuildTLSConfig_FromBytes(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedPEM(t)

	tests := []struct {
		name        string
		opts        IdentityProviderOptions
		expectError string
		expectCert  bool // client certificate loaded
		expectCA    bool // custom RootCAs set
	}{
		{
			name:       "valid CAData sets RootCAs",
			opts:       IdentityProviderOptions{CAData: certPEM},
			expectCA:   true,
			expectCert: false,
		},
		{
			name:       "valid CertData and KeyData load client certificate",
			opts:       IdentityProviderOptions{CertData: certPEM, KeyData: keyPEM},
			expectCert: true,
		},
		{
			name:        "invalid CAData returns parse error",
			opts:        IdentityProviderOptions{CAData: []byte("not-a-pem")},
			expectError: "failed to parse CA certificate",
		},
		{
			name:        "invalid CertData and KeyData returns load error",
			opts:        IdentityProviderOptions{CertData: []byte("bad-cert"), KeyData: []byte("bad-key")},
			expectError: "failed to load client certificate (in-memory)",
		},
		{
			name:       "only CertData without KeyData skips mTLS without error",
			opts:       IdentityProviderOptions{CertData: certPEM},
			expectCert: false,
		},
		{
			name:       "CAData takes precedence over CAFile",
			opts:       IdentityProviderOptions{CAData: certPEM, CAFile: "/non-existent/ca.crt"},
			expectCA:   true,
			expectCert: false,
		},
		{
			name:       "CertData and KeyData take precedence over CertFile and KeyFile",
			opts:       IdentityProviderOptions{CertData: certPEM, KeyData: keyPEM, CertFile: "/non-existent/c.crt", KeyFile: "/non-existent/c.key"},
			expectCert: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tlsConfig, err := buildTLSConfig(tt.opts)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, tlsConfig)
			if tt.expectCA {
				assert.NotNil(t, tlsConfig.RootCAs)
			} else {
				assert.Nil(t, tlsConfig.RootCAs)
			}
			if tt.expectCert {
				assert.Len(t, tlsConfig.Certificates, 1)
			} else {
				assert.Empty(t, tlsConfig.Certificates)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// identityProviderClient.Invoke: per-request token resolution via tokenFunc
// ---------------------------------------------------------------------------

func TestIdentityProviderClient_Invoke_TokenFunc(t *testing.T) {
	t.Run("token resolved per request from tokenFunc, ignoring static authToken", func(t *testing.T) {
		var gotAuth string
		var calls int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"ok": "1"})
		}))
		defer server.Close()

		client := &identityProviderClient{
			endpoint:   server.URL,
			authToken:  "static-should-be-ignored",
			httpClient: server.Client(),
			tokenFunc: func() (string, error) {
				calls++
				return fmt.Sprintf("dynamic-%d", calls), nil
			},
		}

		var out map[string]string
		require.NoError(t, client.Invoke(context.Background(), ActionIssueToken, map[string]string{}, &out))
		assert.Equal(t, "Bearer dynamic-1", gotAuth)

		require.NoError(t, client.Invoke(context.Background(), ActionIssueToken, map[string]string{}, &out))
		assert.Equal(t, "Bearer dynamic-2", gotAuth, "token should be re-read on every request")
	})

	t.Run("token resolution failures do not make a request", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()
		client := &identityProviderClient{endpoint: server.URL, httpClient: server.Client()}

		tests := []struct {
			name        string
			tokenError  error
			expectError string
		}{
			{name: "token file missing", tokenError: fmt.Errorf("token file missing"), expectError: "failed to resolve identity provider auth token"},
			{name: "empty resolved token", expectError: "resolved to an empty value"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				requests.Store(0)
				client.tokenFunc = func() (string, error) { return "", tt.tokenError }
				err := client.Invoke(context.Background(), ActionIssueToken, map[string]string{}, nil)
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				if tt.tokenError != nil {
					assert.Contains(t, err.Error(), tt.tokenError.Error())
				}
				assert.Zero(t, requests.Load(), "no request may leave the process without a credential")
			})
		}
	})
}

// ---------------------------------------------------------------------------
// InstallSecretBackedProviderFromEnv: enablement guard
// ---------------------------------------------------------------------------

func TestInstallSecretBackedProviderFromEnv_Guard(t *testing.T) {
	tests := []struct {
		name                string
		source              string
		endpoint            string
		tokenServiceAccount string
		expectError         string
	}{
		{name: "empty source is a no-op even with a TokenRequest ServiceAccount", source: "", endpoint: "https://agent-identity-provider", tokenServiceAccount: "manager", expectError: ""},
		{name: "FILE source is a no-op even with a TokenRequest ServiceAccount", source: "FILE", endpoint: "https://agent-identity-provider", tokenServiceAccount: "manager"},
		{name: "unknown source returns error", source: "vault", expectError: "unknown IDENTITY_PROVIDER_CREDENTIAL_SOURCE"},
		{name: "secret source without endpoint returns error", source: "secret", endpoint: "", expectError: "IDENTITY_PROVIDER_ENDPOINT not set"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvIdentityProviderCredentialSource, tt.source)
			t.Setenv(EnvIdentityProviderEndpoint, tt.endpoint)
			t.Setenv(EnvIdentityProviderTokenServiceAccount, tt.tokenServiceAccount)
			forbidUserClusterClient(t, tt.source)

			err := InstallSecretBackedProviderFromEnv(context.Background(), &rest.Config{Host: "https://user-cluster.example.com"})
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
		})
	}
}

// forbidUserClusterClient fails the test if the user cluster client is built, which no
// disabled or rejected credential source may do.
func forbidUserClusterClient(t *testing.T, source string) {
	t.Helper()
	old := userClusterClientBuilder
	userClusterClientBuilder = func(*rest.Config) (kubernetes.Interface, error) {
		t.Fatalf("the user cluster client must not be built for credential source %q", source)
		return nil, nil
	}
	t.Cleanup(func() { userClusterClientBuilder = old })
}

// ---------------------------------------------------------------------------
// fetchClientCertSecret / secretBackedOptionsFromEnv
// ---------------------------------------------------------------------------

func newCertSecret(name, namespace string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       data,
	}
}

func TestFetchClientCertSecret(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedPEM(t)

	t.Run("reads material from default secret", func(t *testing.T) {
		clientset := kubernetesfake.NewSimpleClientset(newCertSecret(DefaultClientCertSecretName, DefaultClientCertSecretNamespace, map[string][]byte{
			SecretDataKeyCACert:     certPEM,
			SecretDataKeyClientCert: certPEM,
			SecretDataKeyClientKey:  keyPEM,
		}))

		ca, cert, key, err := fetchClientCertSecret(context.Background(), clientset)
		require.NoError(t, err)
		assert.Equal(t, certPEM, ca)
		assert.Equal(t, certPEM, cert)
		assert.Equal(t, keyPEM, key)
	})

	t.Run("honors secret name and namespace overrides", func(t *testing.T) {
		t.Setenv(EnvIdentityProviderClientCertSecret, "custom-cert")
		t.Setenv(EnvIdentityProviderClientCertNamespace, "custom-ns")
		clientset := kubernetesfake.NewSimpleClientset(newCertSecret("custom-cert", "custom-ns", map[string][]byte{
			SecretDataKeyClientCert: certPEM,
			SecretDataKeyClientKey:  keyPEM,
		}))

		ca, cert, key, err := fetchClientCertSecret(context.Background(), clientset)
		require.NoError(t, err)
		assert.Empty(t, ca, "ca.crt is optional")
		assert.Equal(t, certPEM, cert)
		assert.Equal(t, keyPEM, key)
	})

	t.Run("missing client cert data key returns error", func(t *testing.T) {
		clientset := kubernetesfake.NewSimpleClientset(newCertSecret(DefaultClientCertSecretName, DefaultClientCertSecretNamespace, map[string][]byte{
			SecretDataKeyClientKey: keyPEM,
		}))

		_, _, _, err := fetchClientCertSecret(context.Background(), clientset)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing")
	})

	t.Run("secret not found returns error", func(t *testing.T) {
		clientset := kubernetesfake.NewSimpleClientset() // no objects seeded
		_, _, _, err := fetchClientCertSecret(context.Background(), clientset)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to get identity provider client certificate secret")
	})
}

func TestSecretBackedOptionsFromEnv(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedPEM(t)

	t.Run("projected token mode", func(t *testing.T) {
		clientset := kubernetesfake.NewSimpleClientset(newCertSecret(DefaultClientCertSecretName, DefaultClientCertSecretNamespace, map[string][]byte{
			SecretDataKeyCACert:     certPEM,
			SecretDataKeyClientCert: certPEM,
			SecretDataKeyClientKey:  keyPEM,
		}))

		tokenDir := t.TempDir()
		tokenFile := filepath.Join(tokenDir, "token")
		require.NoError(t, os.WriteFile(tokenFile, []byte("  projected-token\n"), 0600))
		t.Setenv(EnvIdentityProviderTokenFile, tokenFile)
		t.Setenv(EnvIdentityProviderTimeout, "7s")
		t.Setenv(EnvIdentityProviderTokenServiceAccount, "")

		opts, tokenSource, err := secretBackedOptionsFromEnv(context.Background(), clientset, "https://idp.example.com/token")
		require.NoError(t, err)
		assert.Nil(t, tokenSource, "no TokenRequest source exists while the ServiceAccount is unset")

		assert.Equal(t, "https://idp.example.com/token", opts.Endpoint)
		assert.Equal(t, certPEM, opts.CAData)
		assert.Equal(t, certPEM, opts.CertData)
		assert.Equal(t, keyPEM, opts.KeyData)
		assert.Equal(t, 7*time.Second, opts.Timeout)
		assert.True(t, opts.StrictTransport, "both Secret modes refuse a weaker transport")
		require.NotNil(t, opts.TokenFunc)

		token, err := opts.TokenFunc()
		require.NoError(t, err)
		assert.Equal(t, "projected-token", token, "token should be trimmed and read fresh")

		require.NoError(t, os.WriteFile(tokenFile, []byte("rotated-token\n"), 0600))
		token, err = opts.TokenFunc()
		require.NoError(t, err)
		assert.Equal(t, "rotated-token", token, "each call must observe projected token rotation")
	})

	t.Run("token request mode", func(t *testing.T) {
		clientset := kubernetesfake.NewSimpleClientset(newCertSecret(DefaultClientCertSecretName, DefaultClientCertSecretNamespace, map[string][]byte{
			SecretDataKeyClientCert: certPEM,
			SecretDataKeyClientKey:  keyPEM,
		}))
		t.Setenv(EnvIdentityProviderTokenServiceAccount, "  sandbox-manager  ")

		opts, tokenSource, err := secretBackedOptionsFromEnv(context.Background(), clientset, "https://idp.example.com/token")
		require.NoError(t, err)
		require.NotNil(t, tokenSource)
		assert.Equal(t, "sandbox-manager", tokenSource.saName, "the name is trimmed and holds no namespace")
		assert.Same(t, clientset, tokenSource.clientset, "the TokenRequest reuses the same user cluster client")
		assert.True(t, opts.StrictTransport)
		require.NotNil(t, opts.TokenFunc)

		_, err = opts.TokenFunc()
		require.Error(t, err, "the credential is only usable after start has published one")
	})

	for _, tt := range []struct {
		name        string
		contents    string
		writeFile   bool
		expectError string
	}{
		{name: "empty", contents: "   \n", writeFile: true, expectError: "is empty"},
		{name: "missing", expectError: "failed to read identity provider token file"},
	} {
		t.Run(tt.name+" projected token file is a credential error", func(t *testing.T) {
			clientset := kubernetesfake.NewSimpleClientset(newCertSecret(DefaultClientCertSecretName, DefaultClientCertSecretNamespace, map[string][]byte{
				SecretDataKeyClientCert: certPEM,
				SecretDataKeyClientKey:  keyPEM,
			}))
			tokenFile := filepath.Join(t.TempDir(), "token")
			if tt.writeFile {
				require.NoError(t, os.WriteFile(tokenFile, []byte(tt.contents), 0600))
			}
			t.Setenv(EnvIdentityProviderTokenFile, tokenFile)
			t.Setenv(EnvIdentityProviderTokenServiceAccount, "")

			// Startup performs no probe of the projected file; the business call that needs
			// the credential reports an empty or unreadable projection.
			opts, tokenSource, err := secretBackedOptionsFromEnv(context.Background(), clientset, "https://idp.example.com/token")
			require.NoError(t, err)
			assert.Nil(t, tokenSource)
			require.NotNil(t, opts.TokenFunc)

			_, err = opts.TokenFunc()
			require.Error(t, err, "a bad projection must not fall through to an anonymous call")
			assert.Contains(t, err.Error(), tt.expectError)
		})
	}
}

// TestSecretBackedOptionsFromEnv_StrictTransportFailsBeforeCertRead pins the startup
// ordering: a purely local transport misconfiguration is rejected before the client
// certificate Secret Get, so startup fails without any user cluster request. No Secret is
// seeded, so a regressed order would surface as a Secret read error instead.
func TestSecretBackedOptionsFromEnv_StrictTransportFailsBeforeCertRead(t *testing.T) {
	tests := []struct {
		name        string
		endpoint    string
		skipVerify  string
		expectError string
	}{
		{
			name:        "http endpoint is rejected",
			endpoint:    "http://agent-identity-provider",
			expectError: "must use https",
		},
		{
			name:        "skipped TLS verification is rejected",
			endpoint:    "https://agent-identity-provider",
			skipVerify:  "true",
			expectError: EnvIdentityProviderInsecureSkipVerify,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := kubernetesfake.NewSimpleClientset()
			t.Setenv(EnvIdentityProviderInsecureSkipVerify, tt.skipVerify)

			_, _, err := secretBackedOptionsFromEnv(context.Background(), clientset, tt.endpoint)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
			assert.Empty(t, clientset.Actions(),
				"a local transport misconfiguration must fail before any user cluster request")
		})
	}
}

// TestCredentialReadsDoNotLogBodies exercises both credential reads over the real
// client-go request path against a local API server, with the incoming context carrying a
// verbose logger. client-go logs request and response bodies at V(8) through that logger,
// so this fails if either read stops routing its context through quietContext: a Secret
// Get body holds the client private key and a TokenRequest body holds the issued token.
func TestCredentialReadsDoNotLogBodies(t *testing.T) {
	const keyMaterial = "CLIENT-PRIVATE-KEY-MATERIAL"
	const issuedToken = "ISSUED-SERVICE-ACCOUNT-TOKEN"
	t.Setenv(EnvIdentityProviderClientCertSecret, DefaultClientCertSecretName)
	t.Setenv(EnvIdentityProviderClientCertNamespace, DefaultClientCertSecretNamespace)

	writeJSON := func(w http.ResponseWriter, obj runtime.Object) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(obj))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/namespaces/"+DefaultClientCertSecretNamespace+"/secrets/"+DefaultClientCertSecretName,
		func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, &corev1.Secret{
				TypeMeta:   metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"},
				ObjectMeta: metav1.ObjectMeta{Name: DefaultClientCertSecretName, Namespace: DefaultClientCertSecretNamespace},
				Data: map[string][]byte{
					SecretDataKeyClientCert: []byte(keyMaterial),
					SecretDataKeyClientKey:  []byte(keyMaterial),
				},
			})
		})
	mux.HandleFunc("/api/v1/namespaces/"+utils.DefaultSandboxDeployNamespace+"/serviceaccounts/sa/token",
		func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, &authenticationv1.TokenRequest{
				TypeMeta: metav1.TypeMeta{Kind: "TokenRequest", APIVersion: "authentication.k8s.io/v1"},
				Status: authenticationv1.TokenRequestStatus{
					Token:               issuedToken,
					ExpirationTimestamp: metav1.NewTime(time.Now().Add(time.Hour)),
				},
			})
		})
	server := httptest.NewServer(mux)
	defer server.Close()

	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	require.NoError(t, err)

	verboseCtx, captured := capturingContext(9)
	require.True(t, klog.FromContext(verboseCtx).V(8).Enabled(), "precondition: the caller runs verbose")

	_, certPEM, keyPEM, err := fetchClientCertSecret(verboseCtx, clientset)
	require.NoError(t, err)
	assert.Equal(t, []byte(keyMaterial), certPEM)
	assert.Equal(t, []byte(keyMaterial), keyPEM)

	source := &saTokenSource{clientset: clientset, saName: "sa"}
	token, _, err := source.fetch(verboseCtx)
	require.NoError(t, err)
	assert.Equal(t, issuedToken, token)

	output := captured()
	// Secret data travels base64-encoded in the response body, so both encodings are
	// checked: the raw form catches a decoded dump and the encoded form the real one.
	assert.NotContains(t, output, keyMaterial, "a Secret Get body must never reach the logs")
	assert.NotContains(t, output, base64.StdEncoding.EncodeToString([]byte(keyMaterial)),
		"Secret data must never reach the logs in any encoding")
	assert.NotContains(t, output, issuedToken, "a TokenRequest body must never reach the logs")
}

// TestCredentialReadFailureErrorsStaySafe exercises both credential reads over the real
// client-go request path against a local API server that answers with a text body instead
// of a metav1.Status. client-go copies up to 2 KiB of such a body verbatim into its error,
// and a sandbox-manager running outside the user cluster may reach it through an
// intermediary that can echo request material back. Both errors are logged — by startup and by
// the refresh loop — so neither may carry that text.
func TestCredentialReadFailureErrorsStaySafe(t *testing.T) {
	const echoedKeyMaterial = "CLIENT-PRIVATE-KEY-MATERIAL"
	const echoedToken = "ISSUED-SERVICE-ACCOUNT-TOKEN"
	t.Setenv(EnvIdentityProviderClientCertSecret, DefaultClientCertSecretName)
	t.Setenv(EnvIdentityProviderClientCertNamespace, DefaultClientCertSecretNamespace)

	echoText := func(w http.ResponseWriter, secret string) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream error page echoing " + secret))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/namespaces/"+DefaultClientCertSecretNamespace+"/secrets/"+DefaultClientCertSecretName,
		func(w http.ResponseWriter, _ *http.Request) { echoText(w, echoedKeyMaterial) })
	mux.HandleFunc("/api/v1/namespaces/"+utils.DefaultSandboxDeployNamespace+"/serviceaccounts/sa/token",
		func(w http.ResponseWriter, _ *http.Request) { echoText(w, echoedToken) })
	server := httptest.NewServer(mux)
	defer server.Close()

	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	require.NoError(t, err)
	ctx := context.Background()

	// Preconditions: the raw client-go errors for these very responses do carry the echoed
	// body, so the assertions below prove redaction instead of an inert server.
	_, rawSecretErr := clientset.CoreV1().Secrets(DefaultClientCertSecretNamespace).Get(
		ctx, DefaultClientCertSecretName, metav1.GetOptions{})
	require.Error(t, rawSecretErr)
	require.Contains(t, rawSecretErr.Error(), echoedKeyMaterial,
		"precondition: client-go copies an unstructured response body into its error")

	_, rawTokenErr := clientset.CoreV1().ServiceAccounts(utils.DefaultSandboxDeployNamespace).CreateToken(
		ctx, "sa", &authenticationv1.TokenRequest{}, metav1.CreateOptions{})
	require.Error(t, rawTokenErr)
	require.Contains(t, rawTokenErr.Error(), echoedToken,
		"precondition: client-go copies an unstructured response body into its error")

	_, _, _, err = fetchClientCertSecret(ctx, clientset)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get identity provider client certificate secret "+
		DefaultClientCertSecretNamespace+"/"+DefaultClientCertSecretName,
		"the operation and the resource identity are safe diagnostic context")
	assert.Contains(t, err.Error(), "status 502", "the HTTP status is safe diagnostic context")
	assert.NotContains(t, err.Error(), echoedKeyMaterial,
		"a Secret Get failure must never carry the remote response body")

	source := &saTokenSource{clientset: clientset, saName: "sa"}
	_, _, err = source.fetch(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to request identity provider service account token for "+
		utils.DefaultSandboxDeployNamespace+"/sa",
		"the operation and the resource identity are safe diagnostic context")
	assert.Contains(t, err.Error(), "status 502", "the HTTP status is safe diagnostic context")
	assert.NotContains(t, err.Error(), echoedToken,
		"a TokenRequest failure must never carry the remote response body")
}

// ---------------------------------------------------------------------------
// envOrDefault
// ---------------------------------------------------------------------------
func TestEnvOrDefault(t *testing.T) {
	const key = "IDENTITY_PROVIDER_TEST_ENV_OR_DEFAULT"
	t.Run("returns default when unset", func(t *testing.T) {
		t.Setenv(key, "")
		assert.Equal(t, "fallback", envOrDefault(key, "fallback"))
	})
	t.Run("returns default when whitespace only", func(t *testing.T) {
		t.Setenv(key, "   ")
		assert.Equal(t, "fallback", envOrDefault(key, "fallback"))
	})
	t.Run("returns trimmed value when set", func(t *testing.T) {
		t.Setenv(key, "  value  ")
		assert.Equal(t, "value", envOrDefault(key, "fallback"))
	})
}
