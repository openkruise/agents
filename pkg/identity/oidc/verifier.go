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
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var asymmetricSignatureAlgorithms = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.EdDSA,
}

// Verifier verifies a traffic access token against a fixed OIDC key snapshot.
type Verifier interface {
	Verify(rawJWT string) (*TrafficAccessTokenClaims, error)
}

// TrafficAccessTokenClaims contains the claims used to authorize sandbox traffic.
type TrafficAccessTokenClaims struct {
	jwt.Claims
	Sandbox SandboxClaims `json:"sandbox"`
}

// SandboxClaims identifies the sandbox represented by a traffic access token.
type SandboxClaims struct {
	SandboxID  string `json:"sandboxId"`
	SandboxUID string `json:"sandboxUid"`
}

type verifier struct {
	issuer       string
	keys         map[string]jose.JSONWebKey
	clockSkew    time.Duration
	maxTokenSize int
}

type discoveryDocument struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

type jwksDocument struct {
	Keys []json.RawMessage `json:"keys"`
}

// NewVerifier loads the OIDC provider and returns a verifier backed by an immutable JWKS snapshot.
func NewVerifier(ctx context.Context, reader client.Reader, opts Options) (Verifier, error) {
	opts = withDefaults(opts)
	if err := validateOptions(opts); err != nil {
		return nil, fmt.Errorf("invalid OIDC options: %w", err)
	}
	if reader == nil {
		return nil, fmt.Errorf("ConfigMap reader must not be nil")
	}

	configMap := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: opts.CAConfigMapNamespace, Name: opts.CAConfigMapName}
	if err := reader.Get(ctx, key, configMap); err != nil {
		return nil, fmt.Errorf("get CA ConfigMap %s/%s: %w", key.Namespace, key.Name, err)
	}
	caPEM, ok := configMap.Data[opts.CAConfigMapKey]
	if !ok || caPEM == "" {
		return nil, fmt.Errorf("CA ConfigMap %s/%s does not contain non-empty key %q", key.Namespace, key.Name, opts.CAConfigMapKey)
	}
	rootCAs := x509.NewCertPool()
	if !rootCAs.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, fmt.Errorf("CA ConfigMap %s/%s key %q contains no valid PEM certificates", key.Namespace, key.Name, opts.CAConfigMapKey)
	}

	httpClient := &http.Client{
		Timeout: opts.HTTPTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: rootCAs, MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer httpClient.CloseIdleConnections()

	discovery := discoveryDocument{}
	if err := fetchJSON(ctx, httpClient, opts.DiscoveryURL, opts.MaxResponseSize, &discovery); err != nil {
		return nil, fmt.Errorf("fetch OIDC discovery document: %w", err)
	}
	if discovery.Issuer == "" {
		return nil, fmt.Errorf("OIDC discovery document has an empty issuer")
	}
	if err := validateHTTPSURL(discovery.JWKSURI); err != nil {
		return nil, fmt.Errorf("OIDC discovery document has invalid jwks_uri: %w", err)
	}

	keySet := jwksDocument{}
	if err := fetchJSON(ctx, httpClient, discovery.JWKSURI, opts.MaxResponseSize, &keySet); err != nil {
		return nil, fmt.Errorf("fetch OIDC JWKS: %w", err)
	}
	decodedKeys, err := decodeJWKs(keySet.Keys)
	if err != nil {
		return nil, fmt.Errorf("decode OIDC JWKS: %w", err)
	}
	keys, err := validateKeys(decodedKeys)
	if err != nil {
		return nil, fmt.Errorf("validate OIDC JWKS: %w", err)
	}

	return &verifier{
		issuer:       discovery.Issuer,
		keys:         keys,
		clockSkew:    opts.ClockSkew,
		maxTokenSize: opts.MaxTokenSize,
	}, nil
}

func decodeJWKs(rawKeys []json.RawMessage) ([]jose.JSONWebKey, error) {
	keys := make([]jose.JSONWebKey, 0, len(rawKeys))
	for index, rawKey := range rawKeys {
		metadata := struct {
			KeyOperations []string `json:"key_ops"`
		}{}
		if err := json.Unmarshal(rawKey, &metadata); err != nil {
			return nil, fmt.Errorf("decode key %d metadata: %w", index, err)
		}
		if len(metadata.KeyOperations) > 0 {
			canVerify := false
			for _, operation := range metadata.KeyOperations {
				if operation == "verify" {
					canVerify = true
					break
				}
			}
			if !canVerify {
				return nil, fmt.Errorf("key %d key_ops does not permit verify", index)
			}
		}

		key := jose.JSONWebKey{}
		if err := json.Unmarshal(rawKey, &key); err != nil {
			return nil, fmt.Errorf("decode key %d: %w", index, err)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// Verify validates a token signature and the required traffic access token claims.
func (v *verifier) Verify(rawJWT string) (*TrafficAccessTokenClaims, error) {
	if rawJWT == "" {
		return nil, fmt.Errorf("token must not be empty")
	}
	if len(rawJWT) > v.maxTokenSize {
		return nil, fmt.Errorf("token exceeds maximum size of %d bytes", v.maxTokenSize)
	}

	token, err := jwt.ParseSigned(rawJWT, asymmetricSignatureAlgorithms)
	if err != nil {
		return nil, fmt.Errorf("parse signed token: %w", err)
	}
	if len(token.Headers) != 1 {
		return nil, fmt.Errorf("token must contain exactly one signature")
	}
	header := token.Headers[0]
	if header.KeyID == "" {
		return nil, fmt.Errorf("token header must contain a kid")
	}
	key, ok := v.keys[header.KeyID]
	if !ok {
		return nil, fmt.Errorf("token references unknown kid %q", header.KeyID)
	}
	if key.Algorithm != "" && key.Algorithm != header.Algorithm {
		return nil, fmt.Errorf("token algorithm %q does not match JWK algorithm %q", header.Algorithm, key.Algorithm)
	}
	if !algorithmSupportsKey(jose.SignatureAlgorithm(header.Algorithm), key.Key) {
		return nil, fmt.Errorf("token algorithm %q is incompatible with key %q", header.Algorithm, header.KeyID)
	}

	claims := &TrafficAccessTokenClaims{}
	if err := token.Claims(key.Key, claims); err != nil {
		return nil, fmt.Errorf("verify token signature and decode claims: %w", err)
	}
	if claims.Expiry == nil {
		return nil, fmt.Errorf("token is missing exp claim")
	}
	if claims.IssuedAt == nil {
		return nil, fmt.Errorf("token is missing iat claim")
	}
	if claims.NotBefore == nil {
		return nil, fmt.Errorf("token is missing nbf claim")
	}
	if claims.Issuer != v.issuer {
		return nil, fmt.Errorf("token issuer %q does not match expected issuer %q", claims.Issuer, v.issuer)
	}
	if claims.Subject == "" {
		return nil, fmt.Errorf("token is missing sub claim")
	}
	if err := claims.Claims.ValidateWithLeeway(jwt.Expected{Issuer: v.issuer, Time: time.Now()}, v.clockSkew); err != nil {
		return nil, fmt.Errorf("validate token claims: %w", err)
	}
	return claims, nil
}

func fetchJSON(ctx context.Context, httpClient *http.Client, target string, maxSize int64, dest interface{}) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP status %s", response.Status)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxSize+1))
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}
	if int64(len(body)) > maxSize {
		return fmt.Errorf("response exceeds maximum size of %d bytes", maxSize)
	}
	if err := json.Unmarshal(body, dest); err != nil {
		return fmt.Errorf("decode JSON response: %w", err)
	}
	return nil
}

func validateHTTPSURL(rawURL string) error {
	parsedURL, err := url.Parse(rawURL)
	if err != nil || !parsedURL.IsAbs() || parsedURL.Scheme != "https" || parsedURL.Host == "" {
		return fmt.Errorf("must be an absolute HTTPS URL")
	}
	return nil
}

func validateKeys(input []jose.JSONWebKey) (map[string]jose.JSONWebKey, error) {
	if len(input) == 0 {
		return nil, fmt.Errorf("key set must contain at least one key")
	}
	keys := make(map[string]jose.JSONWebKey, len(input))
	for index, key := range input {
		if key.Key == nil {
			return nil, fmt.Errorf("key %d is invalid", index)
		}
		if key.KeyID == "" {
			return nil, fmt.Errorf("key %d has an empty kid", index)
		}
		if key.Use != "" && key.Use != "sig" {
			return nil, fmt.Errorf("key %q has unsupported use %q", key.KeyID, key.Use)
		}
		if !isAsymmetricPublicKey(key.Key) {
			return nil, fmt.Errorf("key %q must be an asymmetric public key", key.KeyID)
		}
		if !key.Valid() {
			return nil, fmt.Errorf("key %q is invalid", key.KeyID)
		}
		if key.Algorithm != "" && !algorithmSupportsKey(jose.SignatureAlgorithm(key.Algorithm), key.Key) {
			return nil, fmt.Errorf("key %q has incompatible algorithm %q", key.KeyID, key.Algorithm)
		}
		if _, exists := keys[key.KeyID]; exists {
			return nil, fmt.Errorf("duplicate kid %q", key.KeyID)
		}
		keys[key.KeyID] = key
	}
	return keys, nil
}

func isAsymmetricPublicKey(key interface{}) bool {
	switch key.(type) {
	case *rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey:
		return true
	default:
		return false
	}
}

func algorithmSupportsKey(algorithm jose.SignatureAlgorithm, key interface{}) bool {
	switch typedKey := key.(type) {
	case *rsa.PublicKey:
		return algorithm == jose.RS256 || algorithm == jose.RS384 || algorithm == jose.RS512 ||
			algorithm == jose.PS256 || algorithm == jose.PS384 || algorithm == jose.PS512
	case *ecdsa.PublicKey:
		switch typedKey.Curve.Params().BitSize {
		case 256:
			return algorithm == jose.ES256
		case 384:
			return algorithm == jose.ES384
		case 521:
			return algorithm == jose.ES512
		default:
			return false
		}
	case ed25519.PublicKey:
		return algorithm == jose.EdDSA
	default:
		return false
	}
}
