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
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadSigningKeyAcceptsCommonEncodings(t *testing.T) {
	ecKey := mustECDSAKey(t, elliptic.P256())
	rsaKey := mustRSAKey(t)
	edKey := mustEd25519Key(t)

	tests := []struct {
		name string
		pem  []byte
	}{
		{name: "PKCS#8 ECDSA", pem: mustPKCS8PEM(t, ecKey)},
		{name: "PKCS#8 RSA", pem: mustPKCS8PEM(t, rsaKey)},
		{name: "PKCS#8 Ed25519", pem: mustPKCS8PEM(t, edKey)},
		{name: "SEC1 ECDSA", pem: mustSEC1PEM(t, ecKey)},
		{name: "PKCS#1 RSA", pem: mustPKCS1PEM(t, rsaKey)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signing, err := LoadSigningKey(tt.pem)
			require.NoError(t, err)
			assert.NotEmpty(t, signing.KeyID)
			require.NotNil(t, signing.PrivateKey)

			// A loaded key must be immediately usable for issuance.
			issuer, err := New("https://issuer.example", signing)
			require.NoError(t, err)
			rawJWT, _, err := issuer.IssueTrafficAccessToken("sub", testBinding())
			require.NoError(t, err)
			assert.NotEmpty(t, rawJWT)
		})
	}
}

// TestKeyIDIsDerivedFromTheKey covers the property that makes a shared Secret
// workable across replicas: the kid is a function of the key alone, so two
// processes loading the same material agree without coordinating, and different
// material can never collide onto one kid.
func TestKeyIDIsDerivedFromTheKey(t *testing.T) {
	key := mustECDSAKey(t, elliptic.P256())

	fromPKCS8, err := LoadSigningKey(mustPKCS8PEM(t, key))
	require.NoError(t, err)
	fromSEC1, err := LoadSigningKey(mustSEC1PEM(t, key))
	require.NoError(t, err)

	assert.Equal(t, fromPKCS8.KeyID, fromSEC1.KeyID,
		"same key in a different encoding must produce the same kid")

	other, err := LoadSigningKey(mustPKCS8PEM(t, mustECDSAKey(t, elliptic.P256())))
	require.NoError(t, err)
	assert.NotEqual(t, fromPKCS8.KeyID, other.KeyID, "distinct keys must not share a kid")
}

// TestRotatedKeyIsVerifiableEndToEnd loads two keys the way an operator would
// and runs them through the real verifier, so key loading and the rotation
// overlap are proven together rather than separately.
func TestRotatedKeyIsVerifiableEndToEnd(t *testing.T) {
	oldSigning, err := LoadSigningKey(mustPKCS8PEM(t, mustECDSAKey(t, elliptic.P256())))
	require.NoError(t, err)
	newSigning, err := LoadSigningKey(mustPKCS8PEM(t, mustECDSAKey(t, elliptic.P256())))
	require.NoError(t, err)

	issuer, server := startIssuer(t, newSigning,
		WithRetainedKeys(VerificationKey{KeyID: oldSigning.KeyID, PublicKey: oldSigning.PrivateKey.Public()}),
	)
	verifier := newRealVerifier(t, server)

	rawJWT, _, err := issuer.IssueTrafficAccessToken("sub", testBinding())
	require.NoError(t, err)
	_, err = verifier.Verify(rawJWT)
	require.NoError(t, err)
}

func TestLoadSigningKeyRejectsBadInput(t *testing.T) {
	tests := []struct {
		name        string
		pem         []byte
		expectError string
	}{
		{name: "empty input", pem: nil, expectError: "no PEM block"},
		{name: "not PEM", pem: []byte("hello"), expectError: "no PEM block"},
		{
			name:        "PEM with garbage body",
			pem:         pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not DER")}),
			expectError: "parse signing key",
		},
		{
			name:        "public key instead of private",
			pem:         mustPublicKeyPEM(t, mustECDSAKey(t, elliptic.P256()).Public()),
			expectError: "parse signing key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signing, err := LoadSigningKey(tt.pem)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
			assert.Empty(t, signing.KeyID)

			// Whatever went wrong, the message must not carry key material.
			assert.NotContains(t, err.Error(), "BEGIN")
		})
	}
}

func TestLoadSigningKeyFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(path, mustPKCS8PEM(t, mustECDSAKey(t, elliptic.P256())), 0o600))

	signing, err := LoadSigningKeyFromFile(path)
	require.NoError(t, err)
	assert.NotEmpty(t, signing.KeyID)

	_, err = LoadSigningKeyFromFile(filepath.Join(dir, "absent.key"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read signing key")
}

func mustPKCS8PEM(t *testing.T, key crypto.Signer) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func mustSEC1PEM(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

func mustPKCS1PEM(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	der := x509.MarshalPKCS1PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
}

func mustPublicKeyPEM(t *testing.T, key crypto.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// TestRejectsUndersizedRSAKey pins the RFC 7518 section 3.3 floor for RS256.
// Neither go-jose nor the gateway verifier checks the modulus, so an operator
// pointing this at a weak Secret would otherwise get signed tokens that nothing
// in the chain flags.
func TestRejectsUndersizedRSAKey(t *testing.T) {
	tests := []struct {
		name   string
		bits   int
		accept bool
	}{
		{name: "1024 is refused", bits: 1024},
		{name: "2048 is accepted", bits: 2048, accept: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := rsa.GenerateKey(rand.Reader, tt.bits)
			require.NoError(t, err)

			signing, err := LoadSigningKey(mustPKCS8PEM(t, key))
			if tt.accept {
				require.NoError(t, err)
				assert.NotEmpty(t, signing.KeyID)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "at least 2048")
		})
	}
}
