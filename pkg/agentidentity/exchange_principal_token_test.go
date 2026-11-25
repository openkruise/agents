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
	"github.com/openkruise/agents/pkg/agentidentity/authorization"
	"github.com/openkruise/agents/pkg/identity/aead"
	"github.com/openkruise/agents/pkg/identity/jwtissuer"
)

const (
	exchangeTestIdentity   = "checkout-agent"
	exchangeTestSandboxUID = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"
	exchangeTestIDPIssuer  = "https://idp.example.com"
	exchangeTestAudience   = "checkout-agent"
	exchangeTestSubject    = "alice@example.com"
)

// readyIdentity is identity() with a Ready condition set, the shape
// isAgentIdentityReady requires: ObservedGeneration matching Generation, and
// a Ready=True condition whose own ObservedGeneration matches too.
func readyIdentity(name string, refNames ...string) *securityv1alpha1.AgentIdentity {
	id := identity(name, 1, refNames...)
	id.Status.ObservedGeneration = 1
	id.Status.Conditions = []metav1.Condition{{
		Type:               securityv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Test",
		ObservedGeneration: 1,
		LastTransitionTime: metav1.Now(),
	}}
	return id
}

// notReadyIdentity is readyIdentity with the Ready condition left False, the
// shape an AgentIdentity has while a reference has not yet resolved.
func notReadyIdentity(name string, refNames ...string) *securityv1alpha1.AgentIdentity {
	id := readyIdentity(name, refNames...)
	id.Status.Conditions[0].Status = metav1.ConditionFalse
	return id
}

func agentRole(name string, rules ...securityv1alpha1.AgentRoleRule) *securityv1alpha1.AgentRole {
	return &securityv1alpha1.AgentRole{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       securityv1alpha1.AgentRoleSpec{Rules: rules},
	}
}

func agentRoleRule(action string, resources ...string) securityv1alpha1.AgentRoleRule {
	return securityv1alpha1.AgentRoleRule{Actions: []string{action}, Resources: resources}
}

func agentRoleBinding(name, roleName, identityName string) *securityv1alpha1.AgentRoleBinding {
	return &securityv1alpha1.AgentRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: securityv1alpha1.AgentRoleBindingSpec{
			AgentRoleRef: securityv1alpha1.AgentRoleReference{
				APIGroup: "security.agents.kruise.io", Kind: "AgentRole", Name: roleName,
			},
			AgentIdentityRef: securityv1alpha1.AgentIdentityReference{Name: identityName},
		},
	}
}

// indexedClientBuilder is a fake client builder carrying the AgentRoleBinding
// field index the Authorizer lists by, the index AddIssuer registers on the
// manager's cache.
func indexedClientBuilder() *fake.ClientBuilder {
	return fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&securityv1alpha1.AgentRoleBinding{}, authorization.IndexFieldAgentIdentityRef, authorization.AgentIdentityRefIndexFunc)
}

// userInfoKeyringForTest builds a fresh AEAD keyring for sealing a principal
// token's userinfo claim, the same way principalIssuer does in the jwtissuer
// package's own tests.
func userInfoKeyringForTest(t *testing.T) *aead.Keyring {
	t.Helper()
	secret := make([]byte, aead.KeySize)
	if _, err := rand.Read(secret); err != nil {
		t.Fatalf("generate userinfo key: %v", err)
	}
	keyring, err := aead.NewKeyring(aead.Key{ID: "userinfo-1", Secret: secret})
	if err != nil {
		t.Fatalf("build userinfo keyring: %v", err)
	}
	return keyring
}

// exchangeFixture is a handler wired to a real Issuer and a real (fake-client
// backed) cluster, plus the inputs a request needs: a genuine agent token and
// a genuine, independently-signed ID token served from a real JWKS endpoint.
type exchangeFixture struct {
	handler       *exchangePrincipalTokenHandler
	issuer        *jwtissuer.Issuer
	userInfoKeys  *aead.Keyring
	rawAgentToken string
	rawIDToken    string
}

// newExchangeFixture builds the fixture. objects are added to the cluster
// alongside the AgentIdentity and AgentAuthenticationConfig every case needs;
// callers add an AgentRole/AgentRoleBinding to grant access, or leave it out
// to exercise a denial.
func newExchangeFixture(t *testing.T, identityObj *securityv1alpha1.AgentIdentity, extra ...client.Object) *exchangeFixture {
	t.Helper()

	signingKey := mustIDTokenKey(t)
	userInfoKeys := userInfoKeyringForTest(t)
	issuer, err := jwtissuer.New("https://issuer.example",
		jwtissuer.SigningKey{KeyID: "active", PrivateKey: signingKey},
		jwtissuer.WithUserInfoKeyring(userInfoKeys))
	if err != nil {
		t.Fatalf("build issuer: %v", err)
	}

	idpKey := mustIDTokenKey(t)
	jwksServer := startJWKSServer(t, publicJWKFor("idp-key-1", idpKey.Public()))
	now := time.Now()
	rawIDToken := signIDToken(t, idpKey, "idp-key-1", jwt.Claims{
		Issuer:    exchangeTestIDPIssuer,
		Subject:   exchangeTestSubject,
		Audience:  jwt.Audience{exchangeTestAudience},
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now),
		Expiry:    jwt.NewNumericDate(now.Add(time.Hour)),
	})

	objects := append([]client.Object{
		identityObj,
		resolvedAuthConfig("idp", exchangeTestIDPIssuer, jwksServer.URL, exchangeTestAudience),
	}, extra...)
	fc := indexedClientBuilder().WithObjects(objects...).Build()

	rawAgentToken, _, err := issuer.IssueAgentToken(jwtissuer.AgentBinding{
		Namespace:         testNamespace,
		AgentIdentityName: exchangeTestIdentity,
		SandboxUID:        exchangeTestSandboxUID,
	}, 0)
	if err != nil {
		t.Fatalf("issue agent token: %v", err)
	}

	return &exchangeFixture{
		handler:       newExchangePrincipalTokenHandler(fc, issuer),
		issuer:        issuer,
		userInfoKeys:  userInfoKeys,
		rawAgentToken: rawAgentToken,
		rawIDToken:    rawIDToken,
	}
}

// newExchangeFixtureWithInterceptor is newExchangeFixture plus the ability to
// fail a specific read on the underlying fake client, for exercising the
// handler's own internal-error paths (a read failure that is not a plain
// absence) rather than its ordinary denial paths.
func newExchangeFixtureWithInterceptor(t *testing.T, identityObj *securityv1alpha1.AgentIdentity,
	funcs interceptor.Funcs, extra ...client.Object) *exchangeFixture {
	t.Helper()

	signingKey := mustIDTokenKey(t)
	userInfoKeys := userInfoKeyringForTest(t)
	issuer, err := jwtissuer.New("https://issuer.example",
		jwtissuer.SigningKey{KeyID: "active", PrivateKey: signingKey},
		jwtissuer.WithUserInfoKeyring(userInfoKeys))
	if err != nil {
		t.Fatalf("build issuer: %v", err)
	}

	idpKey := mustIDTokenKey(t)
	jwksServer := startJWKSServer(t, publicJWKFor("idp-key-1", idpKey.Public()))
	now := time.Now()
	rawIDToken := signIDToken(t, idpKey, "idp-key-1", jwt.Claims{
		Issuer:    exchangeTestIDPIssuer,
		Subject:   exchangeTestSubject,
		Audience:  jwt.Audience{exchangeTestAudience},
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now),
		Expiry:    jwt.NewNumericDate(now.Add(time.Hour)),
	})

	objects := append([]client.Object{
		identityObj,
		resolvedAuthConfig("idp", exchangeTestIDPIssuer, jwksServer.URL, exchangeTestAudience),
	}, extra...)
	fc := indexedClientBuilder().WithObjects(objects...).
		WithInterceptorFuncs(funcs).Build()

	rawAgentToken, _, err := issuer.IssueAgentToken(jwtissuer.AgentBinding{
		Namespace:         testNamespace,
		AgentIdentityName: exchangeTestIdentity,
		SandboxUID:        exchangeTestSandboxUID,
	}, 0)
	if err != nil {
		t.Fatalf("issue agent token: %v", err)
	}

	return &exchangeFixture{
		handler:       newExchangePrincipalTokenHandler(fc, issuer),
		issuer:        issuer,
		userInfoKeys:  userInfoKeys,
		rawAgentToken: rawAgentToken,
		rawIDToken:    rawIDToken,
	}
}

// TestExchangePrincipalTokenInternalFailures covers the handler's three
// genuine 500 paths: a read that fails for a reason other than absence, and
// IssuePrincipalToken itself refusing. These are distinct from
// TestExchangePrincipalTokenFailurePaths, which covers requests the handler
// correctly refuses; these are requests it would otherwise have granted, if
// the surrounding system had not failed around it.
func TestExchangePrincipalTokenInternalFailures(t *testing.T) {
	errBoom := errors.New("boom")
	grantingObjects := []client.Object{
		agentRole("exchange-role", agentRoleRule(securityv1alpha1.ActionExchangePrincipalToken, "*")),
		agentRoleBinding("exchange-binding", "exchange-role", exchangeTestIdentity),
	}

	t.Run("AgentIdentity read fails for a reason other than absence", func(t *testing.T) {
		fixture := newExchangeFixtureWithInterceptor(t, readyIdentity(exchangeTestIdentity, "idp"),
			interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
					obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*securityv1alpha1.AgentIdentity); ok {
						return errBoom
					}
					return c.Get(ctx, key, obj, opts...)
				},
			}, grantingObjects...)

		rec := postExchange(fixture.handler, "Bearer "+fixture.rawAgentToken, idTokenRequestBody(fixture))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status: got %d, want %d; body=%s", rec.Code, http.StatusInternalServerError, rec.Body.String())
		}
		if got := decodeErrorBody(t, rec); got != "internal error" {
			t.Errorf("error: got %q, want %q", got, "internal error")
		}
	})

	t.Run("authorization check itself fails", func(t *testing.T) {
		fixture := newExchangeFixtureWithInterceptor(t, readyIdentity(exchangeTestIdentity, "idp"),
			interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList,
					opts ...client.ListOption) error {
					if _, ok := list.(*securityv1alpha1.AgentRoleBindingList); ok {
						return errBoom
					}
					return c.List(ctx, list, opts...)
				},
			}, grantingObjects...)

		rec := postExchange(fixture.handler, "Bearer "+fixture.rawAgentToken, idTokenRequestBody(fixture))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status: got %d, want %d; body=%s", rec.Code, http.StatusInternalServerError, rec.Body.String())
		}
		if got := decodeErrorBody(t, rec); got != "internal error" {
			t.Errorf("error: got %q, want %q", got, "internal error")
		}
	})

	t.Run("IssuePrincipalToken itself refuses", func(t *testing.T) {
		// A real issuer with no userinfo keyring configured: every other
		// check passes, and IssuePrincipalToken is the one thing left to
		// fail, exactly as it does when AddIssuer is misconfigured in
		// production (see issuer_test.go's own
		// TestAddIssuerWiresTheUserInfoKeyring for that failure in isolation).
		signingKey := mustIDTokenKey(t)
		issuerWithoutKeyring, err := jwtissuer.New("https://issuer.example",
			jwtissuer.SigningKey{KeyID: "active", PrivateKey: signingKey})
		if err != nil {
			t.Fatalf("build issuer: %v", err)
		}
		idpKey := mustIDTokenKey(t)
		jwksServer := startJWKSServer(t, publicJWKFor("idp-key-1", idpKey.Public()))
		now := time.Now()
		rawIDToken := signIDToken(t, idpKey, "idp-key-1", jwt.Claims{
			Issuer: exchangeTestIDPIssuer, Subject: exchangeTestSubject, Audience: jwt.Audience{exchangeTestAudience},
			IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Hour)),
		})
		objects := append([]client.Object{
			readyIdentity(exchangeTestIdentity, "idp"),
			resolvedAuthConfig("idp", exchangeTestIDPIssuer, jwksServer.URL, exchangeTestAudience),
		}, grantingObjects...)
		fc := indexedClientBuilder().WithObjects(objects...).Build()
		rawAgentToken, _, err := issuerWithoutKeyring.IssueAgentToken(jwtissuer.AgentBinding{
			Namespace: testNamespace, AgentIdentityName: exchangeTestIdentity, SandboxUID: exchangeTestSandboxUID,
		}, 0)
		if err != nil {
			t.Fatalf("issue agent token: %v", err)
		}
		handler := newExchangePrincipalTokenHandler(fc, issuerWithoutKeyring)

		body, err := json.Marshal(exchangePrincipalTokenRequestBody{IDToken: rawIDToken})
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		rec := postExchange(handler, "Bearer "+rawAgentToken, string(body))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status: got %d, want %d; body=%s", rec.Code, http.StatusInternalServerError, rec.Body.String())
		}
		if got := decodeErrorBody(t, rec); got != "internal error" {
			t.Errorf("error: got %q, want %q", got, "internal error")
		}
	})
}

func postExchange(handler http.Handler, bearer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, ExchangePrincipalTokenPath, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decodeErrorBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body errorResponseBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body.Error
}

// wirePrincipalClaims mirrors jwtissuer's own (unexported) principal token
// claims shape by its wire names, the same way jwtissuer's test suite defines
// its own copy: a consumer outside that package only ever sees the claim
// names on the wire, never the Go type.
type wirePrincipalClaims struct {
	jwt.Claims
	TokenType string `json:"token_type"`
	UserInfo  string `json:"userinfo"`
	Actor     struct {
		Subject           string `json:"sub"`
		Namespace         string `json:"namespace"`
		AgentIdentityName string `json:"agent_identity_name"`
		SandboxUID        string `json:"sandbox_uid"`
	} `json:"act"`
}

// TestExchangePrincipalTokenSucceeds covers the full happy path: a valid
// agent token and a valid, independently-verified ID token, authorized by a
// real AgentRole/AgentRoleBinding, mint a principal token that itself
// verifies against the issuer's own JWKS and decrypts back to the right
// issuer, subject and ID token.
func TestExchangePrincipalTokenSucceeds(t *testing.T) {
	fixture := newExchangeFixture(t, readyIdentity(exchangeTestIdentity, "idp"),
		agentRole("exchange-role", agentRoleRule(securityv1alpha1.ActionExchangePrincipalToken, "*")),
		agentRoleBinding("exchange-binding", "exchange-role", exchangeTestIdentity))

	body, err := json.Marshal(exchangePrincipalTokenRequestBody{IDToken: fixture.rawIDToken})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	rec := postExchange(fixture.handler, "Bearer "+fixture.rawAgentToken, string(body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var response exchangePrincipalTokenResponseBody
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.PrincipalToken == "" {
		t.Fatal("principal_token is empty")
	}
	if response.ExpiresAt.IsZero() {
		t.Error("expires_at is zero")
	}

	// The token must verify against the issuer's own published key set, the
	// same way a real credential provider would check it, not just decode.
	keySet, err := fixture.issuer.PublicKeySet()
	if err != nil {
		t.Fatalf("PublicKeySet: %v", err)
	}
	parsed, err := jwt.ParseSigned(response.PrincipalToken, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatalf("parse principal token: %v", err)
	}
	var claims wirePrincipalClaims
	if err := parsed.Claims(&keySet, &claims); err != nil {
		t.Fatalf("verify principal token against the issuer's own JWKS: %v", err)
	}
	if claims.TokenType != jwtissuer.PrincipalTokenType {
		t.Errorf("token_type: got %q, want %q", claims.TokenType, jwtissuer.PrincipalTokenType)
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != jwtissuer.PrincipalTokenAudience {
		t.Errorf("aud: got %v, want [%s]", claims.Audience, jwtissuer.PrincipalTokenAudience)
	}
	if claims.Actor.Namespace != testNamespace || claims.Actor.AgentIdentityName != exchangeTestIdentity ||
		claims.Actor.SandboxUID != exchangeTestSandboxUID {
		t.Errorf("act: got %+v, want namespace=%s agentIdentityName=%s sandboxUid=%s",
			claims.Actor, testNamespace, exchangeTestIdentity, exchangeTestSandboxUID)
	}

	// And it must decrypt, through the userinfo keyring alone, back to
	// exactly the user identity and ID token that were presented.
	gotIssuer, gotSubject, gotIDToken, err := jwtissuer.OpenUserInfo(fixture.userInfoKeys, claims.UserInfo, claims.ID)
	if err != nil {
		t.Fatalf("OpenUserInfo: %v", err)
	}
	if gotIssuer != exchangeTestIDPIssuer {
		t.Errorf("userinfo issuer: got %q, want %q", gotIssuer, exchangeTestIDPIssuer)
	}
	if gotSubject != exchangeTestSubject {
		t.Errorf("userinfo subject: got %q, want %q", gotSubject, exchangeTestSubject)
	}
	if gotIDToken != fixture.rawIDToken {
		t.Error("userinfo ID token did not round trip")
	}
}

func TestExchangePrincipalTokenFailurePaths(t *testing.T) {
	grantedIdentity := func() *securityv1alpha1.AgentIdentity { return readyIdentity(exchangeTestIdentity, "idp") }
	grantingObjects := []client.Object{
		agentRole("exchange-role", agentRoleRule(securityv1alpha1.ActionExchangePrincipalToken, "*")),
		agentRoleBinding("exchange-binding", "exchange-role", exchangeTestIdentity),
	}

	tests := []struct {
		name       string
		identity   *securityv1alpha1.AgentIdentity
		extra      []client.Object
		method     string
		bearer     func(fixture *exchangeFixture) string
		body       func(fixture *exchangeFixture) string
		wantStatus int
		wantError  string
	}{
		{
			name:       "missing Authorization header",
			identity:   grantedIdentity(),
			extra:      grantingObjects,
			bearer:     func(*exchangeFixture) string { return "" },
			body:       func(f *exchangeFixture) string { return idTokenRequestBody(f) },
			wantStatus: http.StatusUnauthorized,
			wantError:  "missing or malformed bearer token",
		},
		{
			name:       "malformed Authorization header",
			identity:   grantedIdentity(),
			extra:      grantingObjects,
			bearer:     func(*exchangeFixture) string { return "Basic dXNlcjpwYXNz" },
			body:       func(f *exchangeFixture) string { return idTokenRequestBody(f) },
			wantStatus: http.StatusUnauthorized,
			wantError:  "missing or malformed bearer token",
		},
		{
			name:       "bearer prefix with no token after it",
			identity:   grantedIdentity(),
			extra:      grantingObjects,
			bearer:     func(*exchangeFixture) string { return "Bearer " },
			body:       func(f *exchangeFixture) string { return idTokenRequestBody(f) },
			wantStatus: http.StatusUnauthorized,
			wantError:  "missing or malformed bearer token",
		},
		{
			name:       "bad agent token",
			identity:   grantedIdentity(),
			extra:      grantingObjects,
			bearer:     func(*exchangeFixture) string { return "Bearer not-a-real-token" },
			body:       func(f *exchangeFixture) string { return idTokenRequestBody(f) },
			wantStatus: http.StatusUnauthorized,
			wantError:  "invalid agent token",
		},
		{
			name:     "bad ID token",
			identity: grantedIdentity(),
			extra:    grantingObjects,
			bearer:   func(f *exchangeFixture) string { return "Bearer " + f.rawAgentToken },
			body: func(*exchangeFixture) string {
				body, _ := json.Marshal(exchangePrincipalTokenRequestBody{IDToken: "not-a-real-id-token"})
				return string(body)
			},
			wantStatus: http.StatusUnauthorized,
			wantError:  "invalid ID token",
		},
		{
			name:       "AgentIdentity not found",
			identity:   readyIdentity("a-different-identity", "idp"),
			extra:      grantingObjects,
			bearer:     func(f *exchangeFixture) string { return "Bearer " + f.rawAgentToken },
			body:       func(f *exchangeFixture) string { return idTokenRequestBody(f) },
			wantStatus: http.StatusNotFound,
			wantError:  "agent identity not found",
		},
		{
			name:       "AgentIdentity not Ready",
			identity:   notReadyIdentity(exchangeTestIdentity, "idp"),
			extra:      grantingObjects,
			bearer:     func(f *exchangeFixture) string { return "Bearer " + f.rawAgentToken },
			body:       func(f *exchangeFixture) string { return idTokenRequestBody(f) },
			wantStatus: http.StatusPreconditionFailed,
			wantError:  "agent identity is not ready",
		},
		{
			name:       "authorization denied",
			identity:   grantedIdentity(),
			extra:      nil, // no AgentRole/AgentRoleBinding at all
			bearer:     func(f *exchangeFixture) string { return "Bearer " + f.rawAgentToken },
			body:       func(f *exchangeFixture) string { return idTokenRequestBody(f) },
			wantStatus: http.StatusForbidden,
			wantError:  "not permitted",
		},
		{
			name:       "wrong HTTP method",
			identity:   grantedIdentity(),
			extra:      grantingObjects,
			method:     http.MethodGet,
			bearer:     func(f *exchangeFixture) string { return "Bearer " + f.rawAgentToken },
			body:       func(f *exchangeFixture) string { return idTokenRequestBody(f) },
			wantStatus: http.StatusMethodNotAllowed,
			wantError:  "method not allowed",
		},
		{
			name:     "oversized body",
			identity: grantedIdentity(),
			extra:    grantingObjects,
			bearer:   func(f *exchangeFixture) string { return "Bearer " + f.rawAgentToken },
			body: func(*exchangeFixture) string {
				body, _ := json.Marshal(exchangePrincipalTokenRequestBody{IDToken: strings.Repeat("a", 32<<10)})
				return string(body)
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "request body too large",
		},
		{
			name:     "malformed JSON body",
			identity: grantedIdentity(),
			extra:    grantingObjects,
			bearer:   func(f *exchangeFixture) string { return "Bearer " + f.rawAgentToken },
			body:     func(*exchangeFixture) string { return "{not json" },

			wantStatus: http.StatusBadRequest,
			wantError:  "malformed request body",
		},
		{
			name:       "missing id_token field",
			identity:   grantedIdentity(),
			extra:      grantingObjects,
			bearer:     func(f *exchangeFixture) string { return "Bearer " + f.rawAgentToken },
			body:       func(*exchangeFixture) string { return "{}" },
			wantStatus: http.StatusBadRequest,
			wantError:  "id_token is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newExchangeFixture(t, tt.identity, tt.extra...)
			method := tt.method
			if method == "" {
				method = http.MethodPost
			}

			req := httptest.NewRequest(method, ExchangePrincipalTokenPath, strings.NewReader(tt.body(fixture)))
			if bearer := tt.bearer(fixture); bearer != "" {
				req.Header.Set("Authorization", bearer)
			}
			rec := httptest.NewRecorder()
			fixture.handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if got := decodeErrorBody(t, rec); got != tt.wantError {
				t.Errorf("error: got %q, want %q", got, tt.wantError)
			}
		})
	}
}

func idTokenRequestBody(f *exchangeFixture) string {
	body, _ := json.Marshal(exchangePrincipalTokenRequestBody{IDToken: f.rawIDToken})
	return string(body)
}
