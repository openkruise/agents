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
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Endpoint paths served for OIDC discovery. The gateway is pointed at
// DiscoveryPath and follows jwks_uri from there.
const (
	DiscoveryPath = "/.well-known/openid-configuration"
	JWKSPath      = "/jwks.json"
)

// discoveryDocument is the subset of the OIDC discovery document the gateway
// verifier reads. It only needs the issuer and the JWKS location; the remaining
// fields are advertised because discovery consumers commonly expect them.
type discoveryDocument struct {
	Issuer                           string   `json:"issuer"`
	JWKSURI                          string   `json:"jwks_uri"`
	ResponseTypesSupported           []string `json:"response_types_supported"`
	SubjectTypesSupported            []string `json:"subject_types_supported"`
	IDTokenSigningAlgValuesSupported []string `json:"id_token_signing_alg_values_supported"`
}

// Handler serves OIDC discovery and the JWKS for an Issuer.
//
// Both responses are public by design: they carry only public keys and the
// issuer identity, which is what lets a gateway bootstrap without credentials.
// The private key never leaves the Issuer.
func (i *Issuer) Handler() (http.Handler, error) {
	keySet, err := i.PublicKeySet()
	if err != nil {
		return nil, fmt.Errorf("build JWKS: %w", err)
	}
	jwksBody, err := json.Marshal(keySet)
	if err != nil {
		return nil, fmt.Errorf("marshal JWKS: %w", err)
	}

	algorithms, err := i.signingAlgorithms()
	if err != nil {
		return nil, err
	}
	discoveryBody, err := json.Marshal(discoveryDocument{
		Issuer:                           i.issuerURL,
		JWKSURI:                          strings.TrimSuffix(i.issuerURL, "/") + JWKSPath,
		ResponseTypesSupported:           []string{"id_token"},
		SubjectTypesSupported:            []string{"public"},
		IDTokenSigningAlgValuesSupported: algorithms,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal discovery document: %w", err)
	}

	// Serve under the issuer URL's own path so the advertised jwks_uri is
	// actually reachable when the issuer sits behind an ingress path prefix.
	basePath := issuerBasePath(i.issuerURL)

	mux := http.NewServeMux()
	mux.HandleFunc(basePath+DiscoveryPath, serveJSON(discoveryBody))
	mux.HandleFunc(basePath+JWKSPath, serveJSON(jwksBody))
	return mux, nil
}

// issuerBasePath returns the path portion of the issuer URL without a trailing
// slash, or "" when the issuer is served from the root.
func issuerBasePath(issuerURL string) string {
	parsed, err := url.Parse(issuerURL)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(parsed.Path, "/")
}

// signingAlgorithms lists every algorithm a published key can be used with, so
// the discovery document stays accurate across a rotation overlap window.
func (i *Issuer) signingAlgorithms() ([]string, error) {
	keySet, err := i.PublicKeySet()
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(keySet.Keys))
	algorithms := make([]string, 0, len(keySet.Keys))
	for _, key := range keySet.Keys {
		if _, exists := seen[key.Algorithm]; exists {
			continue
		}
		seen[key.Algorithm] = struct{}{}
		algorithms = append(algorithms, key.Algorithm)
	}
	return algorithms, nil
}

func serveJSON(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if _, err := w.Write(body); err != nil {
			// The response is already partially written, so there is nothing
			// useful left to say to the client.
			return
		}
	}
}
