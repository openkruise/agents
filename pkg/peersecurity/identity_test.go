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

package peersecurity

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestParseAllowedClientCNs(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "empty is unrestricted", want: []string{}},
		{name: "single", raw: "sandbox-manager", want: []string{"sandbox-manager"}},
		{name: "two names", raw: "sandbox-manager,sandbox-ingress-gateway", want: []string{"sandbox-manager", "sandbox-ingress-gateway"}},
		{name: "leading space is kept", raw: "sandbox-manager, sandbox-ingress-gateway", want: []string{"sandbox-manager", " sandbox-ingress-gateway"}},
		{name: "trailing comma", raw: "sandbox-manager,", want: []string{"sandbox-manager", ""}},
		{name: "comma only", raw: ",", want: []string{"", ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseAllowedClientCNs(tt.raw))
		})
	}
}

func TestClientIdentityAllowed(t *testing.T) {
	tests := []struct {
		name        string
		commonName  string
		dnsNames    []string
		uris        []string
		allowed     []string
		expectAllow bool
	}{
		{
			name:        "CN match",
			commonName:  "sandbox-manager",
			allowed:     []string{"sandbox-manager", "sandbox-ingress-gateway"},
			expectAllow: true,
		},
		{
			name:        "DNS SAN match while CN differs",
			commonName:  "some-issuer-specific-subject",
			dnsNames:    []string{"sandbox-ingress-gateway"},
			allowed:     []string{"sandbox-ingress-gateway"},
			expectAllow: true,
		},
		{
			name:        "CN match does not require a SAN hit",
			commonName:  "sandbox-manager",
			dnsNames:    []string{"other.example"},
			allowed:     []string{"sandbox-manager", "sandbox-ingress-gateway"},
			expectAllow: true,
		},
		{
			name:        "case differs",
			commonName:  "Sandbox-Manager",
			allowed:     []string{"sandbox-manager"},
			expectAllow: false,
		},
		{
			name:        "URI SAN is ignored",
			commonName:  "other-client",
			uris:        []string{"spiffe://cluster.local/sandbox-manager"},
			allowed:     []string{"sandbox-manager"},
			expectAllow: false,
		},
		{
			name:        "neither CN nor SAN matches",
			commonName:  "other-client",
			dnsNames:    []string{"also-not-allowed"},
			allowed:     []string{"sandbox-manager"},
			expectAllow: false,
		},
		{
			name:        "empty allow-list entry never matches",
			commonName:  "",
			allowed:     []string{""},
			expectAllow: false,
		},
		{
			name:        "leading space does not match",
			commonName:  "sandbox-ingress-gateway",
			allowed:     []string{"sandbox-manager", " sandbox-ingress-gateway"},
			expectAllow: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			leaf := &x509.Certificate{
				Subject:  pkix.Name{CommonName: tt.commonName},
				DNSNames: tt.dnsNames,
			}
			for _, raw := range tt.uris {
				parsed, err := url.Parse(raw)
				require.NoError(t, err)
				leaf.URIs = append(leaf.URIs, parsed)
			}
			assert.Equal(t, tt.expectAllow, clientIdentityAllowed(leaf, tt.allowed))
		})
	}
}

func TestAuthorizePeerClient(t *testing.T) {
	allowed := []string{"sandbox-manager"}
	leaf := &x509.Certificate{Subject: pkix.Name{CommonName: "sandbox-manager"}}
	intruder := &x509.Certificate{Subject: pkix.Name{CommonName: "intruder"}}

	require.NoError(t, authorizePeerClient(tls.ConnectionState{}, allowed),
		"a connection without a client certificate must stay usable for Gateway probes")

	require.NoError(t, authorizePeerClient(tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{leaf},
		VerifiedChains:   [][]*x509.Certificate{{leaf}},
	}, allowed))

	err := authorizePeerClient(tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{intruder},
		VerifiedChains:   [][]*x509.Certificate{{intruder}},
	}, allowed)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "intruder")

	require.Error(t, authorizePeerClient(tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{leaf},
	}, allowed), "a presented certificate without a verified chain must be refused")
}

func TestAttachClientIdentityAllowlist(t *testing.T) {
	cfg := &tls.Config{}
	attachClientIdentityAllowlist(cfg, "")
	assert.Nil(t, cfg.VerifyConnection)

	attachClientIdentityAllowlist(cfg, ",")
	require.NotNil(t, cfg.VerifyConnection)
}

func TestLoadClientIdentityAllowlist(t *testing.T) {
	serverSecret, clientSecret := mustPeerTLSSecrets(t)
	reader := fake.NewClientBuilder().WithObjects(serverSecret, clientSecret).Build()
	inputs := tlsInputs("ns/server", "ns/client")

	_, serverTLS, clientTLS, err := Load(t.Context(), reader, inputs)
	require.NoError(t, err)
	assert.Nil(t, serverTLS.VerifyConnection)
	assert.Nil(t, clientTLS.VerifyConnection)

	inputs.AllowedClientCNs = "sandbox-manager"
	_, serverTLS, clientTLS, err = Load(t.Context(), reader, inputs)
	require.NoError(t, err)
	require.NotNil(t, serverTLS.VerifyConnection)
	assert.Nil(t, clientTLS.VerifyConnection)

	_, _, _, err = Load(t.Context(), fake.NewClientBuilder().Build(), Inputs{AllowedClientCNs: "sandbox-manager"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "peer allowed client CNs require peer TLS")
}

func TestPeerClientIdentityHandshake(t *testing.T) {
	caCert, caKey, caPEM := mustCA(t)
	serverCertPEM, serverKeyPEM := mustIssuedNamed(t, caCert, caKey, x509.ExtKeyUsageServerAuth, "peer-server", []string{serverName})
	managerCertPEM, managerKeyPEM := mustIssuedNamed(t, caCert, caKey, x509.ExtKeyUsageClientAuth, "sandbox-manager", nil)
	otherCertPEM, otherKeyPEM := mustIssuedNamed(t, caCert, caKey, x509.ExtKeyUsageClientAuth, "other-client", nil)
	sanCertPEM, sanKeyPEM := mustIssuedNamed(t, caCert, caKey, x509.ExtKeyUsageClientAuth, "other-subject", []string{"sandbox-ingress-gateway"})
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(caPEM))

	serverCert := mustTLSCert(t, serverCertPEM, serverKeyPEM)
	managerCert := mustTLSCert(t, managerCertPEM, managerKeyPEM)
	otherCert := mustTLSCert(t, otherCertPEM, otherKeyPEM)
	sanCert := mustTLSCert(t, sanCertPEM, sanKeyPEM)

	newServer := func(raw string, clientAuth tls.ClientAuthType) *tls.Config {
		cfg := &tls.Config{
			Certificates: []tls.Certificate{serverCert},
			ClientCAs:    pool,
			ClientAuth:   clientAuth,
			MinVersion:   tls.VersionTLS12,
			MaxVersion:   tls.VersionTLS12,
		}
		attachClientIdentityAllowlist(cfg, raw)
		return cfg
	}
	newClient := func(cert tls.Certificate) *tls.Config {
		return &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			ServerName:   serverName,
			MinVersion:   tls.VersionTLS12,
			MaxVersion:   tls.VersionTLS12,
		}
	}

	t.Run("CN match", func(t *testing.T) {
		require.NoError(t, tlsHandshake(t, newServer("sandbox-manager,sandbox-ingress-gateway", tls.RequireAndVerifyClientCert), newClient(managerCert)))
	})
	t.Run("DNS SAN match", func(t *testing.T) {
		require.NoError(t, tlsHandshake(t, newServer("sandbox-manager,sandbox-ingress-gateway", tls.RequireAndVerifyClientCert), newClient(sanCert)))
	})
	t.Run("name miss rejects handshake", func(t *testing.T) {
		err := tlsHandshake(t, newServer("sandbox-manager,sandbox-ingress-gateway", tls.RequireAndVerifyClientCert), newClient(otherCert))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not allowed")
	})
	t.Run("comma-only allowlist rejects every identity", func(t *testing.T) {
		err := tlsHandshake(t, newServer(",", tls.RequireAndVerifyClientCert), newClient(managerCert))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not allowed")
	})
	t.Run("gateway probe without a certificate", func(t *testing.T) {
		client := &tls.Config{
			RootCAs:    pool,
			ServerName: serverName,
			MinVersion: tls.VersionTLS12,
			MaxVersion: tls.VersionTLS12,
		}
		require.NoError(t, tlsHandshake(t, newServer("sandbox-manager", tls.VerifyClientCertIfGiven), client))
	})
	t.Run("resumed session still checks the current allowlist", func(t *testing.T) {
		var ticketKey [32]byte
		copy(ticketKey[:], []byte("peer-allowlist-session-ticket-key"))
		cache := tls.NewLRUClientSessionCache(4)

		allowed := newServer("sandbox-manager", tls.RequireAndVerifyClientCert)
		allowed.SetSessionTicketKeys([][32]byte{ticketKey})
		client := newClient(managerCert)
		client.ClientSessionCache = cache

		first, err := tlsHandshakeTCP(t, allowed, client)
		require.NoError(t, err)
		require.False(t, first.DidResume)

		second, err := tlsHandshakeTCP(t, allowed, client)
		require.NoError(t, err)
		require.True(t, second.DidResume)

		denied := newServer("sandbox-ingress-gateway", tls.RequireAndVerifyClientCert)
		denied.SetSessionTicketKeys([][32]byte{ticketKey})
		_, err = tlsHandshakeTCP(t, denied, client)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not allowed")
	})
}

func mustPeerTLSSecrets(t *testing.T) (*corev1.Secret, *corev1.Secret) {
	t.Helper()
	caCert, caKey, caPEM := mustCA(t)
	serverCert, serverKey := mustIssued(t, caCert, caKey, x509.ExtKeyUsageServerAuth, []string{serverName})
	clientCert, clientKey := mustIssued(t, caCert, caKey, x509.ExtKeyUsageClientAuth, nil)
	return secret("ns", "server", map[string][]byte{
			"ca.crt":  caPEM,
			"tls.crt": serverCert,
			"tls.key": serverKey,
		}), secret("ns", "client", map[string][]byte{
			"ca.crt":     caPEM,
			"client.crt": clientCert,
			"client.key": clientKey,
		})
}

func mustTLSCert(t *testing.T, certPEM, keyPEM []byte) tls.Certificate {
	t.Helper()
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	return cert
}

func tlsHandshake(t *testing.T, serverCfg, clientCfg *tls.Config) error {
	t.Helper()
	_, err := tlsHandshakeTCP(t, serverCfg, clientCfg)
	return err
}

func tlsHandshakeTCP(t *testing.T, serverCfg, clientCfg *tls.Config) (tls.ConnectionState, error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	serverErrc := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErrc <- err
			return
		}
		defer conn.Close()
		sc := tls.Server(conn, serverCfg)
		if err := sc.Handshake(); err != nil {
			serverErrc <- err
			return
		}
		buf := make([]byte, 1)
		_, _ = sc.Read(buf)
		serverErrc <- nil
	}()

	cc, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	if err != nil {
		serverErr := <-serverErrc
		if serverErr != nil {
			return tls.ConnectionState{}, serverErr
		}
		return tls.ConnectionState{}, err
	}
	state := cc.ConnectionState()
	_ = cc.Close()
	if serverErr := <-serverErrc; serverErr != nil {
		return state, serverErr
	}
	return state, nil
}
