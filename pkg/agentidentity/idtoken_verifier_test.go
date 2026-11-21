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

package agentidentity

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
)

// startJWKSServer serves a real JWKS document over HTTP, standing in for an
// external IdP's JWKS endpoint. fetchJWKS trusts whatever Status.JWKSURI
// names, which production only ever populates with an HTTPS URL already
// validated by the AgentAuthenticationConfig controller; a plain httptest
// server is enough to exercise fetchJWKS and verifyIDToken's own logic
// without also standing up TLS trust for a self-signed certificate.
func startJWKSServer(t *testing.T, keys ...jose.JSONWebKey) *httptest.Server {
	t.Helper()
	body, err := json.Marshal(jose.JSONWebKeySet{Keys: keys})
	if err != nil {
		t.Fatalf("marshal JWKS: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server
}

func mustIDTokenKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ID token signing key: %v", err)
	}
	return key
}

func publicJWKFor(kid string, key crypto.PublicKey) jose.JSONWebKey {
	return jose.JSONWebKey{Key: key, KeyID: kid, Use: "sig", Algorithm: string(jose.ES256)}
}

// signIDToken signs claims with key under kid, the same shape a real IdP's ID
// token has: a flat, compact JWS.
func signIDToken(t *testing.T, key crypto.Signer, kid string, claims jwt.Claims) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid))
	if err != nil {
		t.Fatalf("build ID token signer: %v", err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("sign ID token: %v", err)
	}
	return raw
}

// resolvedAuthConfig builds an AgentAuthenticationConfig as the controller
// would leave it once discovery succeeded: Ready, with Status.Issuer and
// Status.JWKSURI populated and the spec's allowed audience recorded.
func resolvedAuthConfig(name, issuer, jwksURI string, allowedAudience ...string) *securityv1alpha1.AgentAuthenticationConfig {
	return &securityv1alpha1.AgentAuthenticationConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Generation: 1},
		Spec: securityv1alpha1.AgentAuthenticationConfigSpec{
			Type: "jwt",
			JWT: &securityv1alpha1.JWTAuthentication{
				DiscoveryURL:    "https://" + name + ".example/.well-known/openid-configuration",
				AllowedAudience: allowedAudience,
			},
		},
		Status: securityv1alpha1.AgentAuthenticationConfigStatus{
			ObservedGeneration: 1,
			Issuer:             issuer,
			JWKSURI:            jwksURI,
			Conditions: []metav1.Condition{{
				Type:               securityv1alpha1.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             "Test",
				ObservedGeneration: 1,
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

// staleAuthConfig bumps generation past what status reports, the same
// mismatch staleConfig uses elsewhere in this package: the condition still
// says Ready=True but describes a generation the spec has already moved past.
func staleAuthConfig(config *securityv1alpha1.AgentAuthenticationConfig) *securityv1alpha1.AgentAuthenticationConfig {
	config.Generation = 2
	return config
}

func TestVerifyIDToken(t *testing.T) {
	idpKey := mustIDTokenKey(t)
	otherKey := mustIDTokenKey(t)
	jwksServer := startJWKSServer(t, publicJWKFor("idp-key-1", idpKey.Public()))

	const issuer = "https://idp.example.com"
	const audience = "checkout-agent"
	const subject = "alice@example.com"

	validClaims := func() jwt.Claims {
		now := time.Now()
		return jwt.Claims{
			Issuer:   issuer,
			Subject:  subject,
			Audience: jwt.Audience{audience},
			IssuedAt: jwt.NewNumericDate(now),
			Expiry:   jwt.NewNumericDate(now.Add(time.Hour)),
		}
	}

	tests := []struct {
		name        string
		identity    *securityv1alpha1.AgentIdentity
		configs     []*securityv1alpha1.AgentAuthenticationConfig
		rawIDToken  func() string
		expectError string
		wantSubject string
	}{
		{
			name:     "valid",
			identity: identity("checkout-agent", 1, "idp"),
			configs:  []*securityv1alpha1.AgentAuthenticationConfig{resolvedAuthConfig("idp", issuer, jwksServer.URL, audience)},
			rawIDToken: func() string {
				return signIDToken(t, idpKey, "idp-key-1", validClaims())
			},
			wantSubject: subject,
		},
		{
			// A single configured issuer that does not match the token's own iss:
			// there is nothing to verify against, so this fails the same way an
			// unconfigured issuer would.
			name:     "wrong issuer in token vs config",
			identity: identity("checkout-agent", 1, "idp"),
			configs:  []*securityv1alpha1.AgentAuthenticationConfig{resolvedAuthConfig("idp", "https://a-different-idp.example.com", jwksServer.URL, audience)},
			rawIDToken: func() string {
				return signIDToken(t, idpKey, "idp-key-1", validClaims())
			},
			expectError: "no AgentAuthenticationConfig",
		},
		{
			name:     "audience not in allowed list",
			identity: identity("checkout-agent", 1, "idp"),
			configs:  []*securityv1alpha1.AgentAuthenticationConfig{resolvedAuthConfig("idp", issuer, jwksServer.URL, "a-different-audience")},
			rawIDToken: func() string {
				return signIDToken(t, idpKey, "idp-key-1", validClaims())
			},
			expectError: "does not intersect",
		},
		{
			name:     "expired",
			identity: identity("checkout-agent", 1, "idp"),
			configs:  []*securityv1alpha1.AgentAuthenticationConfig{resolvedAuthConfig("idp", issuer, jwksServer.URL, audience)},
			rawIDToken: func() string {
				claims := validClaims()
				claims.Expiry = jwt.NewNumericDate(time.Now().Add(-2 * time.Hour))
				return signIDToken(t, idpKey, "idp-key-1", claims)
			},
			expectError: "validate ID token claims",
		},
		{
			// Correct kid, but signed by a different private key: the kind of
			// forgery attempt a kid-only check would miss.
			name:     "signature from a different key",
			identity: identity("checkout-agent", 1, "idp"),
			configs:  []*securityv1alpha1.AgentAuthenticationConfig{resolvedAuthConfig("idp", issuer, jwksServer.URL, audience)},
			rawIDToken: func() string {
				return signIDToken(t, otherKey, "idp-key-1", validClaims())
			},
			expectError: "verify ID token signature",
		},
		{
			// Two refs; only the second's resolved issuer matches the token's
			// unverified iss, so it has to be picked rather than the first.
			name:     "multiple refs, only one matches by issuer",
			identity: identity("checkout-agent", 1, "other-idp", "idp"),
			configs: []*securityv1alpha1.AgentAuthenticationConfig{
				resolvedAuthConfig("other-idp", "https://unrelated-idp.example.com", jwksServer.URL, audience),
				resolvedAuthConfig("idp", issuer, jwksServer.URL, audience),
			},
			rawIDToken: func() string {
				return signIDToken(t, idpKey, "idp-key-1", validClaims())
			},
			wantSubject: subject,
		},
		{
			name:     "no ref matches",
			identity: identity("checkout-agent", 1, "idp-a", "idp-b"),
			configs: []*securityv1alpha1.AgentAuthenticationConfig{
				resolvedAuthConfig("idp-a", "https://idp-a.example.com", jwksServer.URL, audience),
				resolvedAuthConfig("idp-b", "https://idp-b.example.com", jwksServer.URL, audience),
			},
			rawIDToken: func() string {
				return signIDToken(t, idpKey, "idp-key-1", validClaims())
			},
			expectError: "no AgentAuthenticationConfig",
		},
		{
			// The matching ref resolves to the right issuer but is not Ready:
			// e.g. its spec changed and the controller has not caught up yet.
			name:     "matched config not Ready",
			identity: identity("checkout-agent", 1, "idp"),
			configs: []*securityv1alpha1.AgentAuthenticationConfig{
				staleAuthConfig(resolvedAuthConfig("idp", issuer, jwksServer.URL, audience)),
			},
			rawIDToken: func() string {
				return signIDToken(t, idpKey, "idp-key-1", validClaims())
			},
			expectError: "is not Ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := make([]client.Object, 0, len(tt.configs))
			for _, config := range tt.configs {
				objects = append(objects, config)
			}
			fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

			verified, err := verifyIDToken(context.Background(), fc, testNamespace, tt.identity, tt.rawIDToken())
			if tt.expectError != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got none", tt.expectError)
				}
				if !strings.Contains(err.Error(), tt.expectError) {
					t.Errorf("error: got %q, want it to contain %q", err, tt.expectError)
				}
				return
			}
			if err != nil {
				t.Fatalf("verifyIDToken: unexpected error: %v", err)
			}
			if verified.Issuer != issuer {
				t.Errorf("issuer: got %q, want %q", verified.Issuer, issuer)
			}
			if verified.Subject != tt.wantSubject {
				t.Errorf("subject: got %q, want %q", verified.Subject, tt.wantSubject)
			}
		})
	}
}

func TestVerifyIDTokenRejectsUnparsableInput(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	_, err := verifyIDToken(context.Background(), fc, testNamespace, identity("checkout-agent", 1, "idp"), "not-a-jwt")
	if err == nil {
		t.Fatal("expected an error for an unparsable ID token")
	}
	if !strings.Contains(err.Error(), "parse ID token") {
		t.Errorf("error: got %q, want it to mention parsing", err)
	}
}
