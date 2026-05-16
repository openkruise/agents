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

// Package credential provides a client for communicating with the
// credential provider API to obtain tokens.
package credential

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openkruise/agents/pkg/traffix-extension/framework/tokencache"
	logutil "github.com/openkruise/agents/pkg/traffix-extension/util/logging"
)

const (
	defaultIdentityProviderURL = "https://identity-provider.ack-agent-identity.svc.cluster.local:8443/"
	identityProviderURLEnvVar  = "IDENTITY_PROVIDER_URL"
	apiActionGetResourceAPIKey = "GetResourceApiKey"

	// Environment variables for mTLS certificate and key paths.
	clientCertEnvVar = "CREDENTIAL_PROVIDER_CLIENT_CERT_PATH"
	clientKeyEnvVar  = "CREDENTIAL_PROVIDER_CLIENT_KEY_PATH"
	caCertEnvVar     = "CREDENTIAL_PROVIDER_CA_CERT_PATH"

	// Default paths when env vars are not set.
	defaultClientCertPath = "/etc/traffix-extension/mtls/client.crt"
	defaultClientKeyPath  = "/etc/traffix-extension/mtls/client.key"
	defaultCACertPath     = "/etc/traffix-extension/mtls/ca.crt"
)

// TokenResponse is the expected response structure from the credential provider.
type TokenResponse struct {
	ApiKey string `json:"apiKey"`
	// Additional fields may be present depending on the API response.
}

// Client is a thread-safe HTTP client for communicating with the
// credential provider.
type Client struct {
	httpClient  *http.Client
	providerURL string
	once        sync.Once
	cache       *tokencache.Cache
}

// NewClient creates a new credential provider client.
func NewClient() *Client {
	return NewClientWithCache(nil)
}

// NewClientWithCache creates a new credential provider client with an optional token cache.
// If cache is nil, tokens are fetched from the provider on every request.
func NewClientWithCache(cache *tokencache.Cache) *Client {
	c := &Client{}
	c.once.Do(func() {
		providerURL := os.Getenv(identityProviderURLEnvVar)
		if providerURL == "" {
			providerURL = defaultIdentityProviderURL
		}
		c.providerURL = providerURL
		c.httpClient = buildHTTPClient()
		c.cache = cache
	})
	return c
}

// buildHTTPClient constructs an HTTP client, preferring mTLS when client
// certificate and key files are available. When mTLS material is missing
// or invalid, the client falls back to a default-TLS client that still
// validates the server certificate against the system trust store — it
// just does not present a client certificate.
func buildHTTPClient() *http.Client {
	certPath := os.Getenv(clientCertEnvVar)
	if certPath == "" {
		certPath = defaultClientCertPath
	}
	keyPath := os.Getenv(clientKeyEnvVar)
	if keyPath == "" {
		keyPath = defaultClientKeyPath
	}
	caPath := os.Getenv(caCertEnvVar)
	if caPath == "" {
		caPath = defaultCACertPath
	}

	if client, err := newMTLSClient(certPath, keyPath, caPath); err == nil {
		return client
	}
	return newDefaultTLSClient()
}

// newMTLSClient loads a client certificate, key, and CA cert, then returns an
// HTTP client configured for mTLS with server CA verification.
func newMTLSClient(certPath, keyPath, caCertPath string) (*http.Client, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read client cert %s: %w", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read client key %s: %w", keyPath, err)
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("failed to parse client certificate: %w", err)
	}

	caCertPool, err := loadCACertPool(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load CA cert %s: %w", caCertPath, err)
	}

	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				RootCAs:      caCertPool,
				MinVersion:   tls.VersionTLS12,
			},
		},
	}, nil
}

// loadCACertPool reads a CA certificate file and returns a CertPool.
func loadCACertPool(caCertPath string) (*x509.CertPool, error) {
	caCertPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA cert %s: %w", caCertPath, err)
	}

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCertPEM) {
		return nil, fmt.Errorf("failed to parse CA cert from %s", caCertPath)
	}
	return caCertPool, nil
}

// newDefaultTLSClient returns an HTTP client with a default TLS transport
// (no client certificate). Server certificates are verified against the
// system trust store; TLS 1.2 is the minimum version. This is the fallback
// used when mTLS material is unavailable — it does NOT skip verification.
func newDefaultTLSClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
		},
	}
}

// SandboxToken represents the JSON structure of filter_state['sandbox.token'].
// The sandbox.token is a JSON string containing accessToken, sandboxClientId, and requestId.
type SandboxToken struct {
	RequestID       string `json:"requestId"`
	AccessToken     string `json:"accessToken"`
	SandboxClientID string `json:"sandboxClientId"`
}

// GetTokenRequest is the request body for GetResourceApiKey action.
type GetTokenRequest struct {
	ResourceID             string `json:"resourceId"`
	CredentialProviderName string `json:"credentialProviderName"`
}

// GetToken retrieves a token from the identity provider.
// accessToken is used for Authorization header to the identity provider.
// sandboxClientID is used as the resourceId in the request body.
// credentialProviderName is the name from tokenProviderRef.name.
// If a cache is configured, the token is cached by credentialProviderName + sandboxClientID.
func (c *Client) GetToken(ctx context.Context, accessToken, sandboxClientID, credentialProviderName string) (string, error) {
	logger := log.FromContext(ctx)

	// Check cache first.
	if c.cache != nil {
		if token, ok := c.cache.Get(credentialProviderName, sandboxClientID); ok {
			logger.V(logutil.DEBUG).Info("Token retrieved from cache",
				"credentialProvider", credentialProviderName)
			return token, nil
		}
	}

	reqBody := GetTokenRequest{
		ResourceID:             sandboxClientID,
		CredentialProviderName: credentialProviderName,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal token request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.providerURL, bytes.NewReader(jsonData))
	if err != nil {
		return "", fmt.Errorf("failed to create token request: %w", err)
	}

	httpReq.Header.Set("Authorization", "Bearer "+accessToken)
	httpReq.Header.Set("X-Api-Action-Name", apiActionGetResourceAPIKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("failed to send token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		logger.V(logutil.DEFAULT).Info("Token request failed",
			"status", resp.StatusCode,
			"body", string(body))
		return "", fmt.Errorf("token request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp TokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("failed to unmarshal token response: %w", err)
	}

	if tokenResp.ApiKey == "" {
		return "", fmt.Errorf("empty token returned from identity provider")
	}

	// Cache the token.
	if c.cache != nil {
		c.cache.Set(credentialProviderName, sandboxClientID, tokenResp.ApiKey)
	}

	logger.V(logutil.DEBUG).Info("Token retrieved successfully",
		"credentialProvider", credentialProviderName)

	return tokenResp.ApiKey, nil
}
