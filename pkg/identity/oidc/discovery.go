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
	"fmt"
	"net/http"
)

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
	if err := validateHTTPSURL(discoveryURL); err != nil {
		return Discovered{}, fmt.Errorf("discovery URL: %w", err)
	}

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

	return Discovered{Issuer: document.Issuer, JWKSURI: document.JWKSURI}, nil
}
