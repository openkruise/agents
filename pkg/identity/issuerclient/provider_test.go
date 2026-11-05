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

package issuerclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/identity/agenttoken"
	"github.com/openkruise/agents/pkg/peersecurity"
)

// testPKI is a throwaway CA with one server certificate and one client
// certificate, matching the deployment shape where both the identity provider
// and the control plane are issued by the same cluster CA.
type testPKI struct {
	caPEM         []byte
	serverCert    tls.Certificate
	clientCertPEM []byte
	clientKeyPEM  []byte
}

func newTestPKI(t *testing.T) testPKI {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}

	issue := func(serial int64, usage x509.ExtKeyUsage) ([]byte, *ecdsa.PrivateKey) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		template := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: "test"},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		}
		if usage == x509.ExtKeyUsageServerAuth {
			template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		}
		der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatalf("create certificate: %v", err)
		}
		return der, key
	}

	serverDER, serverKey := issue(2, x509.ExtKeyUsageServerAuth)
	clientDER, clientKey := issue(3, x509.ExtKeyUsageClientAuth)
	clientKeyDER, err := x509.MarshalPKCS8PrivateKey(clientKey)
	if err != nil {
		t.Fatalf("marshal client key: %v", err)
	}

	return testPKI{
		caPEM:         pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		serverCert:    tls.Certificate{Certificate: [][]byte{serverDER}, PrivateKey: serverKey},
		clientCertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER}),
		clientKeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: clientKeyDER}),
	}
}

func (p testPKI) bundle() *peersecurity.TLSBundle {
	return &peersecurity.TLSBundle{
		CABundle:      p.caPEM,
		ClientCertPEM: p.clientCertPEM,
		ClientKeyPEM:  p.clientKeyPEM,
	}
}

// newIssuanceServer serves handler over TLS and requires the client certificate
// the bundle carries, so a test that forgets to present one fails the handshake
// rather than reaching the handler.
func newIssuanceServer(t *testing.T, pki testPKI, handler http.Handler) *httptest.Server {
	t.Helper()
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(pki.caPEM) {
		t.Fatal("append CA to pool")
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{pki.serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func testSandbox() *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a",
		Name:      "sbx-1",
		UID:       types.UID("uid-1"),
	}}
}

func TestNew(t *testing.T) {
	pki := newTestPKI(t)
	caOnly := &peersecurity.TLSBundle{CABundle: pki.caPEM}

	tests := []struct {
		name    string
		opts    Options
		wantErr string
	}{
		{name: "requires an endpoint", opts: Options{Bundle: pki.bundle()}, wantErr: "endpoint is required"},
		{name: "requires HTTPS", opts: Options{Endpoint: "http://issuer.test", Bundle: pki.bundle()}, wantErr: "must be an absolute HTTPS URL"},
		{name: "requires a TLS bundle", opts: Options{Endpoint: "https://issuer.test"}, wantErr: "TLS bundle is required"},
		{name: "requires a client certificate", opts: Options{Endpoint: "https://issuer.test", Bundle: caOnly}, wantErr: "no client certificate"},
		{name: "accepts a complete configuration", opts: Options{Endpoint: "https://issuer.test", Bundle: pki.bundle()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.opts)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("New() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("New() error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestIssueAgentToken covers the request the service receives and the response
// the caller gets back. The body is asserted because the service re-reads the
// sandbox from these three fields; a wrong field would make it read the wrong
// object rather than fail loudly.
func TestIssueAgentToken(t *testing.T) {
	pki := newTestPKI(t)
	var gotPath, gotMethod string
	var gotBody agenttoken.Request
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("decode request body %q: %v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agentToken":"signed.jwt.value","expiresAt":"2030-01-01T00:00:00Z","requestId":"req-1"}`))
	})
	server := newIssuanceServer(t, pki, handler)

	provider, err := New(Options{Endpoint: server.URL + "/", Bundle: pki.bundle()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	resp, err := provider.IssueToken(context.Background(), testSandbox(), identity.TokenOptions{Kind: identity.TokenKindIDToken})
	if err != nil {
		t.Fatalf("IssueToken() error = %v", err)
	}

	if gotPath != agenttoken.Path {
		t.Errorf("path = %q, want %q", gotPath, agenttoken.Path)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	want := agenttoken.Request{Namespace: "team-a", SandboxName: "sbx-1", SandboxUID: "uid-1"}
	if gotBody != want {
		t.Errorf("request body = %+v, want %+v", gotBody, want)
	}
	if resp.AccessToken != "signed.jwt.value" || resp.RequestID != "req-1" ||
		resp.AccessTokenExpiration != "2030-01-01T00:00:00Z" {
		t.Errorf("response = %+v, not mapped from the service reply", resp)
	}
}

func TestIssueTokenRejectsASandboxWithoutUID(t *testing.T) {
	pki := newTestPKI(t)
	provider, err := New(Options{Endpoint: "https://issuer.test", Bundle: pki.bundle()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	sbx := testSandbox()
	sbx.UID = ""

	if _, err := provider.IssueToken(context.Background(), sbx, identity.TokenOptions{Kind: identity.TokenKindIDToken}); err == nil {
		t.Fatal("IssueToken() accepted a sandbox without a UID")
	}
	if _, err := provider.IssueToken(context.Background(), nil, identity.TokenOptions{Kind: identity.TokenKindIDToken}); err == nil {
		t.Fatal("IssueToken() accepted a nil sandbox")
	}
}

// TestIssueTokenReportsServiceRefusal checks the refusal body reaches the caller
// as an error naming the reason. These refusals are policy decisions, not
// transport failures, so the message is what operators act on.
func TestIssueTokenReportsServiceRefusal(t *testing.T) {
	pki := newTestPKI(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionFailed)
		_ = json.NewEncoder(w).Encode(agenttoken.Error{
			Code:    agenttoken.CodeAgentIdentityNotReady,
			Message: "AgentIdentity team-a/agent-a is not usable",
		})
	})
	server := newIssuanceServer(t, pki, handler)

	provider, err := New(Options{Endpoint: server.URL, Bundle: pki.bundle()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_, err = provider.IssueToken(context.Background(), testSandbox(), identity.TokenOptions{Kind: identity.TokenKindIDToken})
	if err == nil {
		t.Fatal("IssueToken() ignored a refusal")
	}
	for _, want := range []string{agenttoken.CodeAgentIdentityNotReady, "not usable", "412"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

func TestIssueTokenRejectsAnEmptyToken(t *testing.T) {
	pki := newTestPKI(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"agentToken":"","expiresAt":"2030-01-01T00:00:00Z"}`))
	})
	server := newIssuanceServer(t, pki, handler)

	provider, err := New(Options{Endpoint: server.URL, Bundle: pki.bundle()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := provider.IssueToken(context.Background(), testSandbox(),
		identity.TokenOptions{Kind: identity.TokenKindIDToken}); err == nil {
		t.Fatal("IssueToken() returned an empty token")
	}
}

// TestAccessTokensKeepTheDefaultProvider pins the scope boundary: gateway access
// tokens have no issuance endpoint yet, so asking for one must return the
// community placeholder without contacting the service.
func TestAccessTokensKeepTheDefaultProvider(t *testing.T) {
	pki := newTestPKI(t)
	calls := 0
	server := newIssuanceServer(t, pki, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	}))

	provider, err := New(Options{Endpoint: server.URL, Bundle: pki.bundle()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	resp, err := provider.IssueToken(context.Background(), testSandbox(), identity.TokenOptions{Kind: identity.TokenKindAccessToken})
	if err != nil {
		t.Fatalf("IssueToken() error = %v", err)
	}
	if calls != 0 {
		t.Errorf("the service was called %d times for an access token", calls)
	}
	if resp.AccessToken == "" || resp.AccessTokenExpiration == "" {
		t.Errorf("response = %+v, want the default provider's placeholder", resp)
	}
}

func TestIssueTokenRejectsAnUnknownKind(t *testing.T) {
	pki := newTestPKI(t)
	provider, err := New(Options{Endpoint: "https://issuer.test", Bundle: pki.bundle()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := provider.IssueToken(context.Background(), testSandbox(),
		identity.TokenOptions{Kind: identity.TokenKind("Toaster")}); err == nil {
		t.Fatal("IssueToken() accepted an unknown token kind")
	}
}
