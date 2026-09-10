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

// Package jwtissuer mints the signed traffic access tokens that
// pkg/identity/oidc verifies, and publishes the public keys needed to verify
// them. It is the issuing half of the contract described in
// docs/proposals/20260713-traffic-access-token-jwt-verification.md.
package jwtissuer

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"fmt"
	"net/url"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// DefaultTokenLifetime is the validity stamped on a traffic access token when
// the caller does not choose one.
const DefaultTokenLifetime = time.Hour

// minRSAKeyBits is the smallest RSA modulus RFC 7518 section 3.3 permits for
// RS256.
const minRSAKeyBits = 2048

// SigningKey is the private key an Issuer signs with, paired with the key ID it
// publishes in the JWKS. The KeyID must be stable for as long as tokens signed
// by this key can still be presented.
type SigningKey struct {
	KeyID      string
	PrivateKey crypto.Signer
}

// VerificationKey is a public key published in the JWKS but no longer used for
// signing. Retaining the previous key here is what makes rotation safe.
//
// As the gateway behaves today it snapshots the JWKS once at startup and never
// refetches, so a new key must be published and rolled out before it signs
// anything. If the verifier gains a refresh, retention stops being the only
// thing preventing a fleet of 401s and becomes a window for verifiers that have
// not refreshed yet. Retaining the previous key is correct under both.
type VerificationKey struct {
	KeyID     string
	PublicKey crypto.PublicKey
}

// SandboxBinding identifies the sandbox a token is minted for. The gateway
// compares these against the route it selected, so a token for one sandbox
// cannot be replayed against another.
//
// Tokens carry no jti. They are short-lived bearer tokens bound to a single
// sandbox, and a jti would only be useful alongside a revocation store that
// nothing here maintains. The trade is that a leaked token stays valid for the
// rest of its lifetime; shortening the lifetime is the lever, or rotating the
// signing key and dropping the retained one to invalidate every outstanding
// token at once.
type SandboxBinding struct {
	SandboxID  string
	SandboxUID string
}

// Issuer mints tokens for a single issuer URL.
type Issuer struct {
	issuerURL string
	lifetime  time.Duration

	active   SigningKey
	retained []VerificationKey

	now func() time.Time
}

// Option adjusts an Issuer at construction time.
type Option func(*Issuer)

// WithTokenLifetime sets how long minted tokens stay valid.
func WithTokenLifetime(lifetime time.Duration) Option {
	return func(i *Issuer) { i.lifetime = lifetime }
}

// WithRetainedKeys publishes additional public keys that are no longer used for
// signing. Use this during a rotation overlap window so verifiers holding an
// older JWKS snapshot still accept tokens signed by the previous key.
func WithRetainedKeys(keys ...VerificationKey) Option {
	return func(i *Issuer) { i.retained = append(i.retained, keys...) }
}

// withClock overrides the time source. Tests use it to mint tokens outside the
// current validity window.
func withClock(now func() time.Time) Option {
	return func(i *Issuer) { i.now = now }
}

// New returns an Issuer that signs with active and publishes it under issuerURL.
// issuerURL must be an absolute HTTPS URL: it becomes the iss claim, and the
// verifier compares it against the issuer advertised by OIDC discovery.
func New(issuerURL string, active SigningKey, opts ...Option) (*Issuer, error) {
	if err := validateHTTPSURL(issuerURL); err != nil {
		return nil, fmt.Errorf("issuer URL: %w", err)
	}
	if active.KeyID == "" {
		return nil, fmt.Errorf("signing key ID must not be empty")
	}
	if active.PrivateKey == nil {
		return nil, fmt.Errorf("signing key must not be nil")
	}
	if _, err := signatureAlgorithmFor(active.PrivateKey.Public()); err != nil {
		return nil, fmt.Errorf("signing key: %w", err)
	}

	issuer := &Issuer{
		issuerURL: issuerURL,
		lifetime:  DefaultTokenLifetime,
		active:    active,
		now:       time.Now,
	}
	for _, opt := range opts {
		opt(issuer)
	}

	if issuer.lifetime <= 0 {
		return nil, fmt.Errorf("token lifetime must be positive")
	}
	if err := issuer.validateRetainedKeys(); err != nil {
		return nil, err
	}
	return issuer, nil
}

func (i *Issuer) validateRetainedKeys() error {
	seen := map[string]struct{}{i.active.KeyID: {}}
	for _, key := range i.retained {
		if key.KeyID == "" {
			return fmt.Errorf("retained key ID must not be empty")
		}
		if _, exists := seen[key.KeyID]; exists {
			return fmt.Errorf("duplicate key ID %q", key.KeyID)
		}
		if _, err := signatureAlgorithmFor(key.PublicKey); err != nil {
			return fmt.Errorf("retained key %q: %w", key.KeyID, err)
		}
		seen[key.KeyID] = struct{}{}
	}
	return nil
}

// IssueTrafficAccessToken mints a token binding subject to a single sandbox and
// returns it alongside its expiry, so callers can record the expiry without
// parsing the token back.
func (i *Issuer) IssueTrafficAccessToken(subject string, binding SandboxBinding) (string, time.Time, error) {
	if subject == "" {
		return "", time.Time{}, fmt.Errorf("subject must not be empty")
	}
	if binding.SandboxID == "" {
		return "", time.Time{}, fmt.Errorf("sandbox ID must not be empty")
	}
	if binding.SandboxUID == "" {
		return "", time.Time{}, fmt.Errorf("sandbox UID must not be empty")
	}

	algorithm, err := signatureAlgorithmFor(i.active.PrivateKey.Public())
	if err != nil {
		return "", time.Time{}, err
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: algorithm, Key: i.active.PrivateKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", i.active.KeyID),
	)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create signer: %w", err)
	}

	// The verifier requires exp, iat and nbf to all be present. nbf is set equal
	// to iat rather than backdated: oidc.DefaultClockSkew is one minute, so a
	// replica running slightly ahead of a gateway does not trip nbf.
	issuedAt := i.now()
	expiry := issuedAt.Add(i.lifetime)
	claims := trafficAccessTokenClaims{
		Claims: jwt.Claims{
			Issuer:    i.issuerURL,
			Subject:   subject,
			IssuedAt:  jwt.NewNumericDate(issuedAt),
			NotBefore: jwt.NewNumericDate(issuedAt),
			Expiry:    jwt.NewNumericDate(expiry),
		},
		Sandbox: sandboxClaims{
			SandboxID:  binding.SandboxID,
			SandboxUID: binding.SandboxUID,
		},
	}

	rawJWT, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign token: %w", err)
	}
	return rawJWT, expiry, nil
}

// trafficAccessTokenClaims mirrors oidc.TrafficAccessTokenClaims. It is
// duplicated rather than imported so the issuer and the verifier stay
// independently changeable; the round-trip test is what keeps them aligned.
type trafficAccessTokenClaims struct {
	jwt.Claims
	Sandbox sandboxClaims `json:"sandbox"`
}

type sandboxClaims struct {
	SandboxID  string `json:"sandboxId"`
	SandboxUID string `json:"sandboxUid"`
}

// PublicKeySet returns the keys to publish at the JWKS endpoint: the active
// signing key first, then any retained keys.
func (i *Issuer) PublicKeySet() (jose.JSONWebKeySet, error) {
	keys := make([]jose.JSONWebKey, 0, 1+len(i.retained))

	activeKey, err := publicJWK(i.active.KeyID, i.active.PrivateKey.Public())
	if err != nil {
		return jose.JSONWebKeySet{}, err
	}
	keys = append(keys, activeKey)

	for _, retained := range i.retained {
		key, err := publicJWK(retained.KeyID, retained.PublicKey)
		if err != nil {
			return jose.JSONWebKeySet{}, err
		}
		keys = append(keys, key)
	}
	return jose.JSONWebKeySet{Keys: keys}, nil
}

func publicJWK(keyID string, publicKey crypto.PublicKey) (jose.JSONWebKey, error) {
	algorithm, err := signatureAlgorithmFor(publicKey)
	if err != nil {
		return jose.JSONWebKey{}, fmt.Errorf("key %q: %w", keyID, err)
	}
	return jose.JSONWebKey{
		Key:       publicKey,
		KeyID:     keyID,
		Use:       "sig",
		Algorithm: string(algorithm),
	}, nil
}

// signatureAlgorithmFor picks the JWS algorithm for a key type. The verifier
// rejects a token whose algorithm does not match its key, so this is the single
// place that decides the pairing.
func signatureAlgorithmFor(publicKey crypto.PublicKey) (jose.SignatureAlgorithm, error) {
	switch key := publicKey.(type) {
	case *rsa.PublicKey:
		// RFC 7518 section 3.3 requires at least 2048 bits for RS256, and neither
		// go-jose nor the gateway verifier enforces it: oidc.algorithmSupportsKey
		// pairs on key type and curve size and never looks at the RSA modulus. The
		// issuer is the only side that can decline to create the problem, so a weak
		// key is refused here rather than producing tokens nothing flags.
		if bits := key.N.BitLen(); bits < minRSAKeyBits {
			return "", fmt.Errorf("RSA key is %d bits, RS256 requires at least %d", bits, minRSAKeyBits)
		}
		return jose.RS256, nil
	case *ecdsa.PublicKey:
		switch bitSize := key.Curve.Params().BitSize; bitSize {
		case 256:
			return jose.ES256, nil
		case 384:
			return jose.ES384, nil
		case 521:
			return jose.ES512, nil
		default:
			return "", fmt.Errorf("unsupported ECDSA curve size %d", bitSize)
		}
	case ed25519.PublicKey:
		return jose.EdDSA, nil
	default:
		return "", fmt.Errorf("unsupported key type %T", publicKey)
	}
}

func validateHTTPSURL(rawURL string) error {
	parsedURL, err := url.Parse(rawURL)
	if err != nil || !parsedURL.IsAbs() || parsedURL.Scheme != "https" || parsedURL.Host == "" {
		return fmt.Errorf("must be an absolute HTTPS URL")
	}
	return nil
}
