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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/openkruise/agents/pkg/identity/jwtissuer"
)

// writeSigningKey writes a usable PEM private key and returns its path.
func writeSigningKey(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal signing key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "signing.key")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write signing key: %v", err)
	}
	return path
}

// writeServingPair writes a self-signed certificate and key, returning both
// paths.
func writeServingPair(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate serving key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "issuer.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"issuer.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatalf("create serving certificate: %v", err)
	}

	dir := t.TempDir()
	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write serving certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal serving key: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write serving key: %v", err)
	}
	return certFile, keyFile
}

// TestAddIssuer covers the configuration contract. The cases that matter are
// the refusals: issuance is opt-in, and once opted in it must not come up
// without TLS, because the JWKS decides which keys every verifier trusts.
func TestAddIssuer(t *testing.T) {
	signingKey := writeSigningKey(t)
	certFile, keyFile := writeServingPair(t)

	tests := []struct {
		name string
		cfg  IssuerConfig
		// wantIssuer expects issuance to be enabled.
		wantIssuer bool
		wantErr    string
	}{
		{
			name:       "no signing key leaves issuance disabled",
			cfg:        IssuerConfig{},
			wantIssuer: false,
		},
		{
			name: "a signing key without TLS is refused",
			cfg: IssuerConfig{
				IssuerURL:      "https://issuer.test",
				SigningKeyFile: signingKey,
			},
			wantErr: "TLS certificate and key are required",
		},
		{
			name: "a certificate without its key is refused",
			cfg: IssuerConfig{
				IssuerURL:      "https://issuer.test",
				SigningKeyFile: signingKey,
				TLSCertFile:    certFile,
			},
			wantErr: "TLS certificate and key are required",
		},
		{
			name: "a signing key without an issuer URL is refused",
			cfg: IssuerConfig{
				SigningKeyFile: signingKey,
				TLSCertFile:    certFile,
				TLSKeyFile:     keyFile,
			},
			wantErr: "issuer URL is required",
		},
		{
			name: "a plaintext issuer URL is refused",
			cfg: IssuerConfig{
				IssuerURL:      "http://issuer.test",
				SigningKeyFile: signingKey,
				TLSCertFile:    certFile,
				TLSKeyFile:     keyFile,
			},
			wantErr: "build issuer",
		},
		{
			name: "a missing signing key file is refused",
			cfg: IssuerConfig{
				IssuerURL:      "https://issuer.test",
				SigningKeyFile: filepath.Join(t.TempDir(), "absent.key"),
				TLSCertFile:    certFile,
				TLSKeyFile:     keyFile,
			},
			wantErr: "load signing key",
		},
		{
			name: "a missing certificate fails startup rather than every handshake",
			cfg: IssuerConfig{
				IssuerURL:      "https://issuer.test",
				SigningKeyFile: signingKey,
				TLSCertFile:    filepath.Join(t.TempDir(), "absent.crt"),
				TLSKeyFile:     keyFile,
			},
			wantErr: "watch issuer serving certificate",
		},
		{
			name: "a full configuration enables issuance",
			cfg: IssuerConfig{
				IssuerURL:      "https://issuer.test",
				SigningKeyFile: signingKey,
				TLSCertFile:    certFile,
				TLSKeyFile:     keyFile,
				BindAddress:    "127.0.0.1:0",
			},
			wantIssuer: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issuer, err := AddIssuer(newTestManager(t), tt.cfg)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error: got %q, want it to contain %q", err, tt.wantErr)
				}
				if issuer != nil {
					t.Error("a refused configuration must not return an issuer")
				}
				return
			}

			if err != nil {
				t.Fatalf("AddIssuer: unexpected error: %v", err)
			}
			if tt.wantIssuer && issuer == nil {
				t.Error("expected issuance to be enabled, got a nil issuer")
			}
			if !tt.wantIssuer && issuer != nil {
				t.Error("expected issuance to be disabled, got an issuer")
			}
		})
	}
}

// TestAddIssuerKeyIDIsDerived checks that two replicas loading the same key
// material agree on the same kid without coordinating, which is what lets a
// verifier match a token header against the published JWKS.
func TestAddIssuerKeyIDIsDerived(t *testing.T) {
	signingKey := writeSigningKey(t)
	certFile, keyFile := writeServingPair(t)
	cfg := IssuerConfig{
		IssuerURL:      "https://issuer.test",
		SigningKeyFile: signingKey,
		TLSCertFile:    certFile,
		TLSKeyFile:     keyFile,
		BindAddress:    "127.0.0.1:0",
	}

	first, err := AddIssuer(newTestManager(t), cfg)
	if err != nil {
		t.Fatalf("first AddIssuer: %v", err)
	}
	second, err := AddIssuer(newTestManager(t), cfg)
	if err != nil {
		t.Fatalf("second AddIssuer: %v", err)
	}

	firstSet, err := first.PublicKeySet()
	if err != nil {
		t.Fatalf("first key set: %v", err)
	}
	secondSet, err := second.PublicKeySet()
	if err != nil {
		t.Fatalf("second key set: %v", err)
	}
	if len(firstSet.Keys) != 1 || len(secondSet.Keys) != 1 {
		t.Fatalf("expected one key each, got %d and %d", len(firstSet.Keys), len(secondSet.Keys))
	}
	if firstSet.Keys[0].KeyID != secondSet.Keys[0].KeyID {
		t.Errorf("replicas disagree on the kid: %q and %q",
			firstSet.Keys[0].KeyID, secondSet.Keys[0].KeyID)
	}
}

// TestServeServesTheJWKSOverTLS exercises the listener for real: it fetches
// discovery and the key set over TLS, then confirms cancelling the context
// drains the server. Nothing else proves the endpoints are reachable, only that
// the configuration was accepted.
func TestServeServesTheJWKSOverTLS(t *testing.T) {
	certFile, keyFile := writeServingPair(t)
	signingKey, err := jwtissuer.LoadSigningKeyFromFile(writeSigningKey(t))
	if err != nil {
		t.Fatalf("load signing key: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	baseURL := "https://" + listener.Addr().String()

	issuer, err := jwtissuer.New(baseURL, signingKey)
	if err != nil {
		t.Fatalf("build issuer: %v", err)
	}
	handler, err := issuer.Handler()
	if err != nil {
		t.Fatalf("build endpoints: %v", err)
	}
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("load serving certificate: %v", err)
	}
	server := &http.Server{
		Handler:           handler,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: readHeaderTimeout,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, server, listener) }()

	// The certificate is self-signed, so the client is given it explicitly
	// rather than disabling verification.
	pool := x509.NewCertPool()
	pemCert, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("read serving certificate: %v", err)
	}
	if !pool.AppendCertsFromPEM(pemCert) {
		t.Fatal("serving certificate is not valid PEM")
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "issuer.test", MinVersion: tls.VersionTLS12},
	}}

	var discovery struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	fetchJSON(t, client, baseURL+jwtissuer.DiscoveryPath, &discovery)
	if discovery.Issuer != baseURL {
		t.Errorf("discovered issuer: got %q, want %q", discovery.Issuer, baseURL)
	}

	var keySet struct {
		Keys []struct {
			KeyID string `json:"kid"`
			D     string `json:"d"`
		} `json:"keys"`
	}
	fetchJSON(t, client, discovery.JWKSURI, &keySet)
	if len(keySet.Keys) != 1 {
		t.Fatalf("expected one published key, got %d", len(keySet.Keys))
	}
	if keySet.Keys[0].KeyID != signingKey.KeyID {
		t.Errorf("published kid: got %q, want %q", keySet.Keys[0].KeyID, signingKey.KeyID)
	}
	if keySet.Keys[0].D != "" {
		t.Error("the JWKS must never publish private key material")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve returned an error on shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after the context was cancelled")
	}
}

func fetchJSON(t *testing.T, client *http.Client, url string, into any) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

// TestAddIssuerFailsOnAnOccupiedPort covers binding in AddIssuer rather than in
// the Runnable: a port already in use must fail startup, not surface later from
// a goroutine nobody is watching.
func TestAddIssuerFailsOnAnOccupiedPort(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = occupied.Close() }()

	certFile, keyFile := writeServingPair(t)
	issuer, err := AddIssuer(newTestManager(t), IssuerConfig{
		IssuerURL:      "https://issuer.test",
		SigningKeyFile: writeSigningKey(t),
		TLSCertFile:    certFile,
		TLSKeyFile:     keyFile,
		BindAddress:    occupied.Addr().String(),
	})
	if err == nil {
		t.Fatal("expected an occupied port to fail startup")
	}
	if !strings.Contains(err.Error(), "listen for issuer endpoints") {
		t.Errorf("error: got %q, want it to mention the listen failure", err)
	}
	if issuer != nil {
		t.Error("a failed AddIssuer must not return an issuer")
	}
}

// TestIssuerRunnablesRunOnEveryReplica covers the leader-election behaviour of
// the registered runnables.
//
// A bare manager.RunnableFunc does not implement LeaderElectionRunnable, and
// the manager's default for anything that does not is the leader-election
// group. Since every replica binds the issuer port at startup and /readyz
// answers before the lease is held, a leader-only server means a non-leader
// joins Service endpoints and then accepts connections it never answers.
func TestIssuerRunnablesRunOnEveryReplica(t *testing.T) {
	runnable := everyReplica(func(context.Context) error { return nil })

	elective, ok := runnable.(manager.LeaderElectionRunnable)
	if !ok {
		t.Fatal("the runnable must implement LeaderElectionRunnable, " +
			"or the manager defaults it into the leader-election group")
	}
	if elective.NeedLeaderElection() {
		t.Error("the issuer endpoints must serve from every replica, not only the leader")
	}

	// A bare RunnableFunc is what this exists to avoid: it does not implement
	// the interface at all, which is how it ends up leader-only. If a future
	// controller-runtime changes that, everyReplica stops being necessary and
	// this says so rather than leaving the wrapper unexplained.
	var bare manager.Runnable = manager.RunnableFunc(func(context.Context) error { return nil })
	if _, implements := bare.(manager.LeaderElectionRunnable); implements {
		t.Error("manager.RunnableFunc now implements LeaderElectionRunnable; " +
			"re-check whether everyReplica is still needed")
	}
}
