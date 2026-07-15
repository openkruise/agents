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
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testCANamespace = "identity-system"
	testCAName      = "oidc-ca"
	testCAKey       = "ca.crt"
)

type loaderFixture struct {
	discoveryStatus int
	discoveryBody   string
	jwksStatus      int
	jwksBody        string
	discoveryCalls  int
	jwksCalls       int
}

func TestNewVerifier(t *testing.T) {
	rsaKey := mustRSAKey(t)
	publicJWK := jose.JSONWebKey{Key: &rsaKey.PublicKey, KeyID: "rsa", Use: "sig", Algorithm: string(jose.RS256)}

	tests := []struct {
		name            string
		configure       func(*loaderFixture, *httptest.Server)
		configMap       string
		maxResponseSize int64
		assertVerifier  bool
		expectDiscovery int
		expectJWKS      int
		expectError     string
	}{
		{name: "valid flow and immutable key snapshot", assertVerifier: true, expectDiscovery: 1, expectJWKS: 1},
		{name: "ConfigMap missing", configMap: "missing", expectError: "get CA ConfigMap"},
		{name: "CA key missing", configMap: "key-missing", expectError: "does not contain non-empty key"},
		{name: "CA PEM invalid", configMap: "bad-ca", expectError: "contains no valid PEM certificates"},
		{name: "discovery non-200", configure: func(f *loaderFixture, _ *httptest.Server) { f.discoveryStatus = http.StatusServiceUnavailable }, expectDiscovery: 1, expectError: "unexpected HTTP status"},
		{name: "discovery malformed", configure: func(f *loaderFixture, _ *httptest.Server) { f.discoveryBody = "{" }, expectDiscovery: 1, expectError: "decode JSON response"},
		{name: "discovery missing issuer", configure: func(f *loaderFixture, server *httptest.Server) {
			f.discoveryBody = fmt.Sprintf(`{"jwks_uri":%q}`, server.URL+"/jwks")
		}, expectDiscovery: 1, expectError: "empty issuer"},
		{name: "discovery missing JWKS URI", configure: func(f *loaderFixture, _ *httptest.Server) { f.discoveryBody = `{"issuer":"https://issuer.example"}` }, expectDiscovery: 1, expectError: "invalid jwks_uri"},
		{name: "discovery HTTP JWKS URI", configure: func(f *loaderFixture, _ *httptest.Server) {
			f.discoveryBody = `{"issuer":"https://issuer.example","jwks_uri":"http://issuer.example/jwks"}`
		}, expectDiscovery: 1, expectError: "absolute HTTPS URL"},
		{name: "JWKS non-200", configure: func(f *loaderFixture, _ *httptest.Server) { f.jwksStatus = http.StatusBadGateway }, expectDiscovery: 1, expectJWKS: 1, expectError: "unexpected HTTP status"},
		{name: "JWKS malformed", configure: func(f *loaderFixture, _ *httptest.Server) { f.jwksBody = "{" }, expectDiscovery: 1, expectJWKS: 1, expectError: "decode JSON response"},
		{name: "JWKS bad key", configure: func(f *loaderFixture, _ *httptest.Server) {
			f.jwksBody = `{"keys":[{"kty":"oct","kid":"shared","k":"eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHg"}]}`
		}, expectDiscovery: 1, expectJWKS: 1, expectError: "asymmetric public key"},
		{name: "JWKS key operations do not permit verification", configure: func(f *loaderFixture, _ *httptest.Server) {
			encoded := mustJSON(t, publicJWK)
			f.jwksBody = fmt.Sprintf(`{"keys":[%s]}`, strings.TrimSuffix(encoded, "}")+`,"key_ops":["encrypt"]}`)
		}, expectDiscovery: 1, expectJWKS: 1, expectError: "key_ops does not permit verify"},
		{name: "JWKS duplicate kid", configure: func(f *loaderFixture, _ *httptest.Server) {
			f.jwksBody = mustJSON(t, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{publicJWK, publicJWK}})
		}, expectDiscovery: 1, expectJWKS: 1, expectError: "duplicate kid"},
		{name: "discovery response too large", configure: func(f *loaderFixture, _ *httptest.Server) { f.discoveryBody = strings.Repeat("x", 256) }, maxResponseSize: 32, expectDiscovery: 1, expectError: "exceeds maximum size"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := &loaderFixture{discoveryStatus: http.StatusOK, jwksStatus: http.StatusOK}
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/discovery":
					fixture.discoveryCalls++
					response.WriteHeader(fixture.discoveryStatus)
					_, _ = response.Write([]byte(fixture.discoveryBody))
				case "/jwks":
					fixture.jwksCalls++
					response.WriteHeader(fixture.jwksStatus)
					_, _ = response.Write([]byte(fixture.jwksBody))
				default:
					response.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			fixture.discoveryBody = fmt.Sprintf(`{"issuer":"https://issuer.example","jwks_uri":%q}`, server.URL+"/jwks")
			fixture.jwksBody = mustJSON(t, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{publicJWK}})
			if tt.configure != nil {
				tt.configure(fixture, server)
			}

			reader := loaderReader(t, server, tt.configMap)
			opts := Options{
				DiscoveryURL:         server.URL + "/discovery",
				CAConfigMapNamespace: testCANamespace,
				CAConfigMapName:      testCAName,
				CAConfigMapKey:       testCAKey,
			}
			if tt.maxResponseSize != 0 {
				opts.MaxResponseSize = tt.maxResponseSize
			}

			underTest, err := NewVerifier(context.Background(), reader, opts)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Nil(t, underTest)
			} else {
				require.NoError(t, err)
				require.NotNil(t, underTest)
			}
			if tt.assertVerifier {
				now := time.Now().Truncate(time.Second)
				validJWT := signToken(t, jose.RS256, rsaKey, "rsa", tokenClaims(now, "https://issuer.example"))
				claims, verifyErr := underTest.Verify(validJWT)
				require.NoError(t, verifyErr)
				require.NotNil(t, claims)

				unknownJWT := signToken(t, jose.RS256, rsaKey, "new-key", tokenClaims(now, "https://issuer.example"))
				_, verifyErr = underTest.Verify(unknownJWT)
				require.Error(t, verifyErr)
				assert.Contains(t, verifyErr.Error(), "unknown kid")
			}
			assert.Equal(t, tt.expectDiscovery, fixture.discoveryCalls)
			assert.Equal(t, tt.expectJWKS, fixture.jwksCalls)
		})
	}
}

func TestNewVerifierInvalidArguments(t *testing.T) {
	tests := []struct {
		name        string
		reader      client.Reader
		opts        Options
		expectError string
	}{
		{name: "nil reader", opts: defaultOptions(), expectError: "must not be nil"},
		{name: "invalid URL", reader: fake.NewClientBuilder().Build(), opts: Options{DiscoveryURL: "http://issuer.example"}, expectError: "absolute HTTPS URL"},
		{name: "negative timeout", reader: fake.NewClientBuilder().Build(), opts: Options{HTTPTimeout: -time.Second}, expectError: "HTTP timeout"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			underTest, err := NewVerifier(context.Background(), tt.reader, tt.opts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
			assert.Nil(t, underTest)
		})
	}
}

func loaderReader(t *testing.T, server *httptest.Server, configMapMode string) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	builder := fake.NewClientBuilder().WithScheme(scheme)
	if configMapMode == "missing" {
		return builder.Build()
	}

	caData := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	data := map[string]string{testCAKey: caData}
	switch configMapMode {
	case "key-missing":
		data = map[string]string{}
	case "bad-ca":
		data[testCAKey] = "not a certificate"
	}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: testCANamespace, Name: testCAName},
		Data:       data,
	}
	return builder.WithObjects(configMap).Build()
}

func TestFetchJSON(t *testing.T) {
	tests := []struct {
		name        string
		handler     http.Handler
		context     func() (context.Context, context.CancelFunc)
		expectError string
	}{
		{
			name: "cancelled context",
			handler: http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				_, _ = response.Write([]byte(`{"value":"ok"}`))
			}),
			context: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
			expectError: "send request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()
			ctx, cancel := tt.context()
			defer cancel()
			var destination map[string]string
			err := fetchJSON(ctx, server.Client(), server.URL, 1024, &destination)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
		})
	}
}
