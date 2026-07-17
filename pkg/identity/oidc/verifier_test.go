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

package oidc

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifierVerify(t *testing.T) {
	rsaKey := mustRSAKey(t)
	otherRSAKey := mustRSAKey(t)
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	now := time.Now().Truncate(time.Second)
	issuer := "https://issuer.example"
	validClaims := tokenClaims(now, issuer)

	tests := []struct {
		name         string
		rawJWT       func(*testing.T) string
		maxTokenSize int
		assertions   func(*testing.T, *TrafficAccessTokenClaims)
		expectError  string
	}{
		{
			name: "valid RSA token",
			rawJWT: func(t *testing.T) string {
				claims := cloneClaims(validClaims)
				claims["sandbox"] = map[string]interface{}{
					"sandboxId": "sandbox-1", "sandboxUid": "uid-1", "ignored": "value",
				}
				claims["metadata"] = map[string]interface{}{"tenant": "ignored"}
				claims["pod"] = map[string]interface{}{"name": "ignored"}
				return signToken(t, jose.RS256, rsaKey, "rsa", claims)
			},
			assertions: func(t *testing.T, claims *TrafficAccessTokenClaims) {
				assert.Equal(t, "sandbox-1", claims.Sandbox.SandboxID)
				assert.Equal(t, "uid-1", claims.Sandbox.SandboxUID)
			},
		},
		{
			name: "valid ECDSA token",
			rawJWT: func(t *testing.T) string {
				return signToken(t, jose.ES256, ecdsaKey, "ecdsa", validClaims)
			},
		},
		{
			name: "valid RSA token without JWK algorithm",
			rawJWT: func(t *testing.T) string {
				return signToken(t, jose.RS256, rsaKey, "rsa-no-alg", validClaims)
			},
		},
		{
			name: "audience is ignored",
			rawJWT: func(t *testing.T) string {
				claims := cloneClaims(validClaims)
				claims["aud"] = []string{"unrelated-service"}
				return signToken(t, jose.RS256, rsaKey, "rsa", claims)
			},
		},
		{
			name: "long lifetime is accepted",
			rawJWT: func(t *testing.T) string {
				claims := cloneClaims(validClaims)
				claims["iat"] = now.Add(-365 * 24 * time.Hour).Unix()
				claims["nbf"] = now.Add(-365 * 24 * time.Hour).Unix()
				claims["exp"] = now.Add(365 * 24 * time.Hour).Unix()
				return signToken(t, jose.RS256, rsaKey, "rsa", claims)
			},
		},
		{
			name: "clock skew is applied",
			rawJWT: func(t *testing.T) string {
				claims := cloneClaims(validClaims)
				claims["iat"] = now.Add(30 * time.Second).Unix()
				claims["nbf"] = now.Add(30 * time.Second).Unix()
				claims["exp"] = now.Add(-30 * time.Second).Unix()
				return signToken(t, jose.RS256, rsaKey, "rsa", claims)
			},
		},
		{
			name:        "empty token",
			rawJWT:      func(*testing.T) string { return "" },
			expectError: "must not be empty",
		},
		{
			name:        "malformed token",
			rawJWT:      func(*testing.T) string { return "not-a-jwt" },
			expectError: "parse signed token",
		},
		{
			name: "none algorithm",
			rawJWT: func(*testing.T) string {
				header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"rsa"}`))
				payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"subject"}`))
				return header + "." + payload + "."
			},
			expectError: "parse signed token",
		},
		{
			name: "HMAC algorithm",
			rawJWT: func(t *testing.T) string {
				return signToken(t, jose.HS256, []byte(strings.Repeat("x", 32)), "rsa", validClaims)
			},
			expectError: "parse signed token",
		},
		{
			name: "missing kid",
			rawJWT: func(t *testing.T) string {
				return signToken(t, jose.RS256, rsaKey, "", validClaims)
			},
			expectError: "must contain a kid",
		},
		{
			name: "unknown kid",
			rawJWT: func(t *testing.T) string {
				return signToken(t, jose.RS256, rsaKey, "unknown", validClaims)
			},
			expectError: "unknown kid",
		},
		{
			name: "wrong signature",
			rawJWT: func(t *testing.T) string {
				return signToken(t, jose.RS256, otherRSAKey, "rsa", validClaims)
			},
			expectError: "verify token signature",
		},
		{
			name: "JWK algorithm mismatch",
			rawJWT: func(t *testing.T) string {
				return signToken(t, jose.PS256, rsaKey, "rsa", validClaims)
			},
			expectError: "does not match JWK algorithm",
		},
		{
			name: "key type incompatible with algorithm",
			rawJWT: func(t *testing.T) string {
				return signToken(t, jose.ES256, ecdsaKey, "rsa-no-alg", validClaims)
			},
			expectError: "incompatible with key",
		},
		{
			name: "invalid claim type",
			rawJWT: func(t *testing.T) string {
				claims := cloneClaims(validClaims)
				claims["exp"] = "invalid"
				return signToken(t, jose.RS256, rsaKey, "rsa", claims)
			},
			expectError: "decode claims",
		},
		{
			name: "missing exp",
			rawJWT: func(t *testing.T) string {
				return signToken(t, jose.RS256, rsaKey, "rsa", withoutClaim(validClaims, "exp"))
			},
			expectError: "missing exp",
		},
		{
			name: "missing iat",
			rawJWT: func(t *testing.T) string {
				return signToken(t, jose.RS256, rsaKey, "rsa", withoutClaim(validClaims, "iat"))
			},
			expectError: "missing iat",
		},
		{
			name: "missing nbf",
			rawJWT: func(t *testing.T) string {
				return signToken(t, jose.RS256, rsaKey, "rsa", withoutClaim(validClaims, "nbf"))
			},
			expectError: "missing nbf",
		},
		{
			name: "expired",
			rawJWT: func(t *testing.T) string {
				claims := cloneClaims(validClaims)
				claims["exp"] = now.Add(-2 * time.Minute).Unix()
				return signToken(t, jose.RS256, rsaKey, "rsa", claims)
			},
			expectError: "expired",
		},
		{
			name: "issued in future",
			rawJWT: func(t *testing.T) string {
				claims := cloneClaims(validClaims)
				claims["iat"] = now.Add(2 * time.Minute).Unix()
				return signToken(t, jose.RS256, rsaKey, "rsa", claims)
			},
			expectError: "future",
		},
		{
			name: "not valid yet",
			rawJWT: func(t *testing.T) string {
				claims := cloneClaims(validClaims)
				claims["nbf"] = now.Add(2 * time.Minute).Unix()
				return signToken(t, jose.RS256, rsaKey, "rsa", claims)
			},
			expectError: "not valid yet",
		},
		{
			name: "wrong issuer",
			rawJWT: func(t *testing.T) string {
				claims := cloneClaims(validClaims)
				claims["iss"] = "https://other.example"
				return signToken(t, jose.RS256, rsaKey, "rsa", claims)
			},
			expectError: "does not match expected issuer",
		},
		{
			name: "empty subject",
			rawJWT: func(t *testing.T) string {
				claims := cloneClaims(validClaims)
				claims["sub"] = ""
				return signToken(t, jose.RS256, rsaKey, "rsa", claims)
			},
			expectError: "missing sub",
		},
		{
			name: "oversized token",
			rawJWT: func(t *testing.T) string {
				return signToken(t, jose.RS256, rsaKey, "rsa", validClaims)
			},
			maxTokenSize: 10,
			expectError:  "exceeds maximum size",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			maxTokenSize := DefaultMaxTokenSize
			if tt.maxTokenSize != 0 {
				maxTokenSize = tt.maxTokenSize
			}
			underTest := &verifier{
				issuer: issuer,
				keys: map[string]jose.JSONWebKey{
					"rsa": {
						Key: &rsaKey.PublicKey, KeyID: "rsa", Algorithm: string(jose.RS256), Use: "sig",
					},
					"ecdsa": {
						Key: &ecdsaKey.PublicKey, KeyID: "ecdsa", Algorithm: string(jose.ES256), Use: "sig",
					},
					"rsa-no-alg": {
						Key: &rsaKey.PublicKey, KeyID: "rsa-no-alg", Use: "sig",
					},
				},
				clockSkew:    time.Minute,
				maxTokenSize: maxTokenSize,
			}

			claims, err := underTest.Verify(tt.rawJWT(t))
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, strings.ToLower(err.Error()), strings.ToLower(tt.expectError))
				assert.Nil(t, claims)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, claims)
			if tt.assertions != nil {
				tt.assertions(t, claims)
			}
		})
	}
}

func TestDecodeJWKs(t *testing.T) {
	rsaKey := mustRSAKey(t)
	publicJWK := jose.JSONWebKey{Key: &rsaKey.PublicKey, KeyID: "rsa", Use: "sig", Algorithm: string(jose.RS256)}
	withVerifyOperation := json.RawMessage(strings.TrimSuffix(mustJSON(t, publicJWK), "}") + `,"key_ops":["sign","verify"]}`)

	tests := []struct {
		name        string
		rawKeys     []json.RawMessage
		expectCount int
		expectError string
	}{
		{name: "verify operation permitted", rawKeys: []json.RawMessage{withVerifyOperation}, expectCount: 1},
		{name: "malformed metadata", rawKeys: []json.RawMessage{json.RawMessage(`{`)}, expectError: "decode key 0 metadata"},
		{name: "invalid JWK", rawKeys: []json.RawMessage{json.RawMessage(`{"kty":"unsupported","kid":"bad"}`)}, expectError: "decode key 0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keys, err := decodeJWKs(tt.rawKeys)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			assert.Len(t, keys, tt.expectCount)
		})
	}
}

func TestValidateKeys(t *testing.T) {
	rsaKey := mustRSAKey(t)
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	ed25519PublicKey, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	validRSA := jose.JSONWebKey{Key: &rsaKey.PublicKey, KeyID: "rsa", Use: "sig", Algorithm: string(jose.RS256)}

	tests := []struct {
		name        string
		keys        []jose.JSONWebKey
		expectCount int
		expectError string
	}{
		{name: "RSA and ECDSA public keys", keys: []jose.JSONWebKey{validRSA, {Key: &ecdsaKey.PublicKey, KeyID: "ec"}}, expectCount: 2},
		{name: "Ed25519 public key", keys: []jose.JSONWebKey{{Key: ed25519PublicKey, KeyID: "ed25519", Use: "sig", Algorithm: string(jose.EdDSA)}}, expectCount: 1},
		{name: "empty set", expectError: "at least one key"},
		{name: "invalid key", keys: []jose.JSONWebKey{{KeyID: "bad"}}, expectError: "invalid"},
		{name: "invalid RSA parameters", keys: []jose.JSONWebKey{{Key: &rsa.PublicKey{}, KeyID: "invalid-rsa"}}, expectError: "invalid"},
		{name: "empty kid", keys: []jose.JSONWebKey{{Key: &rsaKey.PublicKey}}, expectError: "empty kid"},
		{name: "encryption use", keys: []jose.JSONWebKey{{Key: &rsaKey.PublicKey, KeyID: "enc", Use: "enc"}}, expectError: "unsupported use"},
		{name: "symmetric key", keys: []jose.JSONWebKey{{Key: []byte(strings.Repeat("x", 32)), KeyID: "symmetric"}}, expectError: "asymmetric public key"},
		{name: "private key", keys: []jose.JSONWebKey{{Key: rsaKey, KeyID: "private"}}, expectError: "asymmetric public key"},
		{name: "duplicate kid", keys: []jose.JSONWebKey{validRSA, {Key: &ecdsaKey.PublicKey, KeyID: "rsa"}}, expectError: "duplicate kid"},
		{name: "incompatible JWK algorithm", keys: []jose.JSONWebKey{{Key: &ecdsaKey.PublicKey, KeyID: "ec", Algorithm: string(jose.RS256)}}, expectError: "incompatible algorithm"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keys, err := validateKeys(tt.keys)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			assert.Len(t, keys, tt.expectCount)
		})
	}
}

func TestAlgorithmSupportsKey(t *testing.T) {
	rsaKey := mustRSAKey(t)
	p384Key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	p521Key, err := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	require.NoError(t, err)
	ed25519PublicKey, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	tests := []struct {
		name      string
		algorithm jose.SignatureAlgorithm
		key       interface{}
		expected  bool
	}{
		{name: "RSA RS384", algorithm: jose.RS384, key: &rsaKey.PublicKey, expected: true},
		{name: "RSA RS512", algorithm: jose.RS512, key: &rsaKey.PublicKey, expected: true},
		{name: "RSA PS256", algorithm: jose.PS256, key: &rsaKey.PublicKey, expected: true},
		{name: "RSA PS384", algorithm: jose.PS384, key: &rsaKey.PublicKey, expected: true},
		{name: "RSA PS512", algorithm: jose.PS512, key: &rsaKey.PublicKey, expected: true},
		{name: "RSA rejects ECDSA", algorithm: jose.ES256, key: &rsaKey.PublicKey},
		{name: "P-384 accepts ES384", algorithm: jose.ES384, key: &p384Key.PublicKey, expected: true},
		{name: "P-384 rejects ES256", algorithm: jose.ES256, key: &p384Key.PublicKey},
		{name: "P-521 accepts ES512", algorithm: jose.ES512, key: &p521Key.PublicKey, expected: true},
		{name: "P-521 rejects ES384", algorithm: jose.ES384, key: &p521Key.PublicKey},
		{name: "unsupported ECDSA curve", algorithm: jose.ES256, key: &ecdsa.PublicKey{Curve: elliptic.P224()}},
		{name: "Ed25519 accepts EdDSA", algorithm: jose.EdDSA, key: ed25519PublicKey, expected: true},
		{name: "Ed25519 rejects RSA", algorithm: jose.RS256, key: ed25519PublicKey},
		{name: "unsupported key type", algorithm: jose.HS256, key: []byte("symmetric")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, algorithmSupportsKey(tt.algorithm, tt.key))
		})
	}
}

func tokenClaims(now time.Time, issuer string) map[string]interface{} {
	return map[string]interface{}{
		"iss": issuer,
		"sub": "subject-1",
		"iat": now.Add(-time.Minute).Unix(),
		"nbf": now.Add(-time.Minute).Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
}

func cloneClaims(input map[string]interface{}) map[string]interface{} {
	output := make(map[string]interface{}, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func withoutClaim(input map[string]interface{}, name string) map[string]interface{} {
	output := cloneClaims(input)
	delete(output, name)
	return output
}

func signToken(t *testing.T, algorithm jose.SignatureAlgorithm, key interface{}, keyID string, claims map[string]interface{}) string {
	t.Helper()
	options := &jose.SignerOptions{}
	if keyID != "" {
		options.WithHeader("kid", keyID)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: algorithm, Key: key}, options)
	require.NoError(t, err)
	rawJWT, err := jwt.Signed(signer).Claims(claims).Serialize()
	require.NoError(t, err)
	return rawJWT
}

func mustRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return key
}

func mustJSON(t *testing.T, value interface{}) string {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return string(data)
}
