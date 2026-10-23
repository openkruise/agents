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
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// WellKnownPath is the path an OIDC discovery document is published at,
// relative to the issuer. OIDC Discovery 1.0 section 4 fixes it.
const WellKnownPath = "/.well-known/openid-configuration"

// ErrIssuerMismatch means the discovery document advertised an issuer other
// than the one it was retrieved from, so it cannot be trusted to speak for that
// issuer. It is distinguishable so a caller can report it as a configuration
// error rather than a transport failure.
var ErrIssuerMismatch = errors.New("oidc: discovery document issuer does not match the discovery URL")

// Discovered is what an OIDC discovery document tells a caller that wants to
// trust the issuer behind it.
type Discovered struct {
	// Issuer is the identifier an ID token must carry in iss, matched exactly.
	Issuer string
	// JWKSURI is where that issuer publishes its signing keys.
	JWKSURI string
}

// Discover fetches an OIDC discovery document and returns the issuer and
// jwks_uri it advertises.
//
// This is the resolution half of NewVerifier, split out for callers that need
// to record what an issuer claims without building a verifier from it. The
// AgentAuthenticationConfig controller is one: it resolves these values into
// status so nothing downstream has to take an operator's word for them, and so
// a caller cannot keep a trusted issuer name while pointing jwks_uri somewhere
// else.
//
// Unlike NewVerifier this trusts the system roots rather than an in-cluster CA
// ConfigMap, because the issuers it resolves are external identity providers
// with publicly chaining certificates. Redirects are not followed and the
// response is size-limited, for the same reason NewVerifier does both: a
// discovery URL is operator-supplied and may point at something that is not a
// discovery document.
func Discover(ctx context.Context, discoveryURL string, opts Options) (Discovered, error) {
	opts = withDefaults(opts)
	httpClient := &http.Client{
		Timeout: opts.HTTPTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer httpClient.CloseIdleConnections()

	return discoverWith(ctx, httpClient, discoveryURL, opts)
}

// discoverWith is Discover once the HTTP client is decided.
//
// Split out so tests can supply a client that trusts a local test server's
// certificate. Discover itself trusts only the system roots, which is correct
// in production and means its success path is otherwise unreachable in a unit
// test.
func discoverWith(ctx context.Context, httpClient *http.Client,
	discoveryURL string, opts Options) (Discovered, error) {
	if err := validateHTTPSURL(discoveryURL); err != nil {
		return Discovered{}, fmt.Errorf("discovery URL: %w", err)
	}

	document := discoveryDocument{}
	if err := fetchJSON(ctx, httpClient, discoveryURL, opts.MaxResponseSize, &document); err != nil {
		return Discovered{}, fmt.Errorf("fetch discovery document: %w", err)
	}

	// An issuer that advertises nothing, or advertises a plaintext key set, is
	// unusable. Reporting that here keeps the controller from writing a status
	// that looks resolved but cannot be verified against.
	if document.Issuer == "" {
		return Discovered{}, fmt.Errorf("discovery document has an empty issuer")
	}
	if err := validateHTTPSURL(document.JWKSURI); err != nil {
		return Discovered{}, fmt.Errorf("discovery document has an invalid jwks_uri: %w", err)
	}

	// OIDC Discovery 1.0 section 4.3: the issuer the document advertises must be
	// identical to the one the request was built from.
	//
	// Without this a document can claim to speak for an issuer it has nothing to
	// do with. Whoever can set discoveryUrl points it at a server they control,
	// has it return a trusted issuer name alongside their own jwks_uri, and the
	// name is recorded as resolved. Since a Principal ID derives from the issuer
	// plus the subject, tokens they sign themselves then resolve to existing
	// users and reach the delegations those users have already established.
	//
	// Deriving the expected issuer from the URL rather than asking the caller
	// for it is what makes the check meaningful: a caller-supplied expectation
	// is just the claim being verified, restated.
	if !issuerMatchesDiscoveryURL(discoveryURL, document.Issuer) {
		return Discovered{}, fmt.Errorf(
			"%w: document advertises issuer %q but was retrieved from %q",
			ErrIssuerMismatch, document.Issuer, discoveryURL)
	}

	return Discovered{Issuer: document.Issuer, JWKSURI: document.JWKSURI}, nil
}

// issuerMatchesDiscoveryURL reports whether the advertised issuer is the one
// the discovery URL was built from.
//
// A discovery URL is the issuer with WellKnownPath appended, so the expected
// issuer is recoverable by removing it. A URL that does not end in that path is
// not a discovery URL at all, and leaving the suffix in place makes the
// comparison fail, which is the safe direction.
//
// Only a trailing slash is normalised. Case, percent-encoding and default ports
// are left alone deliberately: treating two spellings as equal is how a check
// like this gets bypassed, and both sides here are operator configuration
// rather than arbitrary input.
func issuerMatchesDiscoveryURL(discoveryURL, issuer string) bool {
	// The trailing slash is trimmed before the well-known path so a URL written
	// with one still matches. Rejecting that would be safe but needlessly
	// confusing, and a trailing slash carries no meaning here.
	expected := strings.TrimSuffix(strings.TrimSuffix(discoveryURL, "/"), WellKnownPath)
	return strings.TrimSuffix(expected, "/") == strings.TrimSuffix(issuer, "/")
}
