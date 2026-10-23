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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// discoveryServer starts a TLS server whose body is built from its own URL, so
// a test can advertise an issuer that either matches the server or deliberately
// does not. It returns the discovery URL, the server URL and a trusting client.
//
// The server is started unstarted-then-StartTLS because the body usually has to
// contain the server's own address, which is not known until it is listening.
func discoveryServer(t *testing.T, status int, body func(serverURL string) string) (string, string, *http.Client) {
	t.Helper()
	var payload string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	server.StartTLS()
	t.Cleanup(server.Close)
	payload = body(server.URL)
	return server.URL + WellKnownPath, server.URL, server.Client()
}

// validDocument advertises the server as its own issuer, which is what a real
// provider does.
func validDocument(serverURL string) string {
	return fmt.Sprintf(`{"issuer":%q,"jwks_uri":%q}`, serverURL, serverURL+"/keys")
}

func TestDiscover(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    func(serverURL string) string
		wantErr string
	}{
		{
			name:   "a well-formed document resolves",
			status: http.StatusOK,
			body:   validDocument,
		},
		{
			name:   "extra fields are ignored",
			status: http.StatusOK,
			body: func(s string) string {
				return fmt.Sprintf(`{"issuer":%q,"jwks_uri":%q,`+
					`"authorization_endpoint":%q,"id_token_signing_alg_values_supported":["RS256"]}`,
					s, s+"/keys", s+"/auth")
			},
		},
		{
			name:   "a trailing slash on the issuer is tolerated",
			status: http.StatusOK,
			body: func(s string) string {
				return fmt.Sprintf(`{"issuer":%q,"jwks_uri":%q}`, s+"/", s+"/keys")
			},
		},
		{
			name:    "an empty issuer is rejected",
			status:  http.StatusOK,
			body:    func(s string) string { return fmt.Sprintf(`{"issuer":"","jwks_uri":%q}`, s+"/keys") },
			wantErr: "empty issuer",
		},
		{
			name:    "a missing issuer is rejected",
			status:  http.StatusOK,
			body:    func(s string) string { return fmt.Sprintf(`{"jwks_uri":%q}`, s+"/keys") },
			wantErr: "empty issuer",
		},
		{
			name:    "a plaintext jwks_uri is rejected",
			status:  http.StatusOK,
			body:    func(s string) string { return fmt.Sprintf(`{"issuer":%q,"jwks_uri":"http://x/keys"}`, s) },
			wantErr: "invalid jwks_uri",
		},
		{
			name:    "a missing jwks_uri is rejected",
			status:  http.StatusOK,
			body:    func(s string) string { return fmt.Sprintf(`{"issuer":%q}`, s) },
			wantErr: "invalid jwks_uri",
		},
		{
			name:    "a non-200 response is rejected",
			status:  http.StatusNotFound,
			body:    validDocument,
			wantErr: "fetch discovery document",
		},
		{
			name:    "a non-JSON body is rejected",
			status:  http.StatusOK,
			body:    func(string) string { return `<html><body>Sign in</body></html>` },
			wantErr: "fetch discovery document",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			discoveryURL, serverURL, client := discoveryServer(t, tt.status, tt.body)

			got, err := discoverWith(context.Background(), client, discoveryURL, withDefaults(Options{}))

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error: got %q, want it to contain %q", err, tt.wantErr)
				}
				if got.Issuer != "" || got.JWKSURI != "" {
					t.Errorf("a rejected document must resolve nothing, got %+v", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("discoverWith: %v", err)
			}
			// The advertised issuer is returned verbatim, trailing slash and all,
			// since an ID token's iss claim is matched against it exactly.
			if !strings.HasPrefix(got.Issuer, serverURL) {
				t.Errorf("issuer: got %q, want it to be %q", got.Issuer, serverURL)
			}
			if got.JWKSURI != serverURL+"/keys" {
				t.Errorf("jwksUri: got %q, want %q", got.JWKSURI, serverURL+"/keys")
			}
		})
	}
}

// TestDiscoverRejectsAnIssuerItWasNotRetrievedFrom is the regression test for
// the hole this check closes.
//
// OIDC Discovery 1.0 section 4.3 requires the advertised issuer to be identical
// to the one the request was built from. Without it, whoever can set
// discoveryUrl points it at a server they control, has it claim a trusted
// issuer name alongside their own jwks_uri, and that name gets recorded as
// resolved. Principal IDs derive from issuer plus subject, so tokens they sign
// would then resolve to existing users and reach delegations those users had
// already established.
func TestDiscoverRejectsAnIssuerItWasNotRetrievedFrom(t *testing.T) {
	tests := []struct {
		name   string
		issuer func(serverURL string) string
	}{
		{
			// The attack itself: an attacker-run server claiming to be Keycloak.
			name:   "a document claiming an unrelated trusted issuer",
			issuer: func(string) string { return "https://keycloak.example.com/realms/agents" },
		},
		{
			name:   "a document claiming a path under the real server",
			issuer: func(s string) string { return s + "/realms/other" },
		},
		{
			name:   "a document claiming the parent of the real server",
			issuer: func(string) string { return "https://example.com" },
		},
		{
			// Close enough to pass a sloppy prefix check, still a different host.
			name:   "a lookalike host",
			issuer: func(s string) string { return s + ".evil.example" },
		},
		{
			name:   "a plaintext version of the real server",
			issuer: func(s string) string { return strings.Replace(s, "https://", "http://", 1) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			discoveryURL, serverURL, client := discoveryServer(t, http.StatusOK, func(s string) string {
				return fmt.Sprintf(`{"issuer":%q,"jwks_uri":%q}`, tt.issuer(s), s+"/keys")
			})

			got, err := discoverWith(context.Background(), client, discoveryURL, withDefaults(Options{}))

			if !errors.Is(err, ErrIssuerMismatch) {
				t.Fatalf("error: got %v, want %v", err, ErrIssuerMismatch)
			}
			if got.Issuer != "" || got.JWKSURI != "" {
				t.Errorf("a mismatched document must resolve nothing, got %+v", got)
			}
			_ = serverURL
		})
	}
}

// TestIssuerMatchesDiscoveryURL covers the comparison directly, including the
// spellings it deliberately does not treat as equal.
func TestIssuerMatchesDiscoveryURL(t *testing.T) {
	const base = "https://keycloak.example.com/realms/agents"

	tests := []struct {
		name         string
		discoveryURL string
		issuer       string
		want         bool
	}{
		{"exact", base + WellKnownPath, base, true},
		{"issuer with a trailing slash", base + WellKnownPath, base + "/", true},
		{"discovery URL with a trailing slash", base + WellKnownPath + "/", base, true},
		{"root issuer", "https://example.com" + WellKnownPath, "https://example.com", true},
		{"different host", base + WellKnownPath, "https://evil.example/realms/agents", false},
		{"different realm", base + WellKnownPath, "https://keycloak.example.com/realms/other", false},
		{"issuer is a prefix of the real one", base + WellKnownPath, "https://keycloak.example.com", false},
		{"issuer extends the real one", base + WellKnownPath, base + "/extra", false},
		{"different scheme", base + WellKnownPath, "http://keycloak.example.com/realms/agents", false},
		{
			// A URL that is not a discovery URL keeps its whole path as the
			// expected issuer, so it fails rather than matching loosely.
			name:         "a URL without the well-known path",
			discoveryURL: base + "/openid-configuration",
			issuer:       base,
			want:         false,
		},
		{
			// Case is not normalised on purpose: treating two spellings as equal
			// is how a check like this gets worked around.
			name:         "host differing only in case",
			discoveryURL: base + WellKnownPath,
			issuer:       "https://Keycloak.example.com/realms/agents",
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := issuerMatchesDiscoveryURL(tt.discoveryURL, tt.issuer); got != tt.want {
				t.Errorf("issuerMatchesDiscoveryURL(%q, %q) = %v, want %v",
					tt.discoveryURL, tt.issuer, got, tt.want)
			}
		})
	}
}

// TestDiscoverRejectsNonHTTPSURL covers the check that happens before any
// request is made, so no server is involved.
func TestDiscoverRejectsNonHTTPSURL(t *testing.T) {
	for _, url := range []string{
		"http://keycloak.example.com" + WellKnownPath,
		"ftp://keycloak.example.com/x",
		"keycloak.example.com/x",
		"",
	} {
		t.Run(fmt.Sprintf("%q", url), func(t *testing.T) {
			if _, err := Discover(context.Background(), url, Options{}); err == nil {
				t.Error("expected a non-HTTPS discovery URL to be rejected")
			}
		})
	}
}

// TestDiscoverEnforcesTheResponseSizeLimit stops a discovery URL pointing at
// something enormous from being read into memory.
func TestDiscoverEnforcesTheResponseSizeLimit(t *testing.T) {
	padding := strings.Repeat("x", 4096)
	discoveryURL, _, client := discoveryServer(t, http.StatusOK, func(s string) string {
		return fmt.Sprintf(`{"issuer":%q,"jwks_uri":%q,"pad":%q}`, s, s+"/keys", padding)
	})

	opts := withDefaults(Options{})
	opts.MaxResponseSize = 512

	if _, err := discoverWith(context.Background(), client, discoveryURL, opts); err == nil {
		t.Fatal("expected an oversized document to be rejected")
	} else if !strings.Contains(err.Error(), "maximum size") {
		t.Errorf("error: got %q, want it to mention the size limit", err)
	}
}

// TestDiscoverDoesNotFollowRedirects keeps a discovery URL from being bounced
// somewhere the operator did not name.
func TestDiscoverDoesNotFollowRedirects(t *testing.T) {
	var reached bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _ = w.Write([]byte(`{"issuer":"https://evil.example","jwks_uri":"https://evil.example/keys"}`))
	}))
	t.Cleanup(target.Close)

	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	client := redirector.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	if _, err := discoverWith(context.Background(), client,
		redirector.URL+WellKnownPath, withDefaults(Options{})); err == nil {
		t.Error("expected a redirect not to resolve")
	}
	if reached {
		t.Error("the redirect was followed to a host the operator did not name")
	}
}
