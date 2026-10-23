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
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/identity/jwtissuer"
)

// deferredHandler lets a server start before the handler that needs its
// address exists. The issuer URL is baked into the document the handler serves,
// and httptest only reveals the address once it is listening.
type deferredHandler struct {
	handler http.Handler
}

func (d *deferredHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.handler.ServeHTTP(w, r)
}

// TestDiscoverAgainstTheRealIssuerHandler runs discovery against a document
// this repo actually serves, rather than against a hand-written stub.
//
// The stubs in discovery_test.go answer whatever a case needs, which is right
// for covering the branches but leaves one thing unchecked: whether the issuer
// handler and the mismatch rule agree about what a discovery URL looks like. If
// they disagree, every legitimate config fails closed and the unit tests still
// pass. So the honest case here matters as much as the attack.
func TestDiscoverAgainstTheRealIssuerHandler(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}

	deferred := &deferredHandler{}
	server := httptest.NewTLSServer(deferred)
	defer server.Close()

	issuer, err := jwtissuer.New(server.URL, jwtissuer.SigningKey{KeyID: "verify-1", PrivateKey: key})
	if err != nil {
		t.Fatalf("build issuer: %v", err)
	}
	served, err := issuer.Handler()
	if err != nil {
		t.Fatalf("build handler: %v", err)
	}
	deferred.handler = served

	// A second server publishing the same document is the attack: whoever can
	// set discoveryUrl stands up a host they control that returns a trusted
	// issuer name alongside their own jwks_uri.
	impostor := httptest.NewTLSServer(served)
	defer impostor.Close()

	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	roots.AddCert(impostor.Certificate())
	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		},
	}
	defer httpClient.CloseIdleConnections()

	cases := []struct {
		name         string
		discoveryURL string
		wantIssuer   string
		wantErr      error
		// wantErrText is for failures that carry no sentinel.
		wantErrText string
	}{
		{
			name:         "the issuer resolves at its own discovery URL",
			discoveryURL: server.URL + WellKnownPath,
			wantIssuer:   server.URL,
		},
		{
			// issuerMatchesDiscoveryURL tolerates a trailing slash, but this
			// issuer's own mux does not serve the slashed path, so the request
			// never reaches that check. The tolerance is there for external
			// providers that do serve it; against this issuer the slash is
			// simply wrong, and it fails as a 404 rather than a mismatch.
			name:         "this issuer does not serve the slashed discovery path",
			discoveryURL: server.URL + WellKnownPath + "/",
			wantErrText:  "404",
		},
		{
			name:         "the same document served elsewhere is refused",
			discoveryURL: impostor.URL + WellKnownPath,
			wantErr:      ErrIssuerMismatch,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := discoverWith(context.Background(), httpClient,
				tc.discoveryURL, withDefaults(Options{}))

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got %v, want an error wrapping %v", err, tc.wantErr)
				}
				return
			}
			if tc.wantErrText != "" {
				if err == nil {
					t.Fatalf("resolving %s succeeded, want an error containing %q",
						tc.discoveryURL, tc.wantErrText)
				}
				if !strings.Contains(err.Error(), tc.wantErrText) {
					t.Fatalf("got %v, want an error containing %q", err, tc.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolving %s failed: %v", tc.discoveryURL, err)
			}
			if resolved.Issuer != tc.wantIssuer {
				t.Errorf("issuer is %q, want %q", resolved.Issuer, tc.wantIssuer)
			}
			if want := tc.wantIssuer + jwtissuer.JWKSPath; resolved.JWKSURI != want {
				t.Errorf("jwks_uri is %q, want %q", resolved.JWKSURI, want)
			}
		})
	}
}
