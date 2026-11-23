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
	"errors"
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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// failingJWKSServer always answers 500, standing in for an external IdP's
// JWKS endpoint that is down or misconfigured.
func failingJWKSServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
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
			// A dangling reference, distinct from "no ref matches" above: this
			// ref names a config that was never created at all, not one whose
			// issuer simply does not match. authConfigForIssuer's own doc
			// comment says this is skipped rather than failing outright, and
			// that only another ref matching is what makes this one path
			// end in success rather than "no AgentAuthenticationConfig".
			name:     "one ref is dangling, the other matches",
			identity: identity("checkout-agent", 1, "does-not-exist", "idp"),
			configs:  []*securityv1alpha1.AgentAuthenticationConfig{resolvedAuthConfig("idp", issuer, jwksServer.URL, audience)},
			rawIDToken: func() string {
				return signIDToken(t, idpKey, "idp-key-1", validClaims())
			},
			wantSubject: subject,
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
		{
			name:     "ID token header carries no kid",
			identity: identity("checkout-agent", 1, "idp"),
			configs:  []*securityv1alpha1.AgentAuthenticationConfig{resolvedAuthConfig("idp", issuer, jwksServer.URL, audience)},
			rawIDToken: func() string {
				return signIDToken(t, idpKey, "", validClaims())
			},
			expectError: "must contain a kid",
		},
		{
			name:     "ID token kid matches no published key",
			identity: identity("checkout-agent", 1, "idp"),
			configs:  []*securityv1alpha1.AgentAuthenticationConfig{resolvedAuthConfig("idp", issuer, jwksServer.URL, audience)},
			rawIDToken: func() string {
				return signIDToken(t, idpKey, "a-kid-this-jwks-never-published", validClaims())
			},
			expectError: "unknown kid",
		},
		{
			// The JWKS entry's own declared Algorithm disagrees with the alg
			// the token header actually carries - the external IdP's JWKS is
			// untrusted input, so this has to be checked explicitly, unlike
			// the issuer's own JWKS in jwtissuer.VerifyAgentToken, which is
			// always internally consistent by construction.
			name:     "token algorithm does not match the published key's declared algorithm",
			identity: identity("checkout-agent", 1, "mismatched-alg"),
			configs: []*securityv1alpha1.AgentAuthenticationConfig{
				resolvedAuthConfig("mismatched-alg", "https://mismatched-alg.example.com",
					startJWKSServer(t, jose.JSONWebKey{
						Key: idpKey.Public(), KeyID: "mismatched-key", Use: "sig", Algorithm: "RS256",
					}).URL, audience),
			},
			rawIDToken: func() string {
				claims := validClaims()
				claims.Issuer = "https://mismatched-alg.example.com"
				return signIDToken(t, idpKey, "mismatched-key", claims)
			},
			expectError: "does not match key algorithm",
		},
		{
			name:     "ID token has no sub claim",
			identity: identity("checkout-agent", 1, "idp"),
			configs:  []*securityv1alpha1.AgentAuthenticationConfig{resolvedAuthConfig("idp", issuer, jwksServer.URL, audience)},
			rawIDToken: func() string {
				claims := validClaims()
				claims.Subject = ""
				return signIDToken(t, idpKey, "idp-key-1", claims)
			},
			expectError: "missing sub claim",
		},
		{
			name:     "the matched config's JWKS endpoint itself fails",
			identity: identity("checkout-agent", 1, "unreachable"),
			configs: []*securityv1alpha1.AgentAuthenticationConfig{
				resolvedAuthConfig("unreachable", "https://unreachable.example.com", failingJWKSServer(t).URL, audience),
			},
			rawIDToken: func() string {
				claims := validClaims()
				claims.Issuer = "https://unreachable.example.com"
				return signIDToken(t, idpKey, "idp-key-1", claims)
			},
			expectError: "fetch JWKS for issuer",
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

// TestFetchJWKS covers fetchJWKS directly rather than only through
// verifyIDToken, since most of its error paths - a non-200 response, a
// malformed body, and every reason a key can be filtered out of the result -
// are reachable only by controlling the JWKS endpoint's response at exactly
// the byte level, which a test built around a real signed ID token cannot
// do without obscuring what is actually being checked.
func TestFetchJWKS(t *testing.T) {
	t.Run("the endpoint is unreachable", func(t *testing.T) {
		// A server that is closed before the request arrives: fetchJWKS's
		// httpClient.Do fails outright, rather than returning any response to
		// inspect, which is a different failure point from a non-200 status.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		server.Close()

		_, err := fetchJWKS(context.Background(), server.URL)
		if err == nil || !strings.Contains(err.Error(), "fetch JWKS") {
			t.Errorf("error: got %v, want it to mention fetching", err)
		}
	})

	t.Run("non-200 status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		_, err := fetchJWKS(context.Background(), server.URL)
		if err == nil || !strings.Contains(err.Error(), "unexpected status") {
			t.Errorf("error: got %v, want it to mention the unexpected status", err)
		}
	})

	t.Run("malformed JSON body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("{not json"))
		}))
		defer server.Close()

		_, err := fetchJWKS(context.Background(), server.URL)
		if err == nil || !strings.Contains(err.Error(), "decode JWKS") {
			t.Errorf("error: got %v, want it to mention decoding", err)
		}
	})

	t.Run("empty keys array has nothing usable", func(t *testing.T) {
		server := startJWKSServer(t)
		_, err := fetchJWKS(context.Background(), server.URL)
		if err == nil || !strings.Contains(err.Error(), "no usable verification keys") {
			t.Errorf("error: got %v, want it to mention no usable keys", err)
		}
	})

	t.Run("a key reserved for encryption is filtered out, a key for signing is kept", func(t *testing.T) {
		signingKey := mustIDTokenKey(t)
		encryptionOnly := publicJWKFor("enc-only", mustIDTokenKey(t).Public())
		encryptionOnly.Use = "enc"
		server := startJWKSServer(t, encryptionOnly, publicJWKFor("sig-key", signingKey.Public()))

		keys, err := fetchJWKS(context.Background(), server.URL)
		if err != nil {
			t.Fatalf("fetchJWKS: %v", err)
		}
		if _, ok := keys["enc-only"]; ok {
			t.Error("the encryption-only key was not filtered out")
		}
		if _, ok := keys["sig-key"]; !ok {
			t.Error("the signing key was filtered out along with it")
		}
	})

	t.Run("a symmetric key is filtered out, never treated as an ID token verification key", func(t *testing.T) {
		signingKey := mustIDTokenKey(t)
		symmetric := jose.JSONWebKey{
			Key: []byte("0123456789abcdef0123456789abcdef"), KeyID: "symmetric-1", Use: "sig", Algorithm: "HS256",
		}
		server := startJWKSServer(t, symmetric, publicJWKFor("sig-key", signingKey.Public()))

		keys, err := fetchJWKS(context.Background(), server.URL)
		if err != nil {
			t.Fatalf("fetchJWKS: %v", err)
		}
		if _, ok := keys["symmetric-1"]; ok {
			t.Error("the symmetric key was not filtered out")
		}
		if _, ok := keys["sig-key"]; !ok {
			t.Error("the asymmetric signing key was filtered out along with it")
		}
	})
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

// TestUnverifiedIDTokenIssuer covers the pre-check directly, including the
// one path TestVerifyIDToken cannot reach: a syntactically valid JWS whose
// payload is not a JSON object, which parses but fails to decode into claims.
func TestUnverifiedIDTokenIssuer(t *testing.T) {
	t.Run("unparsable", func(t *testing.T) {
		_, err := unverifiedIDTokenIssuer("not-a-jwt")
		if err == nil || !strings.Contains(err.Error(), "parse ID token") {
			t.Errorf("error: got %v, want it to mention parsing", err)
		}
	})

	// The "read ID token claims" branch, a syntactically valid JWS whose
	// payload parses but is not a JSON object, has no test here: go-jose's
	// own jwt.Signed refuses at construction time to produce one
	// ("expected claims to be value convertible into JSON object"), so no
	// token built through the library this codebase signs with, or that any
	// other conformant JWT library would produce, can reach it.

	t.Run("missing iss claim", func(t *testing.T) {
		key := mustIDTokenKey(t)
		raw := signIDToken(t, key, "k1", jwt.Claims{Subject: "alice"})
		_, err := unverifiedIDTokenIssuer(raw)
		if err == nil || !strings.Contains(err.Error(), "missing iss claim") {
			t.Errorf("error: got %v, want it to mention the missing iss claim", err)
		}
	})
}

// TestAuthConfigForIssuerSurfacesReadFailures covers the one branch
// TestVerifyIDToken's table cannot reach without a client that fails on
// demand: a Get that fails for a reason other than the config simply not
// existing, which must be surfaced rather than silently skipped the way a
// genuine absence is.
func TestAuthConfigForIssuerSurfacesReadFailures(t *testing.T) {
	errBoom := errors.New("boom")
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
				obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*securityv1alpha1.AgentAuthenticationConfig); ok {
					return errBoom
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()

	_, err := authConfigForIssuer(context.Background(), fc, testNamespace,
		identity("checkout-agent", 1, "idp"), "https://idp.example.com")
	if err == nil || !strings.Contains(err.Error(), "get AgentAuthenticationConfig") {
		t.Errorf("error: got %v, want it to mention the read failure", err)
	}
}

// TestIsAsymmetricPublicKey covers the classification fetchJWKS filters keys
// by, directly: a JWK's Valid() check can filter an unusable key before this
// is ever reached, so testing it only through fetchJWKS cannot guarantee
// reaching its own default branch.
func TestIsAsymmetricPublicKey(t *testing.T) {
	ecKey := mustIDTokenKey(t)
	tests := []struct {
		name string
		key  any
		want bool
	}{
		{name: "ECDSA public key", key: ecKey.Public(), want: true},
		{name: "a byte slice, not a recognised public key type", key: []byte("not-a-key"), want: false},
		{name: "a string", key: "not-a-key-either", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAsymmetricPublicKey(tt.key); got != tt.want {
				t.Errorf("isAsymmetricPublicKey(%T) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}
