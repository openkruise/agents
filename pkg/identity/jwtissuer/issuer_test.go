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

package jwtissuer

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openkruise/agents/pkg/identity/oidc"
)

const (
	testCANamespace = "sandbox-system"
	testCAName      = "oidc-ca"
	testCAKey       = "ca.crt"
)

// TestIssuedTokenPassesRealVerifier is the point of this package: a token minted
// here must be accepted by the verifier the gateway actually runs, bootstrapped
// the way the gateway actually bootstraps it: OIDC discovery over HTTPS, a JWKS
// snapshot, and a CA read from a ConfigMap. A mock verifier would prove nothing.
func TestIssuedTokenPassesRealVerifier(t *testing.T) {
	tests := []struct {
		name string
		key  crypto.Signer
	}{
		{name: "ECDSA P-256", key: mustECDSAKey(t, elliptic.P256())},
		{name: "ECDSA P-384", key: mustECDSAKey(t, elliptic.P384())},
		{name: "ECDSA P-521", key: mustECDSAKey(t, elliptic.P521())},
		{name: "RSA 2048", key: mustRSAKey(t)},
		{name: "Ed25519", key: mustEd25519Key(t)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issuer, server := startIssuer(t, SigningKey{KeyID: "active", PrivateKey: tt.key})
			verifier := newRealVerifier(t, server)

			binding := SandboxBinding{SandboxID: "default--sample", SandboxUID: "89d24507-936c-4a04-a958-b5d6a8277ed5"}
			rawJWT, expiry, err := issuer.IssueTrafficAccessToken("e2b:controlplane:client", binding, 0)
			require.NoError(t, err)

			claims, err := verifier.Verify(rawJWT)
			require.NoError(t, err)

			assert.Equal(t, server.URL, claims.Issuer)
			assert.Equal(t, "e2b:controlplane:client", claims.Subject)
			assert.Equal(t, binding.SandboxID, claims.Sandbox.SandboxID)
			assert.Equal(t, binding.SandboxUID, claims.Sandbox.SandboxUID)
			// The returned expiry must match the exp claim, so callers can record
			// it without parsing the token back.
			assert.WithinDuration(t, expiry, claims.Expiry.Time(), time.Second)
		})
	}
}

// TestRotationOverlapWindow covers the constraint that makes rotation awkward
// here: the gateway snapshots the JWKS once and never refetches it. A key must
// therefore be published before it signs anything, and the previous key must
// stay published until every gateway has rolled.
func TestRotationOverlapWindow(t *testing.T) {
	oldKey := mustECDSAKey(t, elliptic.P256())
	newKey := mustECDSAKey(t, elliptic.P256())

	// Step 1: only the old key exists. A gateway starting now snapshots it alone.
	beforeRotation, server := startIssuer(t, SigningKey{KeyID: "old", PrivateKey: oldKey})
	staleVerifier := newRealVerifier(t, server)

	tokenFromOldKey, _, err := beforeRotation.IssueTrafficAccessToken("sub", testBinding(), 0)
	require.NoError(t, err)
	_, err = staleVerifier.Verify(tokenFromOldKey)
	require.NoError(t, err)

	// Step 2: the new key is published but the old one still signs. A gateway
	// that has not restarted keeps working.
	publishOnly, publishServer := startIssuer(t,
		SigningKey{KeyID: "old", PrivateKey: oldKey},
		WithRetainedKeys(VerificationKey{KeyID: "new", PublicKey: newKey.Public()}),
	)
	refreshedVerifier := newRealVerifier(t, publishServer)

	tokenDuringOverlap, _, err := publishOnly.IssueTrafficAccessToken("sub", testBinding(), 0)
	require.NoError(t, err)
	_, err = refreshedVerifier.Verify(tokenDuringOverlap)
	require.NoError(t, err)

	// Step 3: signing flips to the new key while the old one stays published.
	afterRotation, rotatedServer := startIssuer(t,
		SigningKey{KeyID: "new", PrivateKey: newKey},
		WithRetainedKeys(VerificationKey{KeyID: "old", PublicKey: oldKey.Public()}),
	)
	rotatedVerifier := newRealVerifier(t, rotatedServer)

	tokenFromNewKey, _, err := afterRotation.IssueTrafficAccessToken("sub", testBinding(), 0)
	require.NoError(t, err)
	_, err = rotatedVerifier.Verify(tokenFromNewKey)
	require.NoError(t, err)

	// The hazard the overlap window exists to prevent: a gateway holding the
	// pre-rotation snapshot cannot verify a token signed by the new key.
	_, err = staleVerifier.Verify(tokenFromNewKey)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown kid")
}

func TestVerifierRejectsTokensOutsideValidityWindow(t *testing.T) {
	tests := []struct {
		name        string
		clockOffset time.Duration
		lifetime    time.Duration
		expectError string
	}{
		{name: "expired", clockOffset: -2 * time.Hour, lifetime: time.Hour, expectError: "expired"},
		{name: "not yet valid", clockOffset: 2 * time.Hour, lifetime: time.Hour, expectError: "not valid yet"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := mustECDSAKey(t, elliptic.P256())
			issuer, server := startIssuer(t,
				SigningKey{KeyID: "active", PrivateKey: key},
				WithTokenLifetime(tt.lifetime),
				withClock(func() time.Time { return time.Now().Add(tt.clockOffset) }),
			)
			verifier := newRealVerifier(t, server)

			rawJWT, _, err := issuer.IssueTrafficAccessToken("sub", testBinding(), 0)
			require.NoError(t, err)

			_, err = verifier.Verify(rawJWT)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
		})
	}
}

func TestVerifierRejectsTokenFromAnotherIssuer(t *testing.T) {
	// Two independent issuers, each with its own key and issuer URL.
	_, trustedServer := startIssuer(t, SigningKey{KeyID: "trusted", PrivateKey: mustECDSAKey(t, elliptic.P256())})
	foreign, _ := startIssuer(t, SigningKey{KeyID: "foreign", PrivateKey: mustECDSAKey(t, elliptic.P256())})

	verifier := newRealVerifier(t, trustedServer)

	rawJWT, _, err := foreign.IssueTrafficAccessToken("sub", testBinding(), 0)
	require.NoError(t, err)

	_, err = verifier.Verify(rawJWT)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown kid")
}

func TestNewValidatesInput(t *testing.T) {
	validKey := mustECDSAKey(t, elliptic.P256())

	tests := []struct {
		name        string
		issuerURL   string
		signing     SigningKey
		opts        []Option
		expectError string
	}{
		{
			name:        "plaintext issuer URL",
			issuerURL:   "http://issuer.example",
			signing:     SigningKey{KeyID: "active", PrivateKey: validKey},
			expectError: "absolute HTTPS URL",
		},
		{
			name:        "relative issuer URL",
			issuerURL:   "/issuer",
			signing:     SigningKey{KeyID: "active", PrivateKey: validKey},
			expectError: "absolute HTTPS URL",
		},
		{
			name:        "empty key ID",
			issuerURL:   "https://issuer.example",
			signing:     SigningKey{PrivateKey: validKey},
			expectError: "key ID must not be empty",
		},
		{
			name:        "nil signing key",
			issuerURL:   "https://issuer.example",
			signing:     SigningKey{KeyID: "active"},
			expectError: "signing key must not be nil",
		},
		{
			name:        "non-positive lifetime",
			issuerURL:   "https://issuer.example",
			signing:     SigningKey{KeyID: "active", PrivateKey: validKey},
			opts:        []Option{WithTokenLifetime(0)},
			expectError: "lifetime must be positive",
		},
		{
			name:      "retained key reuses the active key ID",
			issuerURL: "https://issuer.example",
			signing:   SigningKey{KeyID: "active", PrivateKey: validKey},
			opts: []Option{WithRetainedKeys(
				VerificationKey{KeyID: "active", PublicKey: mustECDSAKey(t, elliptic.P256()).Public()},
			)},
			expectError: "duplicate key ID",
		},
		{
			name:        "retained key without an ID",
			issuerURL:   "https://issuer.example",
			signing:     SigningKey{KeyID: "active", PrivateKey: validKey},
			opts:        []Option{WithRetainedKeys(VerificationKey{PublicKey: validKey.Public()})},
			expectError: "retained key ID must not be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issuer, err := New(tt.issuerURL, tt.signing, tt.opts...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
			assert.Nil(t, issuer)
		})
	}
}

// TestIssueTrafficAccessTokenValidity covers the per-call lifetime. The caller
// resolves validity as policy per request, so a token must honour the value it
// was given and fall back to the issuer default only when none was supplied.
func TestIssueTrafficAccessTokenValidity(t *testing.T) {
	// Pinned to a fixed instant so the expiry arithmetic is exact, but taken
	// from now rather than a literal date so the real verifier still accepts
	// the token inside its clock-skew window.
	issuedAt := time.Now()

	tests := []struct {
		name string
		// issuerLifetime configures the issuer default; zero leaves it unset so
		// DefaultTokenLifetime applies.
		issuerLifetime time.Duration
		validity       time.Duration
		wantLifetime   time.Duration
	}{
		{
			name:         "zero falls back to the issuer default",
			validity:     0,
			wantLifetime: DefaultTokenLifetime,
		},
		{
			name:           "zero falls back to a configured default",
			issuerLifetime: 15 * time.Minute,
			validity:       0,
			wantLifetime:   15 * time.Minute,
		},
		{
			name:           "a per-call validity overrides the default",
			issuerLifetime: 15 * time.Minute,
			validity:       2 * time.Hour,
			wantLifetime:   2 * time.Hour,
		},
		{
			name:           "a per-call validity shorter than the default is honoured",
			issuerLifetime: time.Hour,
			validity:       30 * time.Second,
			wantLifetime:   30 * time.Second,
		},
		{
			name:         "a negative validity is treated as unset",
			validity:     -time.Hour,
			wantLifetime: DefaultTokenLifetime,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := []Option{withClock(func() time.Time { return issuedAt })}
			if tt.issuerLifetime > 0 {
				opts = append(opts, WithTokenLifetime(tt.issuerLifetime))
			}
			issuer, server := startIssuer(t,
				SigningKey{KeyID: "active", PrivateKey: mustECDSAKey(t, elliptic.P256())}, opts...)
			verifier := newRealVerifier(t, server)

			rawJWT, expiry, err := issuer.IssueTrafficAccessToken("sub", testBinding(), tt.validity)
			require.NoError(t, err)

			assert.Equal(t, issuedAt.Add(tt.wantLifetime).Unix(), expiry.Unix(),
				"returned expiry must reflect the resolved validity")

			// The returned expiry is only useful if it matches what is sealed
			// inside the token, since callers record one and verifiers read the
			// other. Verifying through the real verifier also proves a token with
			// a per-call lifetime is still accepted rather than merely issued.
			claims, err := verifier.Verify(rawJWT)
			require.NoError(t, err)
			assert.WithinDuration(t, expiry, claims.Expiry.Time(), time.Second)
		})
	}
}

func TestIssueTrafficAccessTokenValidatesBinding(t *testing.T) {
	issuer, err := New("https://issuer.example", SigningKey{KeyID: "active", PrivateKey: mustECDSAKey(t, elliptic.P256())})
	require.NoError(t, err)

	tests := []struct {
		name        string
		subject     string
		binding     SandboxBinding
		expectError string
	}{
		{name: "empty subject", binding: testBinding(), expectError: "subject must not be empty"},
		{
			name:        "empty sandbox ID",
			subject:     "sub",
			binding:     SandboxBinding{SandboxUID: "uid"},
			expectError: "sandbox ID must not be empty",
		},
		{
			name:        "empty sandbox UID",
			subject:     "sub",
			binding:     SandboxBinding{SandboxID: "default--sample"},
			expectError: "sandbox UID must not be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rawJWT, _, err := issuer.IssueTrafficAccessToken(tt.subject, tt.binding, 0)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
			assert.Empty(t, rawJWT)
		})
	}
}

// TestJWKSNeverExposesPrivateKeyMaterial guards the one mistake in this package
// that would be unrecoverable in production.
func TestJWKSNeverExposesPrivateKeyMaterial(t *testing.T) {
	issuer, server := startIssuer(t, SigningKey{KeyID: "active", PrivateKey: mustECDSAKey(t, elliptic.P256())})

	keySet, err := issuer.PublicKeySet()
	require.NoError(t, err)
	for _, key := range keySet.Keys {
		assert.True(t, key.IsPublic(), "key %q must be public", key.KeyID)
	}

	response, err := server.Client().Get(server.URL + JWKSPath)
	require.NoError(t, err)
	defer response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)

	// A private ECDSA key would serialize its scalar as "d".
	var published struct {
		Keys []map[string]any `json:"keys"`
	}
	require.NoError(t, decodeJSON(response.Body, &published))
	require.NotEmpty(t, published.Keys)
	for _, key := range published.Keys {
		assert.NotContains(t, key, "d", "JWKS must not publish private key material")
	}
}

func TestEndpointsRejectNonReadMethods(t *testing.T) {
	_, server := startIssuer(t, SigningKey{KeyID: "active", PrivateKey: mustECDSAKey(t, elliptic.P256())})

	for _, path := range []string{DiscoveryPath, JWKSPath} {
		t.Run(path, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, server.URL+path, nil)
			require.NoError(t, err)
			response, err := server.Client().Do(request)
			require.NoError(t, err)
			defer response.Body.Close()
			assert.Equal(t, http.StatusMethodNotAllowed, response.StatusCode)
		})
	}
}

// startIssuer serves an Issuer over HTTPS and points its issuer URL at itself,
// which is what the verifier requires: the iss claim must equal the issuer
// advertised by discovery. The server URL is only known after it starts, so the
// handler is installed once the Issuer exists.
func startIssuer(t *testing.T, signing SigningKey, opts ...Option) (*Issuer, *httptest.Server) {
	t.Helper()

	var handler http.Handler
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	server.StartTLS()
	t.Cleanup(server.Close)

	issuer, err := New(server.URL, signing, opts...)
	require.NoError(t, err)

	handler, err = issuer.Handler()
	require.NoError(t, err)
	return issuer, server
}

// newRealVerifier builds the production verifier against the running issuer,
// with the server's own CA supplied through a ConfigMap exactly as the gateway
// reads it.
func newRealVerifier(t *testing.T, server *httptest.Server) oidc.Verifier {
	t.Helper()

	verifier, err := oidc.NewVerifier(context.Background(), caReader(t, server), oidc.Options{
		DiscoveryURL:         server.URL + DiscoveryPath,
		CAConfigMapNamespace: testCANamespace,
		CAConfigMapName:      testCAName,
		CAConfigMapKey:       testCAKey,
	})
	require.NoError(t, err)
	return verifier
}

func caReader(t *testing.T, server *httptest.Server) client.Reader {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: testCANamespace, Name: testCAName},
		Data:       map[string]string{testCAKey: string(caPEM)},
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build()
}

func testBinding() SandboxBinding {
	return SandboxBinding{SandboxID: "default--sample", SandboxUID: "89d24507-936c-4a04-a958-b5d6a8277ed5"}
}

func mustECDSAKey(t *testing.T, curve elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	require.NoError(t, err)
	return key
}

func mustRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return key
}

func mustEd25519Key(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return key
}

func decodeJSON(reader io.Reader, dest any) error {
	return json.NewDecoder(reader).Decode(dest)
}

// TestIssuerBehindPathPrefix covers an issuer served under an ingress path
// rather than at the root. The discovery document has to advertise a jwks_uri
// that is actually reachable, otherwise the gateway cannot bootstrap at all.
func TestIssuerBehindPathPrefix(t *testing.T) {
	signing := SigningKey{KeyID: "active", PrivateKey: mustECDSAKey(t, elliptic.P256())}

	var handler http.Handler
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	server.StartTLS()
	t.Cleanup(server.Close)

	issuer, err := New(server.URL+"/identity", signing)
	require.NoError(t, err)
	handler, err = issuer.Handler()
	require.NoError(t, err)

	verifier, err := oidc.NewVerifier(context.Background(), caReader(t, server), oidc.Options{
		DiscoveryURL:         server.URL + "/identity" + DiscoveryPath,
		CAConfigMapNamespace: testCANamespace,
		CAConfigMapName:      testCAName,
		CAConfigMapKey:       testCAKey,
	})
	require.NoError(t, err)

	rawJWT, _, err := issuer.IssueTrafficAccessToken("sub", testBinding(), 0)
	require.NoError(t, err)

	claims, err := verifier.Verify(rawJWT)
	require.NoError(t, err)
	assert.Equal(t, server.URL+"/identity", claims.Issuer)
}

func testAgentBinding() AgentBinding {
	return AgentBinding{
		Namespace:         "team-a",
		AgentIdentityName: "sample-agent",
		SandboxUID:        "89d24507-936c-4a04-a958-b5d6a8277ed5",
	}
}

// decodeAgentClaims reads the claims back without verifying, so a test can
// assert on what was actually sealed into the token rather than on the inputs.
func decodeAgentClaims(t *testing.T, rawJWT string) agentTokenClaims {
	t.Helper()
	parsed, err := jwt.ParseSigned(rawJWT, []jose.SignatureAlgorithm{jose.ES256, jose.RS256, jose.EdDSA})
	require.NoError(t, err)
	var claims agentTokenClaims
	require.NoError(t, parsed.UnsafeClaimsWithoutVerification(&claims))
	return claims
}

// TestIssueAgentTokenSealsTheWorkloadIdentity covers the claims the control
// plane derives. These are the whole point of the token: the identity provider
// trusts them precisely because a requester could not have supplied them.
func TestIssueAgentTokenSealsTheWorkloadIdentity(t *testing.T) {
	issuedAt := time.Now()
	issuer, err := New("https://issuer.example",
		SigningKey{KeyID: "active", PrivateKey: mustECDSAKey(t, elliptic.P256())},
		withClock(func() time.Time { return issuedAt }))
	require.NoError(t, err)

	binding := testAgentBinding()
	rawJWT, expiry, err := issuer.IssueAgentToken(binding, 0)
	require.NoError(t, err)

	claims := decodeAgentClaims(t, rawJWT)
	assert.Equal(t, "https://issuer.example", claims.Issuer)
	assert.Equal(t, binding.Namespace, claims.Namespace)
	assert.Equal(t, binding.AgentIdentityName, claims.AgentIdentityName)
	assert.Equal(t, binding.SandboxUID, claims.SandboxUID)

	// The subject names the agent, not the sandbox, because delegations are
	// isolated per agent identity.
	assert.Equal(t, "agent:team-a:sample-agent", claims.Subject)

	// token_type and aud are what stop this being accepted where a traffic
	// access token or a principal token is expected.
	assert.Equal(t, AgentTokenType, claims.TokenType)
	assert.Equal(t, jwt.Audience{AgentTokenAudience}, claims.Audience)

	// The returned expiry must match what is sealed inside, so a caller can
	// schedule a refresh without parsing the token back.
	assert.Equal(t, issuedAt.Add(DefaultTokenLifetime).Unix(), expiry.Unix())
	assert.Equal(t, expiry.Unix(), claims.Expiry.Time().Unix())
	assert.Equal(t, issuedAt.Unix(), claims.IssuedAt.Time().Unix())
	assert.Equal(t, issuedAt.Unix(), claims.NotBefore.Time().Unix())
}

// TestAgentTokenIsNotATrafficAccessToken is the non-interchangeability
// requirement. Both tokens come off the same key, so nothing but the claims
// distinguishes them.
func TestAgentTokenIsNotATrafficAccessToken(t *testing.T) {
	issuer, err := New("https://issuer.example",
		SigningKey{KeyID: "active", PrivateKey: mustECDSAKey(t, elliptic.P256())})
	require.NoError(t, err)

	agentJWT, _, err := issuer.IssueAgentToken(testAgentBinding(), 0)
	require.NoError(t, err)
	trafficJWT, _, err := issuer.IssueTrafficAccessToken("sub", testBinding(), 0)
	require.NoError(t, err)

	agent := decodeAgentClaims(t, agentJWT)
	traffic := decodeAgentClaims(t, trafficJWT)

	assert.Equal(t, AgentTokenType, agent.TokenType)
	assert.Empty(t, traffic.TokenType, "a traffic access token must not claim an agent token_type")
	assert.NotEmpty(t, agent.Audience, "an agent token must name its audience")
	assert.Empty(t, traffic.Audience, "the traffic access token carries no aud today")
	assert.Empty(t, traffic.SandboxUID, "the traffic token nests its sandbox claims elsewhere")
}

func TestIssueAgentTokenValidatesBinding(t *testing.T) {
	issuer, err := New("https://issuer.example",
		SigningKey{KeyID: "active", PrivateKey: mustECDSAKey(t, elliptic.P256())})
	require.NoError(t, err)

	tests := []struct {
		name        string
		mutate      func(*AgentBinding)
		expectError string
	}{
		{
			name:        "namespace is required",
			mutate:      func(b *AgentBinding) { b.Namespace = "" },
			expectError: "namespace must not be empty",
		},
		{
			name:        "agent identity name is required",
			mutate:      func(b *AgentBinding) { b.AgentIdentityName = "" },
			expectError: "agent identity name must not be empty",
		},
		{
			name:        "sandbox UID is required",
			mutate:      func(b *AgentBinding) { b.SandboxUID = "" },
			expectError: "sandbox UID must not be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binding := testAgentBinding()
			tt.mutate(&binding)

			rawJWT, expiry, err := issuer.IssueAgentToken(binding, 0)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
			assert.Empty(t, rawJWT)
			assert.True(t, expiry.IsZero())
		})
	}
}
