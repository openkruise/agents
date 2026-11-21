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
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
	"github.com/openkruise/agents/pkg/identity/oidc"
)

// idTokenSignatureAlgorithms lists the algorithms verifyIDToken accepts.
// Matches the set oidc.Verifier accepts for the same reason: an external IdP
// decides its own signing algorithm, and this has to recognise whatever a
// discovered JWKS actually publishes.
var idTokenSignatureAlgorithms = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.EdDSA,
}

// verifiedIDToken is what verifyIDToken establishes about a presented ID
// Token: enough to build a jwtissuer.PrincipalBinding without parsing the
// token a second time.
type verifiedIDToken struct {
	Issuer  string
	Subject string
}

// verifyIDToken verifies rawIDToken against whichever of identity's
// AuthenticationRefs names the token's own issuer, and returns the verified
// issuer and subject.
//
// An AgentIdentity may name several AuthenticationRefs. Trying every one of
// them against the token would mean accepting whichever issuer happens to
// verify, which is backwards: the token itself says which issuer signed it,
// in its (unverified) iss claim, and that is what has to pick the one config
// to check against. Reading iss without verifying the signature first is safe
// here only because iss then has to match the resolved Status.Issuer of the
// config it picked, and the signature is still checked against that issuer's
// own JWKS before anything else about the token is trusted - mirroring the
// same pattern jwtissuer's checkIDTokenMatchesBinding uses to cross-check a
// caller-supplied binding against a token's unverified claims.
//
// The JWKS for the matched issuer is fetched fresh on every call, over HTTPS
// with the default system trust store. This is a deliberate v1
// simplification: AgentAuthenticationConfig issuers are ordinary public IdPs
// (Google, Keycloak, …) with publicly-trusted certificates and JWKS endpoints
// that are cheap to call and meant to be polled, so there is no in-cluster CA
// to trust and no obvious staleness hazard from skipping a cache. A caching
// layer, keyed by JWKSURI with its own refresh and invalidation story, is a
// reasonable follow-up if this proves too slow in practice, but is left out
// of this first version.
func verifyIDToken(ctx context.Context, reader client.Reader, namespace string,
	identity *securityv1alpha1.AgentIdentity, rawIDToken string) (verifiedIDToken, error) {
	unverifiedIssuer, err := unverifiedIDTokenIssuer(rawIDToken)
	if err != nil {
		return verifiedIDToken{}, err
	}

	config, err := authConfigForIssuer(ctx, reader, namespace, identity, unverifiedIssuer)
	if err != nil {
		return verifiedIDToken{}, err
	}

	keys, err := fetchJWKS(ctx, config.Status.JWKSURI)
	if err != nil {
		return verifiedIDToken{}, fmt.Errorf("fetch JWKS for issuer %q: %w", config.Status.Issuer, err)
	}

	token, err := jwt.ParseSigned(rawIDToken, idTokenSignatureAlgorithms)
	if err != nil {
		return verifiedIDToken{}, fmt.Errorf("parse ID token: %w", err)
	}
	header := token.Headers[0]
	if header.KeyID == "" {
		return verifiedIDToken{}, fmt.Errorf("ID token header must contain a kid")
	}
	key, ok := keys[header.KeyID]
	if !ok {
		return verifiedIDToken{}, fmt.Errorf("ID token references unknown kid %q", header.KeyID)
	}
	if key.Algorithm != "" && key.Algorithm != header.Algorithm {
		return verifiedIDToken{}, fmt.Errorf("ID token algorithm %q does not match key algorithm %q",
			header.Algorithm, key.Algorithm)
	}

	var claims jwt.Claims
	if err := token.Claims(key.Key, &claims); err != nil {
		return verifiedIDToken{}, fmt.Errorf("verify ID token signature and decode claims: %w", err)
	}

	if claims.Expiry == nil {
		return verifiedIDToken{}, fmt.Errorf("ID token is missing exp claim")
	}
	if claims.Issuer != config.Status.Issuer {
		// The unverified peek already picked this config by matching iss, so
		// this only fires if the signed claims disagree with the unverified
		// ones presented for that peek - a forged or corrupted token.
		return verifiedIDToken{}, fmt.Errorf("ID token issuer %q does not match verified issuer %q",
			claims.Issuer, config.Status.Issuer)
	}
	if claims.Subject == "" {
		return verifiedIDToken{}, fmt.Errorf("ID token is missing sub claim")
	}
	if !audienceIntersects(claims.Audience, config.Spec.JWT.AllowedAudience) {
		return verifiedIDToken{}, fmt.Errorf("ID token audience %v does not intersect allowed audience %v",
			claims.Audience, config.Spec.JWT.AllowedAudience)
	}
	if err := claims.ValidateWithLeeway(
		jwt.Expected{Issuer: config.Status.Issuer, Time: time.Now()}, oidc.DefaultClockSkew); err != nil {
		return verifiedIDToken{}, fmt.Errorf("validate ID token claims: %w", err)
	}

	return verifiedIDToken{Issuer: claims.Issuer, Subject: claims.Subject}, nil
}

// unverifiedIDTokenIssuer reads the iss claim out of rawIDToken without
// checking its signature.
//
// This is a pre-check used only to decide which AgentAuthenticationConfig to
// verify the token against; the signature is still checked, against that
// config's own JWKS, before anything read here is trusted. Mirrors the same
// unverified-claims pattern jwtissuer.checkIDTokenMatchesBinding uses for a
// comparable purpose.
func unverifiedIDTokenIssuer(rawIDToken string) (string, error) {
	parsed, err := jwt.ParseSigned(rawIDToken, idTokenSignatureAlgorithms)
	if err != nil {
		return "", fmt.Errorf("parse ID token: %w", err)
	}
	var claims jwt.Claims
	if err := parsed.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return "", fmt.Errorf("read ID token claims: %w", err)
	}
	if claims.Issuer == "" {
		return "", fmt.Errorf("ID token is missing iss claim")
	}
	return claims.Issuer, nil
}

// authConfigForIssuer finds the AgentAuthenticationConfig among identity's
// AuthenticationRefs whose resolved Status.Issuer matches issuer, and
// confirms it is Ready.
//
// A ref naming a config that no longer exists is skipped rather than failing
// outright: another ref may still match, and a dangling reference is already
// what keeps the identity itself out of Ready, which the caller checks
// separately before this is ever called.
func authConfigForIssuer(ctx context.Context, reader client.Reader, namespace string,
	identity *securityv1alpha1.AgentIdentity, issuer string) (*securityv1alpha1.AgentAuthenticationConfig, error) {
	for _, ref := range identity.Spec.AuthenticationRefs {
		config := &securityv1alpha1.AgentAuthenticationConfig{}
		key := client.ObjectKey{Namespace: namespace, Name: ref.Name}
		if err := reader.Get(ctx, key, config); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get AgentAuthenticationConfig %s/%s: %w", namespace, ref.Name, err)
		}
		if config.Status.Issuer != issuer {
			continue
		}
		if !isAuthConfigReady(config) {
			return nil, fmt.Errorf("AgentAuthenticationConfig %q matching issuer %q is not Ready", ref.Name, issuer)
		}
		if config.Spec.JWT == nil {
			// Admission requires the jwt block alongside type: jwt, and a config
			// cannot report an Issuer without having resolved discovery, which
			// only ever runs once that block is present. Defensive rather than
			// reachable: fail closed rather than panic on a nil AllowedAudience
			// read, in case an object predates that constraint.
			return nil, fmt.Errorf("AgentAuthenticationConfig %q has no jwt configuration", ref.Name)
		}
		return config, nil
	}
	return nil, fmt.Errorf("no AgentAuthenticationConfig on %q matches ID token issuer %q", identity.Name, issuer)
}

// isAuthConfigReady reports whether config's Ready condition describes its
// current generation and is True.
//
// Mirrors alreadyResolved's generation checks in authconfig_controller.go: a
// bare Ready=True is not enough, because a status write can lag the
// generation it is supposed to describe, and a stale verdict says nothing
// about whether the current spec - the one this request is about to trust -
// is usable.
func isAuthConfigReady(config *securityv1alpha1.AgentAuthenticationConfig) bool {
	if config.Status.ObservedGeneration != config.Generation {
		return false
	}
	ready := apiMeta.FindStatusCondition(config.Status.Conditions, securityv1alpha1.ConditionReady)
	return ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == config.Generation
}

// fetchJWKS fetches and decodes a JWKS document from jwksURI.
//
// jwksURI is Status.JWKSURI, which the AgentAuthenticationConfig controller
// only ever populates from a discovery document after validating it as an
// absolute HTTPS URL (pkg/identity/oidc.Discover), so this does not
// re-validate the scheme here. The request itself uses the default http.Client
// and the default system trust store: see verifyIDToken's own doc comment for
// why that is the deliberate choice for this first version.
func fetchJWKS(ctx context.Context, jwksURI string) (map[string]jose.JSONWebKey, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURI, nil)
	if err != nil {
		return nil, fmt.Errorf("build JWKS request: %w", err)
	}
	httpClient := &http.Client{Timeout: oidc.DefaultHTTPTimeout}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch JWKS: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch JWKS: unexpected status %s", response.Status)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, oidc.DefaultMaxResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("read JWKS response: %w", err)
	}
	if int64(len(body)) > oidc.DefaultMaxResponseSize {
		return nil, fmt.Errorf("JWKS response exceeds maximum size of %d bytes", oidc.DefaultMaxResponseSize)
	}

	var document struct {
		Keys []jose.JSONWebKey `json:"keys"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("decode JWKS: %w", err)
	}

	keys := make(map[string]jose.JSONWebKey, len(document.Keys))
	for _, key := range document.Keys {
		if key.Key == nil || key.KeyID == "" || !key.Valid() {
			continue
		}
		if key.Use != "" && key.Use != "sig" {
			continue
		}
		if !isAsymmetricPublicKey(key.Key) {
			continue
		}
		keys[key.KeyID] = key
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("JWKS contains no usable verification keys")
	}
	return keys, nil
}

// isAsymmetricPublicKey reports whether key is a public key type an ID token
// could plausibly be verified against. Excludes anything symmetric, which an
// external IdP's JWKS has no legitimate reason to publish in the first place.
func isAsymmetricPublicKey(key any) bool {
	switch key.(type) {
	case *rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey:
		return true
	default:
		return false
	}
}

// audienceIntersects reports whether tokenAudience names at least one of
// allowed.
func audienceIntersects(tokenAudience jwt.Audience, allowed []string) bool {
	for _, aud := range tokenAudience {
		for _, want := range allowed {
			if aud == want {
				return true
			}
		}
	}
	return false
}
