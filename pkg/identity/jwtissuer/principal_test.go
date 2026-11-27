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
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/openkruise/agents/pkg/identity/aead"
	"github.com/openkruise/agents/pkg/identity/oidc"
)

// userInfoKeyring is the keyring sealing the userinfo claim. It is separate
// from the signing key on purpose: a verifier holds the public signing key and
// must not thereby be able to read the user identity.
func userInfoKeyring(t *testing.T) *aead.Keyring {
	t.Helper()
	// Fresh material per keyring. A fixed value would make every issuer in
	// these tests share one key, and the case checking that a different
	// keyring cannot read a userinfo blob would be comparing a keyring with
	// itself.
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

func principalIssuer(t *testing.T, opts ...Option) (*Issuer, *aead.Keyring) {
	t.Helper()
	return principalIssuerWithKeyID(t, "principal-1", opts...)
}

func principalIssuerWithKeyID(t *testing.T, keyID string, opts ...Option) (*Issuer, *aead.Keyring) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	// Returned alongside the issuer rather than read back off it, so tests
	// call OpenUserInfo the way a real consumer does: from the keyring alone,
	// never through the Issuer that holds the signing key.
	keys := userInfoKeyring(t)
	opts = append([]Option{WithUserInfoKeyring(keys)}, opts...)
	issuer, err := New("https://identity.example.com",
		SigningKey{KeyID: keyID, PrivateKey: key}, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return issuer, keys
}

// verifiedUser is a plausible already-verified inbound credential.
func verifiedUser() PrincipalBinding {
	const issuer, subject = "https://idp.example.com", "alice@example.com"
	return PrincipalBinding{
		IDToken: fakeIDToken(issuer, subject),
		Issuer:  issuer,
		Subject: subject,
		Agent: AgentBinding{
			Namespace:         "team-a",
			AgentIdentityName: "checkout-agent",
			SandboxUID:        "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
		},
	}
}

// fakeIDToken builds a structurally real, signed JWT carrying iss and sub.
//
// IssuePrincipalToken reads these claims out of the token without verifying
// its signature, since verification is the caller's job before a binding ever
// reaches it, but it still has to parse as a genuine JWS: three base64url
// segments under a signature algorithm this package recognises. A hand-typed
// placeholder string does not satisfy that, so tests sign with a throwaway key
// instead, the same shape a real IdP's token would have.
func fakeIDToken(issuer, subject string) string {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, nil)
	if err != nil {
		panic(err)
	}
	raw, err := jwt.Signed(signer).Claims(jwt.Claims{Issuer: issuer, Subject: subject}).Serialize()
	if err != nil {
		panic(err)
	}
	return raw
}

type principalClaims struct {
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

func parsePrincipalToken(t *testing.T, issuer *Issuer, raw string) principalClaims {
	t.Helper()
	keySet, err := issuer.PublicKeySet()
	if err != nil {
		t.Fatalf("PublicKeySet: %v", err)
	}
	parsed, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatalf("ParseSigned: %v", err)
	}
	var claims principalClaims
	if err := parsed.Claims(&keySet, &claims); err != nil {
		t.Fatalf("verify against the published key set: %v", err)
	}
	return claims
}

// TestIssuePrincipalTokenClaims covers the claim set the credential provider
// reads, under the model where the token's identifier and the user's identity
// are separate things.
func TestIssuePrincipalTokenClaims(t *testing.T) {
	issuer, _ := principalIssuer(t)
	binding := verifiedUser()

	raw, expiry, err := issuer.IssuePrincipalToken(binding, 10*time.Minute)
	if err != nil {
		t.Fatalf("IssuePrincipalToken: %v", err)
	}
	claims := parsePrincipalToken(t, issuer, raw)

	if claims.Issuer != "https://identity.example.com" {
		t.Errorf("iss is %q, want the issuer URL", claims.Issuer)
	}
	if claims.ID == "" {
		t.Error("jti is empty; nothing names this token for revocation")
	}
	if claims.Subject == "" {
		t.Error("sub is empty")
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != PrincipalTokenAudience {
		t.Errorf("aud is %v, want [%s]", claims.Audience, PrincipalTokenAudience)
	}
	if claims.TokenType != PrincipalTokenType {
		t.Errorf("token_type is %q, want %q", claims.TokenType, PrincipalTokenType)
	}

	// act carries the parts separately, so the delegation key can take them
	// without splitting a formatted string.
	if claims.Actor.Namespace != binding.Agent.Namespace {
		t.Errorf("act.namespace is %q, want %q", claims.Actor.Namespace, binding.Agent.Namespace)
	}
	if claims.Actor.AgentIdentityName != binding.Agent.AgentIdentityName {
		t.Errorf("act.agent_identity_name is %q, want %q",
			claims.Actor.AgentIdentityName, binding.Agent.AgentIdentityName)
	}
	if claims.Actor.SandboxUID != binding.Agent.SandboxUID {
		t.Errorf("act.sandbox_uid is %q, want %q", claims.Actor.SandboxUID, binding.Agent.SandboxUID)
	}
	if want := "agent:team-a:checkout-agent"; claims.Actor.Subject != want {
		t.Errorf("act.sub is %q, want %q", claims.Actor.Subject, want)
	}
	if got := claims.Expiry.Time(); !got.Equal(expiry.Truncate(time.Second)) {
		t.Errorf("exp is %s, want %s", got, expiry.Truncate(time.Second))
	}
}

// TestTheTokenIdentifierIsFreshEveryIssuance is the point of separating jti
// from the user identity. Nothing looks a delegation up by it, so it does not
// need to be stable, and a per-token identifier is what a revocation list
// would name.
func TestTheTokenIdentifierIsFreshEveryIssuance(t *testing.T) {
	issuer, _ := principalIssuer(t)
	binding := verifiedUser()

	const issuances = 256
	seenIDs := make(map[string]int, issuances)
	seenSubjects := make(map[string]int, issuances)

	for i := 0; i < issuances; i++ {
		raw, _, err := issuer.IssuePrincipalToken(binding, time.Minute)
		if err != nil {
			t.Fatalf("issuance %d: %v", i, err)
		}
		claims := parsePrincipalToken(t, issuer, raw)
		if previous, repeated := seenIDs[claims.ID]; repeated {
			t.Fatalf("issuance %d repeated the jti from issuance %d", i, previous)
		}
		seenIDs[claims.ID] = i
		if previous, repeated := seenSubjects[claims.Subject]; repeated {
			t.Fatalf("issuance %d repeated the sub from issuance %d", i, previous)
		}
		seenSubjects[claims.Subject] = i
	}
}

// TestUserInfoIsRecoverableButNotPublic is the property the whole adjustment
// exists for: the service can get the real user identity back, and a holder of
// the token cannot.
func TestUserInfoIsRecoverableButNotPublic(t *testing.T) {
	issuer, keys := principalIssuer(t)
	binding := verifiedUser()

	raw, _, err := issuer.IssuePrincipalToken(binding, time.Minute)
	if err != nil {
		t.Fatalf("IssuePrincipalToken: %v", err)
	}
	claims := parsePrincipalToken(t, issuer, raw)

	t.Run("the service recovers the identity", func(t *testing.T) {
		// Called with the keyring alone, the way a real consumer does: the
		// credential provider decrypts userinfo and never touches an Issuer,
		// which is what holds the signing private key.
		gotIssuer, gotSubject, gotToken, err := OpenUserInfo(keys, claims.UserInfo, claims.ID)
		if err != nil {
			t.Fatalf("OpenUserInfo: %v", err)
		}
		if gotIssuer != binding.Issuer {
			t.Errorf("issuer is %q, want %q", gotIssuer, binding.Issuer)
		}
		if gotSubject != binding.Subject {
			t.Errorf("subject is %q, want %q", gotSubject, binding.Subject)
		}
		if gotToken != binding.IDToken {
			t.Errorf("the original ID token did not round trip")
		}
	})

	t.Run("a holder of the token cannot read it", func(t *testing.T) {
		// A JWT is only base64, so anything in the payload is readable by
		// whoever holds the token.
		parts := strings.Split(raw, ".")
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		for _, secret := range []string{binding.Subject, binding.Issuer, binding.IDToken} {
			if strings.Contains(string(payload), secret) {
				t.Errorf("%q is readable in the token payload", secret)
			}
		}
	})

	t.Run("a userinfo blob cannot be moved to another token", func(t *testing.T) {
		// It is sealed under the token's own jti, so pasting it into a
		// different token fails rather than yielding somebody else's identity.
		second, _, err := issuer.IssuePrincipalToken(verifiedUser(), time.Minute)
		if err != nil {
			t.Fatalf("second issuance: %v", err)
		}
		other := parsePrincipalToken(t, issuer, second)

		if _, _, _, err := OpenUserInfo(keys, claims.UserInfo, other.ID); err == nil {
			t.Error("userinfo from one token opened under another token's jti")
		}
	})

	t.Run("a different keyring cannot read it", func(t *testing.T) {
		_, strangerKeys := principalIssuer(t)
		if _, _, _, err := OpenUserInfo(strangerKeys, claims.UserInfo, claims.ID); err == nil {
			t.Error("userinfo opened under a different keyring")
		}
	})
}

// TestIssuanceRefusesWithoutAUserInfoKeyring covers the fail-closed case. A
// token with no userinfo would verify and then be useless, because nothing
// could recover the identity needed to find the user's delegation.
func TestIssuanceRefusesWithoutAUserInfoKeyring(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	issuer, err := New("https://identity.example.com", SigningKey{KeyID: "k", PrivateKey: key})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	raw, _, err := issuer.IssuePrincipalToken(verifiedUser(), time.Minute)
	if err == nil {
		t.Fatalf("a principal token was issued with no userinfo keyring: %s", raw)
	}
	if !strings.Contains(err.Error(), "userinfo keyring") {
		t.Errorf("the refusal does not name the cause: %v", err)
	}
	if raw != "" {
		t.Errorf("a token was returned alongside the error: %s", raw)
	}
}

// TestPrincipalAndAgentTokensAreNotInterchangeable is why both carry a
// token_type and a distinct audience. They may share a signing key, so a
// verifier checking only the signature would accept either wherever it expects
// one, and the two authorise very different things.
func TestPrincipalAndAgentTokensAreNotInterchangeable(t *testing.T) {
	issuer, _ := principalIssuer(t)
	binding := verifiedUser()

	principalRaw, _, err := issuer.IssuePrincipalToken(binding, time.Minute)
	if err != nil {
		t.Fatalf("IssuePrincipalToken: %v", err)
	}
	agentRaw, _, err := issuer.IssueAgentToken(binding.Agent, time.Minute)
	if err != nil {
		t.Fatalf("IssueAgentToken: %v", err)
	}

	principal := parsePrincipalToken(t, issuer, principalRaw)
	agent := parsePrincipalToken(t, issuer, agentRaw)

	if principal.TokenType == agent.TokenType {
		t.Errorf("both tokens carry token_type %q", principal.TokenType)
	}
	if principal.Audience[0] == agent.Audience[0] {
		t.Errorf("both tokens carry audience %q", principal.Audience[0])
	}
	if agent.Actor.Subject != "" {
		t.Errorf("the agent token carries act.sub %q", agent.Actor.Subject)
	}
	if agent.UserInfo != "" {
		t.Errorf("the agent token carries userinfo")
	}
}

// TestIssuePrincipalTokenRejectsIncompleteBindings covers the fields that must
// come from verified input.
func TestIssuePrincipalTokenRejectsIncompleteBindings(t *testing.T) {
	issuer, _ := principalIssuer(t)

	tests := []struct {
		name    string
		mutate  func(binding *PrincipalBinding)
		wantErr string
	}{
		{
			name:    "no ID token",
			mutate:  func(b *PrincipalBinding) { b.IDToken = "" },
			wantErr: "ID token must not be empty",
		},
		{
			name:    "no credential issuer",
			mutate:  func(b *PrincipalBinding) { b.Issuer = "" },
			wantErr: "issuer must not be empty",
		},
		{
			name:    "no credential subject",
			mutate:  func(b *PrincipalBinding) { b.Subject = "" },
			wantErr: "subject must not be empty",
		},
		{
			name:    "no namespace",
			mutate:  func(b *PrincipalBinding) { b.Agent.Namespace = "" },
			wantErr: "namespace must not be empty",
		},
		{
			name:    "no agent identity",
			mutate:  func(b *PrincipalBinding) { b.Agent.AgentIdentityName = "" },
			wantErr: "agent identity name must not be empty",
		},
		{
			name:    "no sandbox UID",
			mutate:  func(b *PrincipalBinding) { b.Agent.SandboxUID = "" },
			wantErr: "sandbox UID must not be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binding := verifiedUser()
			tt.mutate(&binding)

			raw, _, err := issuer.IssuePrincipalToken(binding, time.Minute)
			if err == nil {
				t.Fatalf("issued a token from an incomplete binding: %s", raw)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("got %v, want an error containing %q", err, tt.wantErr)
			}
			if raw != "" {
				t.Errorf("a token was returned alongside the error: %s", raw)
			}
		})
	}
}

// TestPrincipalTokenLifetimeIsItsOwn covers the separation from the agent
// token's lifetime. The two are different orders of magnitude, so one default
// cannot serve both without each constraining the other.
func TestPrincipalTokenLifetimeIsItsOwn(t *testing.T) {
	t.Run("the default is the principal default, not the shared one", func(t *testing.T) {
		issuer, _ := principalIssuer(t)

		_, expiry, err := issuer.IssuePrincipalToken(verifiedUser(), 0)
		if err != nil {
			t.Fatalf("IssuePrincipalToken: %v", err)
		}
		remaining := time.Until(expiry)
		if remaining < DefaultPrincipalTokenLifetime-time.Minute ||
			remaining > DefaultPrincipalTokenLifetime {
			t.Errorf("expiry is %s away, want close to %s", remaining, DefaultPrincipalTokenLifetime)
		}
		if remaining <= DefaultTokenLifetime {
			t.Errorf("the principal token is using the shared %s default", DefaultTokenLifetime)
		}
	})

	t.Run("configuring one does not move the other", func(t *testing.T) {
		issuer, _ := principalIssuer(t,
			WithTokenLifetime(2*time.Minute),
			WithPrincipalTokenLifetime(5*time.Hour))

		_, agentExpiry, err := issuer.IssueAgentToken(verifiedUser().Agent, 0)
		if err != nil {
			t.Fatalf("IssueAgentToken: %v", err)
		}
		if remaining := time.Until(agentExpiry); remaining > 2*time.Minute {
			t.Errorf("the agent token lasts %s, want the 2m it was configured with", remaining)
		}

		_, principalExpiry, err := issuer.IssuePrincipalToken(verifiedUser(), 0)
		if err != nil {
			t.Fatalf("IssuePrincipalToken: %v", err)
		}
		remaining := time.Until(principalExpiry)
		if remaining < 4*time.Hour || remaining > 5*time.Hour {
			t.Errorf("the principal token lasts %s, want the 5h it was configured with", remaining)
		}
	})

	t.Run("an explicit validity still wins", func(t *testing.T) {
		issuer, _ := principalIssuer(t)
		_, expiry, err := issuer.IssuePrincipalToken(verifiedUser(), 90*time.Second)
		if err != nil {
			t.Fatalf("IssuePrincipalToken: %v", err)
		}
		if remaining := time.Until(expiry); remaining > 90*time.Second {
			t.Errorf("expiry is %s away, want 90s", remaining)
		}
	})
}

// TestNewRejectsANonPositivePrincipalLifetime covers the same bug class as the
// one fixed in principalLifetime never being defaulted, reached through the
// Option instead of a missing default. validity of 0 means "use the default"
// everywhere else in this file, so WithPrincipalTokenLifetime(0) is an easy way
// to ask for that by mistake and get an instantly expired token instead.
func TestNewRejectsANonPositivePrincipalLifetime(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	tests := []struct {
		name     string
		lifetime time.Duration
	}{
		{"zero", 0},
		{"negative", -time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issuer, err := New("https://identity.example.com",
				SigningKey{KeyID: "k", PrivateKey: key},
				WithUserInfoKeyring(userInfoKeyring(t)),
				WithPrincipalTokenLifetime(tt.lifetime))
			if err == nil {
				t.Fatal("New accepted a non-positive principal token lifetime")
			}
			if !strings.Contains(err.Error(), "principal token lifetime") {
				t.Errorf("the refusal does not name the cause: %v", err)
			}
			if issuer != nil {
				t.Error("an issuer was returned alongside the error")
			}
		})
	}
}

// TestIssuePrincipalTokenRejectsAMismatchedIDToken covers the case where
// IDToken, Issuer and Subject were not all read from the same verification.
// The outer signature and the userinfo AEAD both still verify in this case, so
// the check has to happen here or nothing catches it.
func TestIssuePrincipalTokenRejectsAMismatchedIDToken(t *testing.T) {
	issuer, _ := principalIssuer(t)

	tests := []struct {
		name   string
		mutate func(binding *PrincipalBinding)
	}{
		{
			name:   "the binding's issuer does not match the ID token's",
			mutate: func(b *PrincipalBinding) { b.Issuer = "https://a-different-idp.example.com" },
		},
		{
			name:   "the binding's subject does not match the ID token's",
			mutate: func(b *PrincipalBinding) { b.Subject = "someone-else@example.com" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binding := verifiedUser()
			tt.mutate(&binding)

			raw, _, err := issuer.IssuePrincipalToken(binding, time.Minute)
			if err == nil {
				t.Fatalf("issued a token from a binding whose fields disagree: %s", raw)
			}
			if !strings.Contains(err.Error(), "does not match the binding") {
				t.Errorf("the refusal does not name the cause: %v", err)
			}
		})
	}
}

// TestIssuePrincipalTokenRejectsAnUnsignedIDTokenShape covers the other side
// of the consistency check: go-jose refuses to parse an "alg": "none" token
// unless that algorithm is explicitly allowed, and checkIDTokenMatchesBinding
// does not allow it. This is deliberate and worth pinning rather than relaxed
// away. The check reads claims without verifying a signature, which already
// means it trusts the caller to have verified IDToken elsewhere; allowing
// "none" on top of that would mean the one JWT this package touches without a
// key is also the one kind every JOSE library singles out for the classic
// alg-confusion forgery.
func TestIssuePrincipalTokenRejectsAnUnsignedIDTokenShape(t *testing.T) {
	issuer, _ := principalIssuer(t)
	binding := verifiedUser()

	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString(
		[]byte(`{"iss":"https://idp.example.com","sub":"alice@example.com"}`))
	binding.IDToken = header + "." + payload + "."

	if raw, _, err := issuer.IssuePrincipalToken(binding, time.Minute); err == nil {
		t.Fatalf("issued a token from an alg:none ID token: %s", raw)
	}
}

// TestIssuePrincipalTokenAcceptsEveryProviderAlgorithm holds
// idTokenSignatureAlgorithms to oidc.SignatureAlgorithms, the set an ID token
// is verified with before it reaches IssuePrincipalToken. If the two drift, an
// ID token signed with the difference verifies and then fails to mint.
func TestIssuePrincipalTokenAcceptsEveryProviderAlgorithm(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate Ed25519 key: %v", err)
	}
	ecKeys := map[jose.SignatureAlgorithm]elliptic.Curve{
		jose.ES256: elliptic.P256(), jose.ES384: elliptic.P384(), jose.ES512: elliptic.P521(),
	}
	issuer, _ := principalIssuer(t)

	for _, alg := range oidc.SignatureAlgorithms {
		t.Run(string(alg), func(t *testing.T) {
			var key any = rsaKey
			switch {
			case alg == jose.EdDSA:
				key = edKey
			case ecKeys[alg] != nil:
				ecKey, err := ecdsa.GenerateKey(ecKeys[alg], rand.Reader)
				if err != nil {
					t.Fatalf("generate EC key: %v", err)
				}
				key = ecKey
			}
			signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, nil)
			if err != nil {
				t.Fatalf("build signer: %v", err)
			}
			binding := verifiedUser()
			if binding.IDToken, err = jwt.Signed(signer).
				Claims(jwt.Claims{Issuer: binding.Issuer, Subject: binding.Subject}).Serialize(); err != nil {
				t.Fatalf("sign ID token: %v", err)
			}
			if _, _, err := issuer.IssuePrincipalToken(binding, 0); err != nil {
				t.Errorf("an ID token signed with %s was refused: %v", alg, err)
			}
		})
	}
}
