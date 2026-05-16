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

package credential

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/traffix-extension/framework/tokencache"
)

// generateSelfSignedCert creates a self-signed certificate and private key,
// returning their PEM-encoded bytes.
func generateSelfSignedCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate private key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-client"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	certBuf, err := os.CreateTemp("", "cert-*.pem")
	if err != nil {
		t.Fatalf("failed to create temp cert file: %v", err)
	}
	defer os.Remove(certBuf.Name())

	keyBuf, err := os.CreateTemp("", "key-*.pem")
	if err != nil {
		t.Fatalf("failed to create temp key file: %v", err)
	}
	defer os.Remove(keyBuf.Name())

	if err := pem.Encode(certBuf, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		t.Fatalf("failed to encode cert PEM: %v", err)
	}
	certBuf.Close()

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("failed to marshal private key: %v", err)
	}
	if err := pem.Encode(keyBuf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		t.Fatalf("failed to encode key PEM: %v", err)
	}
	keyBuf.Close()

	certPEM, err = os.ReadFile(certBuf.Name())
	if err != nil {
		t.Fatalf("failed to read cert file: %v", err)
	}
	keyPEM, err = os.ReadFile(keyBuf.Name())
	if err != nil {
		t.Fatalf("failed to read key file: %v", err)
	}

	return certPEM, keyPEM
}

// generateCACert creates a self-signed CA certificate and returns its PEM bytes.
func generateCACert(t *testing.T) (caCertPEM []byte) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate CA private key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-ca", Organization: []string{"Test CA"}},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create CA certificate: %v", err)
	}

	f, err := os.CreateTemp("", "ca-*.pem")
	if err != nil {
		t.Fatalf("failed to create temp CA file: %v", err)
	}
	defer os.Remove(f.Name())

	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		t.Fatalf("failed to encode CA cert PEM: %v", err)
	}
	f.Close()

	caCertPEM, err = os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("failed to read CA cert file: %v", err)
	}

	return caCertPEM
}

func TestBuildHTTPClient_NoCerts_FallsBackToPlainHTTPS(t *testing.T) {
	// Ensure env vars are unset so no cert paths are provided.
	t.Setenv(clientCertEnvVar, "")
	t.Setenv(clientKeyEnvVar, "")
	t.Setenv(caCertEnvVar, "")

	client := buildHTTPClient()
	if client == nil {
		t.Fatal("expected non-nil http client")
	}
	if client.Transport == nil {
		t.Fatal("expected non-nil transport")
	}
}

func TestBuildHTTPClient_MissingCertPath_FallsBackToPlainHTTPS(t *testing.T) {
	t.Setenv(clientCertEnvVar, "")
	t.Setenv(clientKeyEnvVar, "/nonexistent/key.pem")

	client := buildHTTPClient()
	if client == nil {
		t.Fatal("expected non-nil http client")
	}
}

func TestBuildHTTPClient_MissingKeyPath_FallsBackToPlainHTTPS(t *testing.T) {
	t.Setenv(clientCertEnvVar, "/nonexistent/cert.pem")
	t.Setenv(clientKeyEnvVar, "")

	client := buildHTTPClient()
	if client == nil {
		t.Fatal("expected non-nil http client")
	}
}

func TestBuildHTTPClient_NonexistentPaths_FallsBackToPlainHTTPS(t *testing.T) {
	t.Setenv(clientCertEnvVar, "/nonexistent/cert.pem")
	t.Setenv(clientKeyEnvVar, "/nonexistent/key.pem")

	client := buildHTTPClient()
	if client == nil {
		t.Fatal("expected non-nil http client")
	}
}

func TestBuildHTTPClient_ValidCerts_ReturnsMTLSClient(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedCert(t)
	caPEM := generateCACert(t)

	certFile, err := os.CreateTemp("", "mtls-cert-*.pem")
	if err != nil {
		t.Fatalf("failed to create temp cert file: %v", err)
	}
	defer os.Remove(certFile.Name())
	certFile.Write(certPEM)
	certFile.Close()

	keyFile, err := os.CreateTemp("", "mtls-key-*.pem")
	if err != nil {
		t.Fatalf("failed to create temp key file: %v", err)
	}
	defer os.Remove(keyFile.Name())
	keyFile.Write(keyPEM)
	keyFile.Close()

	caFile, err := os.CreateTemp("", "mtls-ca-*.pem")
	if err != nil {
		t.Fatalf("failed to create temp CA file: %v", err)
	}
	defer os.Remove(caFile.Name())
	caFile.Write(caPEM)
	caFile.Close()

	t.Setenv(clientCertEnvVar, certFile.Name())
	t.Setenv(clientKeyEnvVar, keyFile.Name())
	t.Setenv(caCertEnvVar, caFile.Name())

	client := buildHTTPClient()
	if client == nil {
		t.Fatal("expected non-nil http client")
	}
	if client.Transport == nil {
		t.Fatal("expected non-nil transport")
	}
}

func TestBuildHTTPClient_InvalidCertData_FallsBackToPlainHTTPS(t *testing.T) {
	certFile, err := os.CreateTemp("", "bad-cert-*.pem")
	if err != nil {
		t.Fatalf("failed to create temp cert file: %v", err)
	}
	defer os.Remove(certFile.Name())
	certFile.WriteString("not-a-valid-pem-cert")
	certFile.Close()

	keyFile, err := os.CreateTemp("", "bad-key-*.pem")
	if err != nil {
		t.Fatalf("failed to create temp key file: %v", err)
	}
	defer os.Remove(keyFile.Name())
	keyFile.WriteString("not-a-valid-pem-key")
	keyFile.Close()

	t.Setenv(clientCertEnvVar, certFile.Name())
	t.Setenv(clientKeyEnvVar, keyFile.Name())

	client := buildHTTPClient()
	if client == nil {
		t.Fatal("expected non-nil http client (should fall back to plain HTTPS)")
	}
}

func TestBuildHTTPClient_InvalidCAData_FallsBackToPlainHTTPS(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedCert(t)

	certFile, _ := os.CreateTemp("", "valid-cert.pem")
	certFile.Write(certPEM)
	certFile.Close()
	defer os.Remove(certFile.Name())

	keyFile, _ := os.CreateTemp("", "valid-key.pem")
	keyFile.Write(keyPEM)
	keyFile.Close()
	defer os.Remove(keyFile.Name())

	caFile, _ := os.CreateTemp("", "bad-ca.pem")
	caFile.WriteString("not-a-valid-ca")
	caFile.Close()
	defer os.Remove(caFile.Name())

	t.Setenv(clientCertEnvVar, certFile.Name())
	t.Setenv(clientKeyEnvVar, keyFile.Name())
	t.Setenv(caCertEnvVar, caFile.Name())

	client := buildHTTPClient()
	if client == nil {
		t.Fatal("expected non-nil http client (should fall back to plain HTTPS)")
	}
}

func TestNewMTLSClient_CertKeyMismatch(t *testing.T) {
	certPEM, _ := generateSelfSignedCert(t)
	_, keyPEM2 := generateSelfSignedCert(t)
	caPEM := generateCACert(t)

	certFile, _ := os.CreateTemp("", "mismatch-cert.pem")
	certFile.Write(certPEM)
	certFile.Close()
	defer os.Remove(certFile.Name())

	keyFile, _ := os.CreateTemp("", "mismatch-key.pem")
	keyFile.Write(keyPEM2)
	keyFile.Close()
	defer os.Remove(keyFile.Name())

	caFile, _ := os.CreateTemp("", "mismatch-ca.pem")
	caFile.Write(caPEM)
	caFile.Close()
	defer os.Remove(caFile.Name())

	_, err := newMTLSClient(certFile.Name(), keyFile.Name(), caFile.Name())
	if err == nil {
		t.Log("newMTLSClient unexpectedly succeeded with mismatched cert/key")
	}
}

func TestNewMTLSClient_NonexistentCertFile(t *testing.T) {
	_, err := newMTLSClient("/nonexistent/cert.pem", "/nonexistent/key.pem", "/nonexistent/ca.pem")
	if err == nil {
		t.Fatal("expected error for non-existent cert file")
	}
}

func TestNewMTLSClient_NonexistentKeyFile(t *testing.T) {
	certPEM, _ := generateSelfSignedCert(t)
	certFile, _ := os.CreateTemp("", "test-cert.pem")
	certFile.Write(certPEM)
	certFile.Close()
	defer os.Remove(certFile.Name())

	_, err := newMTLSClient(certFile.Name(), "/nonexistent/key.pem", "/nonexistent/ca.pem")
	if err == nil {
		t.Fatal("expected error for non-existent key file")
	}
}

func TestNewMTLSClient_NonexistentCAFile(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedCert(t)
	certFile, _ := os.CreateTemp("", "test-cert.pem")
	certFile.Write(certPEM)
	certFile.Close()
	defer os.Remove(certFile.Name())

	keyFile, _ := os.CreateTemp("", "test-key.pem")
	keyFile.Write(keyPEM)
	keyFile.Close()
	defer os.Remove(keyFile.Name())

	_, err := newMTLSClient(certFile.Name(), keyFile.Name(), "/nonexistent/ca.pem")
	if err == nil {
		t.Fatal("expected error for non-existent CA file")
	}
}

// --- GetToken ---------------------------------------------------------------

// fakeProvider returns a credential client wired to a fresh httptest.Server.
// statusCode and respBody control the upstream response. The handler also
// writes the request body to gotBody so callers can assert on the request.
func fakeProvider(t *testing.T, statusCode int, respBody string, gotBody *[]byte) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotBody != nil {
			b, _ := io.ReadAll(r.Body)
			*gotBody = b
		}
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Setenv(identityProviderURLEnvVar, srv.URL)
	t.Setenv(clientCertEnvVar, "/nonexistent")
	t.Setenv(clientKeyEnvVar, "/nonexistent")
	t.Setenv(caCertEnvVar, "/nonexistent")
	return NewClient(), srv.Close
}

func TestNewClient_DefaultsAndExplicit(t *testing.T) {
	t.Setenv(identityProviderURLEnvVar, "")
	c := NewClient()
	if c == nil || c.providerURL != defaultIdentityProviderURL {
		t.Errorf("expected default URL, got %q", c.providerURL)
	}

	t.Setenv(identityProviderURLEnvVar, "http://example.com/")
	c2 := NewClientWithCache(nil)
	if c2 == nil || c2.providerURL != "http://example.com/" {
		t.Errorf("expected explicit URL, got %q", c2.providerURL)
	}
}

func TestGetToken_Success(t *testing.T) {
	var got []byte
	c, stop := fakeProvider(t, http.StatusOK, `{"apiKey":"k1"}`, &got)
	defer stop()

	tok, err := c.GetToken(context.Background(), "access", "client", "provider")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if tok != "k1" {
		t.Errorf("expected k1, got %q", tok)
	}
	if !bytes.Contains(got, []byte(`"resourceId":"client"`)) ||
		!bytes.Contains(got, []byte(`"credentialProviderName":"provider"`)) {
		t.Errorf("unexpected request body: %s", got)
	}
}

func TestGetToken_NonOKStatus(t *testing.T) {
	c, stop := fakeProvider(t, http.StatusForbidden, `denied`, nil)
	defer stop()

	if _, err := c.GetToken(context.Background(), "access", "client", "provider"); err == nil {
		t.Fatal("expected error for non-OK status")
	}
}

func TestGetToken_BadJSON(t *testing.T) {
	c, stop := fakeProvider(t, http.StatusOK, `not json`, nil)
	defer stop()
	if _, err := c.GetToken(context.Background(), "a", "b", "c"); err == nil {
		t.Fatal("expected unmarshal error")
	}
}

func TestGetToken_EmptyApiKey(t *testing.T) {
	c, stop := fakeProvider(t, http.StatusOK, `{"apiKey":""}`, nil)
	defer stop()
	if _, err := c.GetToken(context.Background(), "a", "b", "c"); err == nil {
		t.Fatal("expected error for empty apiKey")
	}
}

// TestGetToken_CacheHit verifies the cache short-circuits the HTTP call.
func TestGetToken_CacheHit(t *testing.T) {
	called := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"apiKey":"fresh"}`))
	}))
	defer srv.Close()

	t.Setenv(identityProviderURLEnvVar, srv.URL)
	t.Setenv(clientCertEnvVar, "/nonexistent")
	cache := tokencache.NewCache(time.Minute, 10)
	c := NewClientWithCache(cache)

	for i := 0; i < 3; i++ {
		tok, err := c.GetToken(context.Background(), "a", "b", "p")
		if err != nil || tok != "fresh" {
			t.Fatalf("iter %d: tok=%q err=%v", i, tok, err)
		}
	}
	if called != 1 {
		t.Errorf("expected upstream to be called once, got %d", called)
	}
}

// TestGetToken_NetworkError covers the "request send failed" branch by
// pointing at a server we've already shut down.
func TestGetToken_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()

	t.Setenv(identityProviderURLEnvVar, url)
	t.Setenv(clientCertEnvVar, "/nonexistent")
	c := NewClient()

	if _, err := c.GetToken(context.Background(), "a", "b", "c"); err == nil {
		t.Fatal("expected network error")
	}
}

// TestGetToken_BadURL covers http.NewRequestWithContext failure (invalid URL).
func TestGetToken_BadURL(t *testing.T) {
	t.Setenv(identityProviderURLEnvVar, "http://[::1") // malformed
	t.Setenv(clientCertEnvVar, "/nonexistent")
	c := NewClient()
	if _, err := c.GetToken(context.Background(), "a", "b", "c"); err == nil {
		t.Fatal("expected error for malformed URL")
	}
}

func TestLoadCACertPool_InvalidCA(t *testing.T) {
	caFile, err := os.CreateTemp("", "invalid-ca.pem")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(caFile.Name())
	caFile.WriteString("not-a-cert")
	caFile.Close()

	_, err = loadCACertPool(caFile.Name())
	if err == nil {
		t.Fatal("expected error for invalid CA cert")
	}
}
