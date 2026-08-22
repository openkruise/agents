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
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"

	jose "github.com/go-jose/go-jose/v4"
)

// LoadSigningKey parses a PEM-encoded private key and derives its key ID from
// the public key, so every replica that loads the same key material agrees on
// the same kid without coordinating. Callers holding a Kubernetes Secret pass
// the relevant data value straight in.
func LoadSigningKey(pemData []byte) (SigningKey, error) {
	privateKey, err := ParsePrivateKeyPEM(pemData)
	if err != nil {
		return SigningKey{}, err
	}
	keyID, err := ThumbprintKeyID(privateKey.Public())
	if err != nil {
		return SigningKey{}, err
	}
	return SigningKey{KeyID: keyID, PrivateKey: privateKey}, nil
}

// LoadSigningKeyFromFile reads a PEM-encoded private key from disk, for the
// common case of a Secret projected into the pod as a volume.
func LoadSigningKeyFromFile(path string) (SigningKey, error) {
	pemData, err := os.ReadFile(path)
	if err != nil {
		return SigningKey{}, fmt.Errorf("read signing key: %w", err)
	}
	return LoadSigningKey(pemData)
}

// ParsePrivateKeyPEM decodes the first PEM block and accepts the three private
// key encodings cert tooling emits: PKCS#8, PKCS#1 and SEC1.
//
// Errors deliberately carry no key material, since anything returned here can
// end up in a log line.
func ParsePrivateKeyPEM(pemData []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in signing key")
	}

	parsed, err := parsePrivateKeyDER(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse signing key: %w", err)
	}

	signer, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("signing key of type %T cannot sign", parsed)
	}
	// Reject key types the verifier could never accept, at load time rather
	// than at the first issuance.
	if _, err := signatureAlgorithmFor(signer.Public()); err != nil {
		return nil, fmt.Errorf("signing key: %w", err)
	}
	return signer, nil
}

func parsePrivateKeyDER(der []byte) (any, error) {
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(der); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("not a PKCS#8, PKCS#1 or SEC1 private key")
}

// ThumbprintKeyID returns the RFC 7638 JWK thumbprint of a public key, base64url
// encoded. It is a pure function of the key, which gives two useful properties:
// replicas sharing one Secret publish one kid, and rotating to new key material
// necessarily produces a new kid rather than silently reusing the old one.
func ThumbprintKeyID(publicKey crypto.PublicKey) (string, error) {
	switch publicKey.(type) {
	case *rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey:
	default:
		return "", fmt.Errorf("unsupported key type %T", publicKey)
	}

	thumbprint, err := (&jose.JSONWebKey{Key: publicKey}).Thumbprint(crypto.SHA256)
	if err != nil {
		return "", fmt.Errorf("compute key thumbprint: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(thumbprint), nil
}
