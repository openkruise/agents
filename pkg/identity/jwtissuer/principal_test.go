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
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// principalIssuer builds an Issuer for the principal token tests.
func principalIssuer(t *testing.T) *Issuer {
	t.Helper()
	return principalIssuerWithKeyID(t, "principal-1")
}

// principalIssuerWithKeyID builds an Issuer under a named key, so a test
// needing two distinguishable keys can have them.
func principalIssuerWithKeyID(t *testing.T, keyID string) *Issuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	issuer, err := New("https://identity.example.com",
		SigningKey{KeyID: keyID, PrivateKey: key})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return issuer
}

// verifiedUser is a plausible verified inbound credential.
func verifiedUser() PrincipalBinding {
	return PrincipalBinding{
		Issuer:  "https://idp.example.com",
		Subject: "alice@example.com",
		Agent: AgentBinding{
			Namespace:         "team-a",
			AgentIdentityName: "checkout-agent",
			SandboxUID:        "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
		},
	}
}

// principalClaims is the full claim set of an issued principal token.
type principalClaims struct {
	jwt.Claims
	TokenType string `json:"token_type"`
	Actor     struct {
		Subject    string `json:"sub"`
		SandboxUID string `json:"sandbox_uid"`
	} `json:"act"`
}

// parsePrincipalToken verifies a token against the issuer public key set and
// returns its claims, which is what any real consumer would do.
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

// TestIssuePrincipalTokenClaims covers the claim set a credential provider
// reads. The subject is the user and the agent is in act, which is the shape
// RFC 8693 section 4.1 exists for and the reason neither half alone authorises
// anything.
func TestIssuePrincipalTokenClaims(t *testing.T) {
	issuer := principalIssuer(t)
	binding := verifiedUser()

	raw, expiry, err := issuer.IssuePrincipalToken(binding, 10*time.Minute)
	if err != nil {
		t.Fatalf("IssuePrincipalToken: %v", err)
	}
	claims := parsePrincipalToken(t, issuer, raw)

	if claims.Issuer != "https://identity.example.com" {
		t.Errorf("iss is %q, want the issuer URL", claims.Issuer)
	}
	if want := PrincipalID(binding.Issuer, binding.Subject); claims.Subject != want {
		t.Errorf("sub is %q, want the derived principal ID %q", claims.Subject, want)
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != PrincipalTokenAudience {
		t.Errorf("aud is %v, want [%s]", claims.Audience, PrincipalTokenAudience)
	}
	if claims.TokenType != PrincipalTokenType {
		t.Errorf("token_type is %q, want %q", claims.TokenType, PrincipalTokenType)
	}
	if want := "agent:team-a:checkout-agent"; claims.Actor.Subject != want {
		t.Errorf("act.sub is %q, want %q", claims.Actor.Subject, want)
	}
	if claims.Actor.SandboxUID != binding.Agent.SandboxUID {
		t.Errorf("act.sandbox_uid is %q, want %q", claims.Actor.SandboxUID, binding.Agent.SandboxUID)
	}
	if got := claims.Expiry.Time(); !got.Equal(expiry.Truncate(time.Second)) {
		t.Errorf("exp is %s, want %s", got, expiry.Truncate(time.Second))
	}
	if claims.NotBefore.Time().After(claims.IssuedAt.Time()) {
		t.Error("nbf is after iat, so a token is not valid when it is issued")
	}
}

// TestPrincipalTokenDoesNotCarryTheRawUser is the point of deriving sub. The
// raw subject reaches storage keys, logs and audit records through this claim,
// so it must not appear anywhere in the token.
func TestPrincipalTokenDoesNotCarryTheRawUser(t *testing.T) {
	issuer := principalIssuer(t)
	binding := verifiedUser()

	raw, _, err := issuer.IssuePrincipalToken(binding, time.Minute)
	if err != nil {
		t.Fatalf("IssuePrincipalToken: %v", err)
	}

	// A JWT is only base64, so the decoded payload is what a reader sees.
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}

	if strings.Contains(string(payload), binding.Subject) {
		t.Errorf("the raw subject %q appears in the payload: %s", binding.Subject, payload)
	}
	// The issuer of the user credential is part of the identity too, and is
	// equally not something to republish.
	if strings.Contains(string(payload), binding.Issuer) {
		t.Errorf("the credential issuer %q appears in the payload: %s", binding.Issuer, payload)
	}
}

// TestPrincipalIDIsStableAndCollisionResistant covers the two properties a
// delegation lookup depends on. Stability because a delegation stored against
// this value has to be found again, including after a key rotation. Distinctness
// because a collision merges the delegations of two different users.
func TestPrincipalIDIsStableAndCollisionResistant(t *testing.T) {
	t.Run("the same inputs always derive the same ID", func(t *testing.T) {
		first := PrincipalID("https://idp.example.com", "alice@example.com")
		second := PrincipalID("https://idp.example.com", "alice@example.com")
		if first != second {
			t.Errorf("derived %q then %q for the same inputs", first, second)
		}
	})

	t.Run("a rotated signing key does not change the ID", func(t *testing.T) {
		// Derivation does not involve the signing key at all, which is what
		// makes a delegation survive rotation. Asserted through two issuers
		// holding different keys, so a future change that keyed the digest with
		// the signing key would fail here.
		before := principalIssuerWithKeyID(t, "before-rotation")
		after := principalIssuerWithKeyID(t, "after-rotation")
		if before.active.KeyID == after.active.KeyID {
			t.Fatal("the two issuers share a key ID, so this proves nothing")
		}
		binding := verifiedUser()

		firstRaw, _, err := before.IssuePrincipalToken(binding, time.Minute)
		if err != nil {
			t.Fatalf("IssuePrincipalToken: %v", err)
		}
		secondRaw, _, err := after.IssuePrincipalToken(binding, time.Minute)
		if err != nil {
			t.Fatalf("IssuePrincipalToken: %v", err)
		}

		first := parsePrincipalToken(t, before, firstRaw)
		second := parsePrincipalToken(t, after, secondRaw)
		if first.Subject != second.Subject {
			t.Errorf("the principal ID changed across keys: %q then %q",
				first.Subject, second.Subject)
		}
	})

	t.Run("distinct inputs derive distinct IDs", func(t *testing.T) {
		cases := []struct {
			name            string
			issuer, subject string
			otherIssuer     string
			otherSubject    string
		}{
			{
				name:   "a different subject at the same issuer",
				issuer: "https://idp.example.com", subject: "alice@example.com",
				otherIssuer: "https://idp.example.com", otherSubject: "bob@example.com",
			},
			{
				name:   "the same subject at a different issuer",
				issuer: "https://idp.example.com", subject: "alice@example.com",
				otherIssuer: "https://other.example.com", otherSubject: "alice@example.com",
			},
			{
				// The reason the parts are length-prefixed. Concatenated these
				// two pairs are byte-identical, and a collision would merge the
				// delegations of two different users.
				name:   "a boundary shifted between the two parts",
				issuer: "https://a.example", subject: "bc",
				otherIssuer: "https://a.examplb", otherSubject: "c",
			},
			{
				name:   "an empty subject against an empty issuer",
				issuer: "", subject: "ab",
				otherIssuer: "a", otherSubject: "b",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				first := PrincipalID(tc.issuer, tc.subject)
				second := PrincipalID(tc.otherIssuer, tc.otherSubject)
				if first == second {
					t.Errorf("(%q, %q) and (%q, %q) both derived %q",
						tc.issuer, tc.subject, tc.otherIssuer, tc.otherSubject, first)
				}
			})
		}
	})

	t.Run("a trailing slash on the issuer does not change the ID", func(t *testing.T) {
		// The stability half of section 4 normalized issuer. Seeing the same
		// provider written both ways must not cost a user the delegation they
		// already granted.
		bare := PrincipalID("https://kc.example/realms/agents", "alice")
		slashed := PrincipalID("https://kc.example/realms/agents/", "alice")
		if bare != slashed {
			t.Errorf("derived %q without the slash and %q with it", bare, slashed)
		}
	})

	t.Run("nothing beyond the trailing slash is folded", func(t *testing.T) {
		// Folding more would merge the delegations of different users, which is
		// strictly worse than making one user re-consent.
		base := PrincipalID("https://kc.example/realms/agents", "alice")

		cases := []struct {
			name            string
			issuer, subject string
		}{
			{"a different host case", "https://KC.example/realms/agents", "alice"},
			{"a different scheme case", "HTTPS://kc.example/realms/agents", "alice"},
			{"an explicit default port", "https://kc.example:443/realms/agents", "alice"},
			{"a percent-encoded path", "https://kc.example/realms%2Fagents", "alice"},
			{"a different subject case", "https://kc.example/realms/agents", "Alice"},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := PrincipalID(tc.issuer, tc.subject); got == base {
					t.Errorf("(%q, %q) folded into the same ID as the canonical form",
						tc.issuer, tc.subject)
				}
			})
		}
	})

	t.Run("the ID is opaque and bounded", func(t *testing.T) {
		id := PrincipalID("https://idp.example.com", "alice@example.com")
		encoded, found := strings.CutPrefix(id, "principal:")
		if !found {
			t.Fatalf("the ID %q does not carry the principal prefix", id)
		}
		digest, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("the ID is not unpadded base64url: %v", err)
		}
		if len(digest) != 32 {
			t.Errorf("the digest is %d bytes, want 32 for SHA-256", len(digest))
		}
	})
}

// TestPrincipalAndAgentTokensAreNotInterchangeable is why both carry a
// token_type and a distinct audience. They may be signed by the same key, so a
// verifier checking only the signature would accept either wherever it expects
// one, and the two authorise very different things.
func TestPrincipalAndAgentTokensAreNotInterchangeable(t *testing.T) {
	issuer := principalIssuer(t)
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
	// The subjects must differ too: an agent token speaks for the workload, a
	// principal token for the user.
	if principal.Subject == agent.Subject {
		t.Errorf("both tokens carry subject %q", principal.Subject)
	}
	// And the agent token must not carry an actor, since nothing is acting on
	// anyone else behalf in it.
	if agent.Actor.Subject != "" {
		t.Errorf("the agent token carries act.sub %q", agent.Actor.Subject)
	}
}

// TestIssuePrincipalTokenRejectsIncompleteBindings covers the fields that must
// come from verified input. An empty issuer or subject means the caller has no
// verified credential to derive a principal from, and issuing anyway would
// produce a token for the derivation of two empty strings, which every such
// caller would share.
func TestIssuePrincipalTokenRejectsIncompleteBindings(t *testing.T) {
	issuer := principalIssuer(t)

	tests := []struct {
		name    string
		mutate  func(binding *PrincipalBinding)
		wantErr string
	}{
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

// TestIssuePrincipalTokenUsesTheIssuerDefaultLifetime matches IssueAgentToken:
// a validity of zero means the caller has no per-request policy to apply.
func TestIssuePrincipalTokenUsesTheIssuerDefaultLifetime(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	issuer, err := New("https://identity.example.com",
		SigningKey{KeyID: "principal-1", PrivateKey: key},
		WithTokenLifetime(7*time.Minute))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, expiry, err := issuer.IssuePrincipalToken(verifiedUser(), 0)
	if err != nil {
		t.Fatalf("IssuePrincipalToken: %v", err)
	}
	if remaining := time.Until(expiry); remaining > 7*time.Minute || remaining < 6*time.Minute {
		t.Errorf("expiry is %s away, want close to the configured 7m", remaining)
	}
}
