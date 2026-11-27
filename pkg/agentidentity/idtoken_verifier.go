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
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"golang.org/x/sync/singleflight"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
	"github.com/openkruise/agents/pkg/identity/oidc"
)

// The classes verifyIDToken sorts its failures into, so ExchangePrincipalToken
// can answer each the way section 7.6 asks rather than treating every one as
// bad credentials. Anything outside them is an internal error.
var (
	// errIDTokenRejected means the ID token itself is unacceptable: malformed,
	// from an issuer this AgentIdentity does not trust, signed by a key the
	// issuer does not publish, or failing a claim check. Section 7.6's
	// unauthenticated.
	errIDTokenRejected = errors.New("ID token rejected")

	// errAuthConfigNotReady means the AgentAuthenticationConfig for the token's
	// issuer exists but cannot be used now. Section 7.6's failed_precondition:
	// the token may be perfectly good, and the caller waits for the
	// configuration to recover.
	errAuthConfigNotReady = errors.New("authentication config is not ready")

	// errIdentityProviderUnavailable means the issuer's JWKS could not be
	// fetched or read. Section 7.6's unavailable, OIDC_UNAVAILABLE: nothing
	// about the token is known to be wrong, and a bounded retry may succeed.
	errIdentityProviderUnavailable = errors.New("identity provider is unavailable")
)

const (
	// jwksTTL is how long a fetched JWKS is used before it is fetched again:
	// short enough that a key the issuer withdraws stops being trusted within
	// minutes, long enough that exchanges do not each cost the issuer a
	// request and inherit its latency.
	jwksTTL = 5 * time.Minute

	// jwksRefetchInterval is the least time between two fetches of one JWKS. A
	// token naming a kid the cached set lacks triggers an early refetch, in
	// case the issuer has just rotated; this bounds how often presenting
	// unknown kids can make this process call the issuer.
	jwksRefetchInterval = 10 * time.Second
)

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
// Every failure wraps one of errIDTokenRejected, errAuthConfigNotReady or
// errIdentityProviderUnavailable, or is an internal error.
func verifyIDToken(ctx context.Context, reader client.Reader, keys *jwksCache, namespace string,
	identity *securityv1alpha1.AgentIdentity, rawIDToken string) (verifiedIDToken, error) {
	token, err := jwt.ParseSigned(rawIDToken, oidc.SignatureAlgorithms)
	if err != nil {
		return verifiedIDToken{}, fmt.Errorf("%w: parse ID token: %w", errIDTokenRejected, err)
	}
	unverifiedIssuer, err := unverifiedIDTokenIssuer(token)
	if err != nil {
		return verifiedIDToken{}, err
	}

	config, err := authConfigForIssuer(ctx, reader, namespace, identity, unverifiedIssuer)
	if err != nil {
		return verifiedIDToken{}, err
	}

	key, err := keys.keyFor(ctx, config.Status.JWKSURI, token.Headers[0])
	if err != nil {
		return verifiedIDToken{}, err
	}

	var claims jwt.Claims
	if err := token.Claims(key.Key, &claims); err != nil {
		return verifiedIDToken{}, fmt.Errorf("%w: verify ID token signature and decode claims: %w",
			errIDTokenRejected, err)
	}

	if claims.Expiry == nil {
		return verifiedIDToken{}, fmt.Errorf("%w: ID token is missing exp claim", errIDTokenRejected)
	}
	if claims.Issuer != config.Status.Issuer {
		// The unverified peek already picked this config by matching iss, so
		// this only fires if the signed claims disagree with the unverified
		// ones presented for that peek - a forged or corrupted token.
		return verifiedIDToken{}, fmt.Errorf("%w: ID token issuer %q does not match verified issuer %q",
			errIDTokenRejected, claims.Issuer, config.Status.Issuer)
	}
	if claims.Subject == "" {
		return verifiedIDToken{}, fmt.Errorf("%w: ID token is missing sub claim", errIDTokenRejected)
	}
	if !audienceIntersects(claims.Audience, config.Spec.JWT.AllowedAudience) {
		return verifiedIDToken{}, fmt.Errorf("%w: ID token audience %v does not intersect allowed audience %v",
			errIDTokenRejected, claims.Audience, config.Spec.JWT.AllowedAudience)
	}
	if err := claims.ValidateWithLeeway(
		jwt.Expected{Issuer: config.Status.Issuer, Time: time.Now()}, oidc.DefaultClockSkew); err != nil {
		return verifiedIDToken{}, fmt.Errorf("%w: validate ID token claims: %w", errIDTokenRejected, err)
	}

	return verifiedIDToken{Issuer: claims.Issuer, Subject: claims.Subject}, nil
}

// unverifiedIDTokenIssuer reads the iss claim out of token without checking
// its signature.
//
// This is a pre-check used only to decide which AgentAuthenticationConfig to
// verify the token against; the signature is still checked, against that
// config's own JWKS, before anything read here is trusted. Mirrors the same
// unverified-claims pattern jwtissuer.checkIDTokenMatchesBinding uses for a
// comparable purpose.
func unverifiedIDTokenIssuer(token *jwt.JSONWebToken) (string, error) {
	var claims jwt.Claims
	if err := token.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return "", fmt.Errorf("%w: read ID token claims: %w", errIDTokenRejected, err)
	}
	if claims.Issuer == "" {
		return "", fmt.Errorf("%w: ID token is missing iss claim", errIDTokenRejected)
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
			return nil, fmt.Errorf("%w: AgentAuthenticationConfig %q matching issuer %q is not Ready",
				errAuthConfigNotReady, ref.Name, issuer)
		}
		if config.Spec.JWT == nil {
			// Admission requires the jwt block alongside type: jwt, and a config
			// cannot report an Issuer without having resolved discovery, which
			// only ever runs once that block is present. Defensive rather than
			// reachable: fail closed rather than panic on a nil AllowedAudience
			// read, in case an object predates that constraint.
			return nil, fmt.Errorf("%w: AgentAuthenticationConfig %q has no jwt configuration",
				errAuthConfigNotReady, ref.Name)
		}
		return config, nil
	}
	return nil, fmt.Errorf("%w: no AgentAuthenticationConfig on %q matches ID token issuer %q",
		errIDTokenRejected, identity.Name, issuer)
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

// jwksCache holds the key sets ID tokens are verified against, by JWKS URI.
//
// Without it every exchange would fetch its issuer's JWKS, so exchange
// traffic would become the same traffic against the issuer, each exchange
// would wait on that round trip, and a rate limit or brief outage there would
// fail them all. The sets are loaded and validated by oidc.LoadKeySet, the
// same policy the gateway's verifier applies.
//
// Status.JWKSURI is only ever written from a discovery document that
// oidc.Discover validated as absolute HTTPS, so the URI is not re-validated
// here. The system trust store applies: these are ordinary IdPs with publicly
// trusted certificates.
type jwksCache struct {
	httpClient *http.Client
	flights    singleflight.Group

	mu      sync.Mutex
	entries map[string]jwksEntry
}

type jwksEntry struct {
	keys      map[string]jose.JSONWebKey
	fetchedAt time.Time
}

func newJWKSCache() *jwksCache {
	return &jwksCache{
		httpClient: &http.Client{
			Timeout: oidc.DefaultHTTPTimeout,
			// An HTTPS JWKS URI says nothing about where a redirect leads,
			// including back to plaintext, where anyone on the path could
			// substitute the keys. oidc.NewVerifier refuses them for the same
			// reason.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		entries: map[string]jwksEntry{},
	}
}

// keyFor returns the key from the JWKS at jwksURI that header says signed a
// token. A failure to obtain the set wraps errIdentityProviderUnavailable;
// a set that has no suitable key wraps errIDTokenRejected.
func (c *jwksCache) keyFor(ctx context.Context, jwksURI string, header jose.Header) (jose.JSONWebKey, error) {
	entry, err := c.keySet(ctx, jwksURI, time.Time{})
	if err != nil {
		return jose.JSONWebKey{}, err
	}
	key, err := oidc.KeyFor(entry.keys, header)
	if errors.Is(err, oidc.ErrUnknownKeyID) && time.Since(entry.fetchedAt) >= jwksRefetchInterval {
		// The issuer may have rotated since this copy was fetched. It publishes
		// a new key before signing with it, so a fresh copy may hold it.
		if entry, err = c.keySet(ctx, jwksURI, entry.fetchedAt); err != nil {
			return jose.JSONWebKey{}, err
		}
		key, err = oidc.KeyFor(entry.keys, header)
	}
	if err != nil {
		return jose.JSONWebKey{}, fmt.Errorf("%w: %w", errIDTokenRejected, err)
	}
	return key, nil
}

// keySet returns the cached set for jwksURI, fetching it when there is none,
// when it has outlived jwksTTL, or when it is the copy fetched at stale, which
// the caller has found wanting. Concurrent fetches of one URI share a request.
func (c *jwksCache) keySet(ctx context.Context, jwksURI string, stale time.Time) (jwksEntry, error) {
	c.mu.Lock()
	entry, ok := c.entries[jwksURI]
	c.mu.Unlock()
	if ok && time.Since(entry.fetchedAt) < jwksTTL && !entry.fetchedAt.Equal(stale) {
		return entry, nil
	}

	results := c.flights.DoChan(jwksURI, func() (any, error) {
		// Detached from whichever caller started it, since its result serves
		// every caller waiting on it. The client's timeout bounds it.
		keys, err := oidc.LoadKeySet(context.WithoutCancel(ctx), c.httpClient, jwksURI, oidc.DefaultMaxResponseSize)
		if err != nil {
			return nil, err
		}
		fetched := jwksEntry{keys: keys, fetchedAt: time.Now()}
		c.mu.Lock()
		c.entries[jwksURI] = fetched
		c.mu.Unlock()
		return fetched, nil
	})
	select {
	case <-ctx.Done():
		return jwksEntry{}, ctx.Err()
	case result := <-results:
		if result.Err != nil {
			return jwksEntry{}, fmt.Errorf("%w: %w", errIdentityProviderUnavailable, result.Err)
		}
		return result.Val.(jwksEntry), nil
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
