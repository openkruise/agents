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
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
// external IdP's JWKS endpoint. The verifier trusts whatever Status.JWKSURI
// names, which production only ever populates with an HTTPS URL already
// validated by the AgentAuthenticationConfig controller; a plain httptest
// server is enough to exercise verifyIDToken's own logic without also
// standing up TLS trust for a self-signed certificate.
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

func mustRSAIDTokenKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA ID token signing key: %v", err)
	}
	return key
}

// signIDToken signs claims with key under kid, the same shape a real IdP's ID
// token has: a flat, compact JWS.
func signIDToken(t *testing.T, key crypto.Signer, kid string, claims jwt.Claims) string {
	t.Helper()
	return signIDTokenWith(t, jose.ES256, key, kid, claims)
}

// signIDTokenWith is signIDToken with an algorithm other than ES256.
func signIDTokenWith(t *testing.T, alg jose.SignatureAlgorithm, key crypto.Signer, kid string,
	claims jwt.Claims) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: alg, Key: key},
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
	rsaKey := mustRSAIDTokenKey(t)
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
	configAt := func(jwksURL string) []*securityv1alpha1.AgentAuthenticationConfig {
		return []*securityv1alpha1.AgentAuthenticationConfig{resolvedAuthConfig("idp", issuer, jwksURL, audience)}
	}

	tests := []struct {
		name        string
		identity    *securityv1alpha1.AgentIdentity
		configs     []*securityv1alpha1.AgentAuthenticationConfig
		rawIDToken  func() string
		wantClass   error
		expectError string
		wantSubject string
	}{
		{
			name:     "valid",
			identity: identity("checkout-agent", 1, "idp"),
			configs:  configAt(jwksServer.URL),
			rawIDToken: func() string {
				return signIDToken(t, idpKey, "idp-key-1", validClaims())
			},
			wantSubject: subject,
		},
		{
			// The algorithms an IdP may choose are oidc.SignatureAlgorithms, the
			// same set jwtissuer reads the token with when it mints.
			name:     "valid, signed with PS256",
			identity: identity("checkout-agent", 1, "idp"),
			configs: configAt(startJWKSServer(t,
				jose.JSONWebKey{Key: rsaKey.Public(), KeyID: "rsa-1", Use: "sig", Algorithm: string(jose.PS256)}).URL),
			rawIDToken: func() string {
				return signIDTokenWith(t, jose.PS256, rsaKey, "rsa-1", validClaims())
			},
			wantSubject: subject,
		},
		{
			// Keycloak publishes an RSA-OAEP encryption key beside its signing
			// key by default, and that must not cost the whole set.
			name:     "valid against a JWKS that also publishes an encryption key",
			identity: identity("checkout-agent", 1, "idp"),
			configs: configAt(startJWKSServer(t,
				jose.JSONWebKey{Key: mustRSAIDTokenKey(t).Public(), KeyID: "rsa-enc", Use: "enc", Algorithm: "RSA-OAEP"},
				publicJWKFor("idp-key-1", idpKey.Public())).URL),
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
			wantClass:   errIDTokenRejected,
			expectError: "no AgentAuthenticationConfig",
		},
		{
			name:     "audience not in allowed list",
			identity: identity("checkout-agent", 1, "idp"),
			configs:  []*securityv1alpha1.AgentAuthenticationConfig{resolvedAuthConfig("idp", issuer, jwksServer.URL, "a-different-audience")},
			rawIDToken: func() string {
				return signIDToken(t, idpKey, "idp-key-1", validClaims())
			},
			wantClass:   errIDTokenRejected,
			expectError: "does not intersect",
		},
		{
			name:     "expired",
			identity: identity("checkout-agent", 1, "idp"),
			configs:  configAt(jwksServer.URL),
			rawIDToken: func() string {
				claims := validClaims()
				claims.Expiry = jwt.NewNumericDate(time.Now().Add(-2 * time.Hour))
				return signIDToken(t, idpKey, "idp-key-1", claims)
			},
			wantClass:   errIDTokenRejected,
			expectError: "validate ID token claims",
		},
		{
			name:     "no expiry",
			identity: identity("checkout-agent", 1, "idp"),
			configs:  configAt(jwksServer.URL),
			rawIDToken: func() string {
				claims := validClaims()
				claims.Expiry = nil
				return signIDToken(t, idpKey, "idp-key-1", claims)
			},
			wantClass:   errIDTokenRejected,
			expectError: "missing exp claim",
		},
		{
			// Correct kid, but signed by a different private key: the kind of
			// forgery attempt a kid-only check would miss.
			name:     "signature from a different key",
			identity: identity("checkout-agent", 1, "idp"),
			configs:  configAt(jwksServer.URL),
			rawIDToken: func() string {
				return signIDToken(t, otherKey, "idp-key-1", validClaims())
			},
			wantClass:   errIDTokenRejected,
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
			wantClass:   errIDTokenRejected,
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
			configs:  configAt(jwksServer.URL),
			rawIDToken: func() string {
				return signIDToken(t, idpKey, "idp-key-1", validClaims())
			},
			wantSubject: subject,
		},
		{
			// The matching ref resolves to the right issuer but is not Ready:
			// e.g. its spec changed and the controller has not caught up yet.
			// Nothing about the token is known to be wrong.
			name:     "matched config not Ready",
			identity: identity("checkout-agent", 1, "idp"),
			configs: []*securityv1alpha1.AgentAuthenticationConfig{
				staleAuthConfig(resolvedAuthConfig("idp", issuer, jwksServer.URL, audience)),
			},
			rawIDToken: func() string {
				return signIDToken(t, idpKey, "idp-key-1", validClaims())
			},
			wantClass:   errAuthConfigNotReady,
			expectError: "is not Ready",
		},
		{
			name:     "matched config has no jwt block",
			identity: identity("checkout-agent", 1, "idp"),
			configs: func() []*securityv1alpha1.AgentAuthenticationConfig {
				configs := configAt(jwksServer.URL)
				configs[0].Spec.JWT = nil
				return configs
			}(),
			rawIDToken: func() string {
				return signIDToken(t, idpKey, "idp-key-1", validClaims())
			},
			wantClass:   errAuthConfigNotReady,
			expectError: "no jwt configuration",
		},
		{
			name:     "ID token header carries no kid",
			identity: identity("checkout-agent", 1, "idp"),
			configs:  configAt(jwksServer.URL),
			rawIDToken: func() string {
				return signIDToken(t, idpKey, "", validClaims())
			},
			wantClass:   errIDTokenRejected,
			expectError: "must contain a kid",
		},
		{
			name:     "ID token kid matches no published key",
			identity: identity("checkout-agent", 1, "idp"),
			configs:  configAt(jwksServer.URL),
			rawIDToken: func() string {
				return signIDToken(t, idpKey, "a-kid-this-jwks-never-published", validClaims())
			},
			wantClass:   errIDTokenRejected,
			expectError: "unknown kid",
		},
		{
			// The JWKS entry's own declared Algorithm disagrees with the alg
			// the token header actually carries - the external IdP's JWKS is
			// untrusted input, so this has to be checked explicitly, unlike
			// the issuer's own JWKS in jwtissuer.VerifyAgentToken, which is
			// always internally consistent by construction.
			name:     "token algorithm does not match the published key's declared algorithm",
			identity: identity("checkout-agent", 1, "idp"),
			configs: configAt(startJWKSServer(t,
				jose.JSONWebKey{Key: rsaKey.Public(), KeyID: "rsa-1", Use: "sig", Algorithm: string(jose.RS256)}).URL),
			rawIDToken: func() string {
				return signIDTokenWith(t, jose.PS256, rsaKey, "rsa-1", validClaims())
			},
			wantClass:   errIDTokenRejected,
			expectError: "does not match JWK algorithm",
		},
		{
			// A key declaring an algorithm its own type cannot use is the IdP's
			// misconfiguration, so the set is refused whole and nothing about
			// the token itself is held against it.
			name:     "a published key declaring an algorithm its type cannot use",
			identity: identity("checkout-agent", 1, "idp"),
			configs: configAt(startJWKSServer(t,
				jose.JSONWebKey{Key: idpKey.Public(), KeyID: "idp-key-1", Use: "sig", Algorithm: string(jose.RS256)}).URL),
			rawIDToken: func() string {
				return signIDToken(t, idpKey, "idp-key-1", validClaims())
			},
			wantClass:   errIdentityProviderUnavailable,
			expectError: "incompatible algorithm",
		},
		{
			name:     "ID token has no sub claim",
			identity: identity("checkout-agent", 1, "idp"),
			configs:  configAt(jwksServer.URL),
			rawIDToken: func() string {
				claims := validClaims()
				claims.Subject = ""
				return signIDToken(t, idpKey, "idp-key-1", claims)
			},
			wantClass:   errIDTokenRejected,
			expectError: "missing sub claim",
		},
		{
			name:     "the matched config's JWKS endpoint itself fails",
			identity: identity("checkout-agent", 1, "idp"),
			configs:  configAt(failingJWKSServer(t).URL),
			rawIDToken: func() string {
				return signIDToken(t, idpKey, "idp-key-1", validClaims())
			},
			wantClass:   errIdentityProviderUnavailable,
			expectError: "unexpected HTTP status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := make([]client.Object, 0, len(tt.configs))
			for _, config := range tt.configs {
				objects = append(objects, config)
			}
			fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

			verified, err := verifyIDToken(context.Background(), fc, newJWKSCache(), testNamespace,
				tt.identity, tt.rawIDToken())
			if tt.expectError != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got none", tt.expectError)
				}
				if !errors.Is(err, tt.wantClass) {
					t.Errorf("error %q is not classed as %q", err, tt.wantClass)
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

// jwksEndpoint is a JWKS server whose answer a test can change, and which
// counts what it is asked, standing in for an IdP whose keys rotate.
type jwksEndpoint struct {
	server   *httptest.Server
	requests atomic.Int32
	delay    time.Duration

	mu     sync.Mutex
	keys   []jose.JSONWebKey
	status int
}

func newJWKSEndpoint(t *testing.T, keys ...jose.JSONWebKey) *jwksEndpoint {
	t.Helper()
	e := &jwksEndpoint{keys: keys, status: http.StatusOK}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.requests.Add(1)
		time.Sleep(e.delay)
		e.mu.Lock()
		keys, status := e.keys, e.status
		e.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: keys})
	}))
	t.Cleanup(e.server.Close)
	return e
}

func (e *jwksEndpoint) publish(status int, keys ...jose.JSONWebKey) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.status, e.keys = status, keys
}

// age moves the cached copy of uri's set back in time, as if it had been
// fetched that long ago.
func (c *jwksCache) age(uri string, by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[uri]
	entry.fetchedAt = entry.fetchedAt.Add(-by)
	c.entries[uri] = entry
}

// TestJWKSCache covers the cache verifyIDToken reads keys through: a set is
// reused until it expires, an unknown kid refetches it early but not more
// than once per jwksRefetchInterval, a failure is never cached, and a
// redirect is never followed.
func TestJWKSCache(t *testing.T) {
	first := publicJWKFor("key-1", mustIDTokenKey(t).Public())
	rotated := publicJWKFor("key-2", mustIDTokenKey(t).Public())
	header := func(kid string) jose.Header {
		return jose.Header{KeyID: kid, Algorithm: string(jose.ES256)}
	}

	tests := []struct {
		name string
		// steps run in order against one cache and one endpoint. Each names
		// the kid it asks for, what to do first, and what it expects.
		steps        []cacheStep
		wantRequests int32
	}{
		{
			name: "a fresh set is reused",
			steps: []cacheStep{
				{kid: "key-1"},
				{kid: "key-1"},
			},
			wantRequests: 1,
		},
		{
			name: "an expired set is fetched again",
			steps: []cacheStep{
				{kid: "key-1"},
				{kid: "key-1", before: func(c *jwksCache, _ *jwksEndpoint, uri string) { c.age(uri, jwksTTL) }},
			},
			wantRequests: 2,
		},
		{
			name: "an unknown kid finds a rotated key once the set is old enough",
			steps: []cacheStep{
				{kid: "key-1"},
				{kid: "key-2", before: func(c *jwksCache, e *jwksEndpoint, uri string) {
					e.publish(http.StatusOK, first, rotated)
					c.age(uri, jwksRefetchInterval)
				}},
			},
			wantRequests: 2,
		},
		{
			name: "an unknown kid does not refetch within the refetch interval",
			steps: []cacheStep{
				{kid: "key-1"},
				{kid: "key-2", before: func(_ *jwksCache, e *jwksEndpoint, _ string) {
					e.publish(http.StatusOK, first, rotated)
				}, wantClass: errIDTokenRejected},
			},
			wantRequests: 1,
		},
		{
			name: "a kid the refetched set still lacks is rejected",
			steps: []cacheStep{
				{kid: "key-1"},
				{kid: "never-published", before: func(c *jwksCache, _ *jwksEndpoint, uri string) {
					c.age(uri, jwksRefetchInterval)
				}, wantClass: errIDTokenRejected},
			},
			wantRequests: 2,
		},
		{
			name: "a failed fetch is unavailable and not cached",
			steps: []cacheStep{
				{kid: "key-1", before: func(_ *jwksCache, e *jwksEndpoint, _ string) {
					e.publish(http.StatusBadGateway)
				}, wantClass: errIdentityProviderUnavailable},
				{kid: "key-1", before: func(_ *jwksCache, e *jwksEndpoint, _ string) {
					e.publish(http.StatusOK, first)
				}},
			},
			wantRequests: 2,
		},
		{
			name: "a failed early refetch is unavailable",
			steps: []cacheStep{
				{kid: "key-1"},
				{kid: "key-2", before: func(c *jwksCache, e *jwksEndpoint, uri string) {
					e.publish(http.StatusServiceUnavailable)
					c.age(uri, jwksRefetchInterval)
				}, wantClass: errIdentityProviderUnavailable},
			},
			wantRequests: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint := newJWKSEndpoint(t, first)
			cache := newJWKSCache()
			for i, step := range tt.steps {
				if step.before != nil {
					step.before(cache, endpoint, endpoint.server.URL)
				}
				key, err := cache.keyFor(context.Background(), endpoint.server.URL, header(step.kid))
				if step.wantClass != nil {
					if !errors.Is(err, step.wantClass) {
						t.Fatalf("step %d: error %v, want it classed as %v", i, err, step.wantClass)
					}
					continue
				}
				if err != nil {
					t.Fatalf("step %d: keyFor: %v", i, err)
				}
				if key.KeyID != step.kid {
					t.Errorf("step %d: got key %q, want %q", i, key.KeyID, step.kid)
				}
			}
			if got := endpoint.requests.Load(); got != tt.wantRequests {
				t.Errorf("JWKS requests: got %d, want %d", got, tt.wantRequests)
			}
		})
	}
}

type cacheStep struct {
	kid       string
	before    func(c *jwksCache, e *jwksEndpoint, uri string)
	wantClass error
}

// TestJWKSCacheRefusesRedirects covers why the cache has its own client: an
// HTTPS JWKS URI says nothing about where a redirect leads, including back to
// plaintext, where the keys could be swapped.
func TestJWKSCacheRefusesRedirects(t *testing.T) {
	elsewhere := newJWKSEndpoint(t, publicJWKFor("key-1", mustIDTokenKey(t).Public()))
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.server.URL, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	_, err := newJWKSCache().keyFor(context.Background(), redirector.URL,
		jose.Header{KeyID: "key-1", Algorithm: string(jose.ES256)})
	if !errors.Is(err, errIdentityProviderUnavailable) {
		t.Fatalf("error: got %v, want the redirect refused as unavailable", err)
	}
	if got := elsewhere.requests.Load(); got != 0 {
		t.Errorf("the redirect target received %d requests, want 0", got)
	}
}

// TestJWKSCacheSharesOneFetch covers concurrent exchanges for one issuer:
// they wait on a single request rather than each sending their own.
func TestJWKSCacheSharesOneFetch(t *testing.T) {
	endpoint := newJWKSEndpoint(t, publicJWKFor("key-1", mustIDTokenKey(t).Public()))
	endpoint.delay = 100 * time.Millisecond
	cache := newJWKSCache()

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := cache.keyFor(context.Background(), endpoint.server.URL,
				jose.Header{KeyID: "key-1", Algorithm: string(jose.ES256)})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("keyFor: %v", err)
		}
	}
	if got := endpoint.requests.Load(); got != 1 {
		t.Errorf("JWKS requests: got %d, want 1", got)
	}
}

// TestJWKSCacheStopsWaitingWithItsCaller covers a caller whose own context
// ends while the fetch it is waiting on is still in flight.
func TestJWKSCacheStopsWaitingWithItsCaller(t *testing.T) {
	endpoint := newJWKSEndpoint(t, publicJWKFor("key-1", mustIDTokenKey(t).Public()))
	endpoint.delay = 300 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := newJWKSCache().keyFor(ctx, endpoint.server.URL, jose.Header{KeyID: "key-1", Algorithm: string(jose.ES256)})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error: got %v, want the caller's deadline", err)
	}
}

func TestVerifyIDTokenRejectsUnparsableInput(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	_, err := verifyIDToken(context.Background(), fc, newJWKSCache(), testNamespace,
		identity("checkout-agent", 1, "idp"), "not-a-jwt")
	if !errors.Is(err, errIDTokenRejected) {
		t.Fatalf("error: got %v, want it classed as rejected", err)
	}
	if !strings.Contains(err.Error(), "parse ID token") {
		t.Errorf("error: got %q, want it to mention parsing", err)
	}
}

// TestUnverifiedIDTokenIssuer covers the pre-check directly.
//
// The "read ID token claims" branch, a syntactically valid JWS whose payload
// parses but is not a JSON object, has no test here: go-jose's own jwt.Signed
// refuses at construction time to produce one ("expected claims to be value
// convertible into JSON object"), so no token built through the library this
// codebase signs with, or that any other conformant JWT library would
// produce, can reach it.
func TestUnverifiedIDTokenIssuer(t *testing.T) {
	raw := signIDToken(t, mustIDTokenKey(t), "k1", jwt.Claims{Subject: "alice"})
	token, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = unverifiedIDTokenIssuer(token)
	if !errors.Is(err, errIDTokenRejected) || !strings.Contains(err.Error(), "missing iss claim") {
		t.Errorf("error: got %v, want the missing iss claim rejected", err)
	}
}

// TestAuthConfigForIssuerSurfacesReadFailures covers the one branch
// TestVerifyIDToken's table cannot reach without a client that fails on
// demand: a Get that fails for a reason other than the config simply not
// existing, which must be surfaced rather than silently skipped the way a
// genuine absence is, and as an internal error rather than one about the
// token.
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
	if !errors.Is(err, errBoom) {
		t.Fatalf("error: got %v, want the read failure surfaced", err)
	}
	for _, class := range []error{errIDTokenRejected, errAuthConfigNotReady, errIdentityProviderUnavailable} {
		if errors.Is(err, class) {
			t.Errorf("a read failure is classed as %q, want it left an internal error", class)
		}
	}
}
