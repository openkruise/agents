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

package refresh

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
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/sandboxroute"
)

// staticPeers is a fixed peers.Peers view for tests.
type staticPeers []peers.Peer

func (p staticPeers) Start(context.Context, string, int) error { return nil }
func (p staticPeers) Stop(context.Context) error               { return nil }
func (p staticPeers) GetPeers() []peers.Peer                   { return p }
func (p staticPeers) GetAllMembers() []peers.Peer              { return p }
func (p staticPeers) LocalAddr() net.IP                        { return nil }
func (p staticPeers) LocalPort() int                           { return 0 }

func newTestPeerOutbound(client *http.Client) *PeerOutbound {
	return &PeerOutbound{scheme: "http", client: client}
}

type errRoundTripper struct {
	err  error
	resp *http.Response
}

func (t errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	if t.err != nil {
		return nil, t.err
	}
	return t.resp, nil
}

func TestRequestPeerAddressAndTransport(t *testing.T) {
	t.Run("invalid IP", func(t *testing.T) {
		err := NewPeerOutbound(nil).request(t.Context(), http.MethodPost, "not-an-ip", Path, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid peer IP")
	})

	t.Run("plaintext disables proxy and redirects", func(t *testing.T) {
		out := NewPeerOutbound(nil)
		assert.Equal(t, "http", out.scheme)
		transport, ok := out.client.Transport.(*http.Transport)
		require.True(t, ok)
		assert.Nil(t, transport.Proxy)
		assert.ErrorIs(t, out.client.CheckRedirect(&http.Request{}, []*http.Request{{}}), errPeerRedirect)
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
		out = NewPeerOutbound(tlsConfig)
		assert.Equal(t, "https", out.scheme)
		transport, ok = out.client.Transport.(*http.Transport)
		require.True(t, ok)
		assert.Same(t, tlsConfig, transport.TLSClientConfig)
	})
}

func TestSyncRouteWithPeers(t *testing.T) {
	route := sandboxroute.Route{ID: "sb-1", Namespace: "ns", Name: "sb-1", IP: "1.2.3.4", UID: "uid-sb-1", ResourceVersion: "1"}
	threePeers := staticPeers{
		{IP: "10.0.0.1", Name: "node-1"},
		{IP: "10.0.0.2", Name: "node-2"},
		{IP: "10.0.0.3", Name: "node-3"},
	}
	tests := []struct {
		name            string
		peers           peers.Peers
		outbound        *PeerOutbound
		expectErr       string
		expectPeerCount float64
	}{
		{name: "nil peers manager is a no-op"},
		{name: "no peers is a no-op without an outbound client", peers: staticPeers{}},
		{name: "sender-only members need no outbound client", peers: staticPeers{{Name: "sc-controller", IP: "127.0.0.1"}}},
		{
			name:            "mixed membership counts only refresh targets",
			peers:           staticPeers{{Name: "sc-controller", IP: "127.0.0.1"}, {Name: "sm-manager", IP: "127.0.0.2"}},
			expectErr:       "peer outbound client is not configured",
			expectPeerCount: 1,
		},
		{
			name:            "peers without an outbound client fail",
			peers:           threePeers,
			expectErr:       "peer outbound client is not configured",
			expectPeerCount: 3,
		},
		{
			name:  "rejected peers are reported and still counted",
			peers: threePeers,
			outbound: newTestPeerOutbound(&http.Client{Transport: errRoundTripper{resp: &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader("")),
			}}}),
			expectErr:       "peer rejected request",
			expectPeerCount: 3,
		},
	}

	// The peerCount gauge is process-global: rows run serially and reset it.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Cleanup(tt.outbound.CloseIdleConnections)
			var before []peers.Peer
			if tt.peers != nil {
				before = append([]peers.Peer(nil), tt.peers.GetPeers()...)
			}
			peerCount.Set(-1)
			err := SyncRouteWithPeers(t.Context(), tt.peers, route, tt.outbound)
			if tt.expectErr == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, tt.expectErr)
			}
			assert.Equal(t, tt.expectPeerCount, testutil.ToFloat64(peerCount))
			if tt.peers != nil {
				assert.Equal(t, before, append([]peers.Peer(nil), tt.peers.GetPeers()...), "membership must not be modified")
			}
		})
	}
}

// Only TCP dialing is redirected; the public constructor's HTTP policy and real request behavior are kept.
func outboundForServers(t *testing.T, targets map[string]string) *PeerOutbound {
	t.Helper()
	out := NewPeerOutbound(nil)
	transport := out.client.Transport.(*http.Transport)
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		target, ok := targets[address]
		if !ok {
			return nil, errors.New("no test target for " + address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, target)
	}
	t.Cleanup(out.CloseIdleConnections)
	return out
}

func TestSyncRouteDelivery(t *testing.T) {
	tests := []struct {
		name         string
		ip           string
		secondPeer   bool
		failSecond   bool
		status       int
		failures     int32
		canceled     bool
		cancelDuring bool
		wantErr      string
		wantRequests int32
	}{
		{name: "two targets receive full route", ip: "127.0.0.1", secondPeer: true, wantRequests: 1},
		{name: "IPv6 uses bracketed address", ip: "fd11:1111:1111::10", wantRequests: 1},
		{name: "one failed target preserves other delivery", ip: "127.0.0.1", secondPeer: true, failSecond: true, wantErr: "no test target", wantRequests: 1},
		{name: "4xx is not retried", ip: "127.0.0.1", status: http.StatusBadRequest, failures: 100, wantErr: "status code: 400", wantRequests: 1},
		{name: "5xx retries until success", ip: "127.0.0.1", status: http.StatusInternalServerError, failures: 2, wantRequests: 3},
		{name: "exhausted 5xx reports last error", ip: "127.0.0.1", status: http.StatusInternalServerError, failures: 100, wantErr: "status code: 500", wantRequests: int32(peerSyncRetrySteps)},
		{name: "canceled context sends nothing", ip: "127.0.0.1", canceled: true, wantErr: "context canceled"},
		{name: "cancellation during retry stops further attempts", ip: "127.0.0.1", status: http.StatusInternalServerError, failures: 100, cancelDuring: true, wantErr: "context canceled", wantRequests: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route := *fullRoute("running", "1")
			route.IP = "10.0.0.5"
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, Path, r.URL.Path)
				var actual sandboxroute.Route
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&actual))
				assert.Equal(t, route, actual)
				if requests.Add(1) <= tt.failures {
					if tt.cancelDuring {
						cancel()
					}
					w.WriteHeader(tt.status)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			targets := map[string]string{net.JoinHostPort(tt.ip, strconv.Itoa(DefaultPort)): server.Listener.Addr().String()}
			members := staticPeers{{Name: "sm-first", IP: tt.ip}, {Name: "sc-sender-only", IP: "invalid-controller-address"}}
			var secondRequests atomic.Int32
			if tt.secondPeer {
				members = append(members, peers.Peer{Name: "sg-second", IP: "127.0.0.2"})
				if !tt.failSecond {
					second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var actual sandboxroute.Route
						assert.NoError(t, json.NewDecoder(r.Body).Decode(&actual))
						assert.Equal(t, route, actual)
						secondRequests.Add(1)
						w.WriteHeader(http.StatusNoContent)
					}))
					defer second.Close()
					targets["127.0.0.2:7789"] = second.Listener.Addr().String()
				}
			}
			out := outboundForServers(t, targets)
			if tt.canceled {
				cancel()
			}
			err := SyncRouteWithPeers(ctx, members, route, out)
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
			}
			if tt.canceled || tt.cancelDuring {
				assert.ErrorIs(t, err, context.Canceled)
			}
			assert.Equal(t, tt.wantRequests, requests.Load())
			if tt.secondPeer && !tt.failSecond {
				assert.Equal(t, int32(1), secondRequests.Load())
			}
		})
	}
}

func TestSyncRouteWithRealMemberlist(t *testing.T) {
	reader := fake.NewClientBuilder().Build()
	sender, port, err := peers.CreateTestPeer(t.Context(), reader, "sc-sender")
	require.NoError(t, err)
	require.NoError(t, sender.Start(t.Context(), "127.0.0.1", port))
	t.Cleanup(func() { require.NoError(t, sender.Stop(context.Background())) })
	cfg := memberlist.DefaultLANConfig()
	cfg.Name, cfg.BindAddr, cfg.BindPort, cfg.LogOutput = "sm-receiver", "127.0.0.1", 0, io.Discard
	receiver, err := memberlist.Create(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, receiver.Shutdown()) })
	_, err = receiver.Join([]string{net.JoinHostPort(sender.LocalAddr().String(), strconv.Itoa(sender.LocalPort()))})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(sender.GetPeers()) == 1 }, time.Second, time.Millisecond)
	store := sandboxroute.NewStore()
	httpReceiver := httptest.NewServer(NewHandler(store, nil))
	defer httpReceiver.Close()
	out := outboundForServers(t, map[string]string{"127.0.0.1:7789": httpReceiver.Listener.Addr().String()})
	route := *fullRoute("running", "1")
	route.IP = "192.168.1.100"
	require.NoError(t, SyncRouteWithPeers(t.Context(), sender, route, out))
	got, ok := store.Get(route.ID)
	require.True(t, ok)
	assert.Equal(t, route, got)
}

func TestPeerOutboundMTLS(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	identity := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "controller-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, identity, identity, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	clientCert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	require.NoError(t, err)
	identity, err = x509.ParseCertificate(der)
	require.NoError(t, err)
	clientTrust := x509.NewCertPool()
	clientTrust.AddCert(identity)
	route := sandboxroute.Route{ID: "sb-1", Namespace: "ns", Name: "sb-1", IP: "192.0.2.1", ResourceVersion: "2"}
	var received atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, Path, r.URL.Path)
		assert.NotEmpty(t, r.TLS.VerifiedChains)
		var actual sandboxroute.Route
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&actual))
		assert.Equal(t, route, actual)
		received.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientTrust, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	serverTrust := x509.NewCertPool()
	serverTrust.AddCert(server.Certificate())
	validTLS := &tls.Config{RootCAs: serverTrust, Certificates: []tls.Certificate{clientCert}, ServerName: server.Certificate().DNSNames[0], MinVersion: tls.VersionTLS12}
	tests := []struct {
		name    string
		modify  func(*tls.Config)
		wantErr bool
	}{
		{name: "authenticated HTTPS refresh"},
		{name: "missing client identity", modify: func(c *tls.Config) { c.Certificates = nil }, wantErr: true},
		{name: "wrong server name", modify: func(c *tls.Config) { c.ServerName = "wrong.example" }, wantErr: true},
		{name: "untrusted server", modify: func(c *tls.Config) { c.RootCAs = x509.NewCertPool() }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := validTLS.Clone()
			if tt.modify != nil {
				tt.modify(config)
			}
			out := NewPeerOutbound(config)
			defer out.CloseIdleConnections()
			transport := out.client.Transport.(*http.Transport)
			// Redirect only TCP dialing for the fixed port, preserving real TLS verification and shared fanout.
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				assert.Equal(t, "127.0.0.1:7789", address)
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}
			before := received.Load()
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			err := SyncRouteWithPeers(ctx, staticPeers{{Name: "sm-test", IP: "127.0.0.1"}}, route, out)
			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, before, received.Load())
			} else {
				require.NoError(t, err)
				assert.Equal(t, before+1, received.Load())
			}
		})
	}
}

func TestOutboundTLSErrors(t *testing.T) {
	unknownAuth := x509.UnknownAuthorityError{}
	tests := []struct {
		name        string
		scheme      string
		trip        errRoundTripper
		expectDelta float64
	}{
		{
			name:        "https unknown authority",
			scheme:      "https",
			trip:        errRoundTripper{err: &url.Error{Op: "Post", Err: unknownAuth}},
			expectDelta: 1,
		},
		{
			name:        "https hostname mismatch",
			scheme:      "https",
			trip:        errRoundTripper{err: x509.HostnameError{Host: "wrong.example"}},
			expectDelta: 1,
		},
		{
			name:        "https record header",
			scheme:      "https",
			trip:        errRoundTripper{err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}},
			expectDelta: 1,
		},
		{
			name:        "https dial refused is not TLS",
			scheme:      "https",
			trip:        errRoundTripper{err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}},
			expectDelta: 0,
		},
		{
			name:   "https HTTP 403 is not TLS",
			scheme: "https",
			trip: errRoundTripper{resp: &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader("")),
			}},
			expectDelta: 0,
		},
		{
			name:        "plaintext does not count TLS errors",
			scheme:      "http",
			trip:        errRoundTripper{err: unknownAuth},
			expectDelta: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := &PeerOutbound{
				scheme: tt.scheme,
				client: &http.Client{Transport: tt.trip},
			}
			before := testutil.ToFloat64(outboundTLSErrors)
			_ = out.request(t.Context(), http.MethodPost, "127.0.0.1", Path, nil)
			assert.Equal(t, tt.expectDelta, testutil.ToFloat64(outboundTLSErrors)-before)
		})
	}
}
