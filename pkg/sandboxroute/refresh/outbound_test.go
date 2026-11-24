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
	"bytes"
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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/peersecurity"
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

func TestInitializePeers(t *testing.T) {
	keyRef := types.NamespacedName{Namespace: "ns", Name: "peer-key"}
	keyInputs := peersecurity.Inputs{PeerKeySecret: keyRef}
	keySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: keyRef.Namespace, Name: keyRef.Name},
		Data:       map[string][]byte{"key": bytes.Repeat([]byte{'k'}, 32)},
	}
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, keyRef.Name, errors.New("denied"))
	tests := []struct {
		name       string
		inputs     peersecurity.Inputs
		memberlist bool
		nilReader  bool
		canceled   bool
		readErr    error
		wantReads  int
		wantErr    string
		wantIs     error
	}{
		{name: "empty inputs read nothing"},
		{name: "plaintext needs no Secret reader", nilReader: true},
		{name: "installs a key on existing memberlist", inputs: keyInputs, memberlist: true, wantReads: 1},
		{name: "key requires memberlist", inputs: keyInputs, wantReads: 1, wantErr: "does not accept it"},
		{name: "configured inputs require a reader", inputs: keyInputs, nilReader: true, wantErr: "peer secret reader is not configured"},
		{name: "missing Secret fails without retry", inputs: peersecurity.Inputs{PeerKeySecret: types.NamespacedName{Namespace: "ns", Name: "missing"}}, wantReads: 1, wantErr: "not found"},
		{name: "Forbidden fails without retry", inputs: keyInputs, readErr: forbidden, wantReads: 1, wantErr: "forbidden", wantIs: forbidden},
		{name: "incomplete TLS pair fails before reads", inputs: peersecurity.Inputs{TLSServerSecret: types.NamespacedName{Namespace: "ns", Name: "server"}}, wantErr: "both server and client"},
		{name: "canceled plaintext publishes no client", canceled: true, wantErr: "context canceled", wantIs: context.Canceled},
		{name: "canceled security does not read", inputs: keyInputs, canceled: true, wantErr: "context canceled", wantIs: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reads := 0
			reader := interceptor.NewClient(fake.NewClientBuilder().WithObjects(keySecret).Build(), interceptor.Funcs{
				Get: func(ctx context.Context, c ctrlclient.WithWatch, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
					reads++
					deadline, ok := ctx.Deadline()
					assert.True(t, ok, "shared initialization must bound Secret reads")
					assert.LessOrEqual(t, time.Until(deadline), peersecurity.LoadTimeout)
					if tt.readErr != nil {
						return tt.readErr
					}
					return c.Get(ctx, key, obj, opts...)
				},
				List: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectList, ...ctrlclient.ListOption) error {
					t.Error("initialization must not start discovery")
					return errors.New("unexpected discovery")
				},
			})
			var peerManager peers.Peers = staticPeers{}
			if tt.memberlist {
				peerManager = peers.NewMemberlistPeers(reader, "sm-test", "ns", "app=peer")
			}
			var secretReader ctrlclient.Reader = reader
			if tt.nilReader {
				secretReader = nil
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.canceled {
				cancel()
			}
			outbound, serverTLS, err := InitializePeers(ctx, secretReader, peerManager, tt.inputs)
			assert.Equal(t, tt.wantReads, reads)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				if tt.wantIs != nil {
					assert.ErrorIs(t, err, tt.wantIs)
				}
				assert.Nil(t, outbound)
				assert.Nil(t, serverTLS)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, outbound)
			defer outbound.CloseIdleConnections()
			assert.Equal(t, "http", outbound.Scheme(), "gossip encryption does not imply refresh TLS")
			assert.Nil(t, serverTLS)
			assert.Nil(t, peerManager.LocalAddr(), "membership stays unstarted")
		})
	}
}

func TestInitializePeersGossipKey(t *testing.T) {
	key := bytes.Repeat([]byte{'k'}, 32)
	tests := []struct {
		name        string
		receiverKey []byte
		wantErr     bool
	}{
		{name: "matching encrypted peer joins", receiverKey: key},
		{name: "different key is rejected", receiverKey: bytes.Repeat([]byte{'x'}, 32), wantErr: true},
		{name: "plaintext peer is rejected", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "peer-key"},
				Data:       map[string][]byte{"key": key},
			}
			reader := fake.NewClientBuilder().WithObjects(secret).Build()
			peerManager, port, err := peers.CreateTestPeer(t.Context(), reader, "sc-test")
			require.NoError(t, err)
			outbound, _, err := InitializePeers(t.Context(), reader, peerManager, peersecurity.Inputs{
				PeerKeySecret: types.NamespacedName{Namespace: "ns", Name: "peer-key"},
			})
			require.NoError(t, err)
			t.Cleanup(outbound.CloseIdleConnections)
			require.NoError(t, peerManager.Start(t.Context(), "127.0.0.1", port))
			t.Cleanup(func() { require.NoError(t, peerManager.Stop(context.Background())) })

			// Configure an independent real peer so a missing key installation cannot
			// pass merely because both sides accidentally use plaintext gossip.
			cfg := memberlist.DefaultLANConfig()
			cfg.Name = "sm-receiver"
			cfg.BindAddr = "127.0.0.1"
			cfg.BindPort = 0
			cfg.SecretKey = tt.receiverKey
			cfg.LogOutput = io.Discard
			receiver, err := memberlist.Create(cfg)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, receiver.Shutdown()) })
			_, err = receiver.Join([]string{net.JoinHostPort(peerManager.LocalAddr().String(), strconv.Itoa(peerManager.LocalPort()))})
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Len(t, receiver.Members(), 2)
			}
		})
	}
}

func TestRequestPeerAddressAndTransport(t *testing.T) {
	t.Run("invalid IP", func(t *testing.T) {
		err := NewPeerOutbound(nil).request(t.Context(), http.MethodPost, "not-an-ip", Path, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid peer IP")
	})

	t.Run("plaintext disables proxy and redirects", func(t *testing.T) {
		out := NewPeerOutbound(nil)
		assert.Equal(t, "http", out.Scheme())
		transport, ok := out.client.Transport.(*http.Transport)
		require.True(t, ok)
		assert.Nil(t, transport.Proxy)
		assert.ErrorIs(t, out.client.CheckRedirect(&http.Request{}, []*http.Request{{}}), errPeerRedirect)
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
		out = NewPeerOutbound(tlsConfig)
		assert.Equal(t, "https", out.Scheme())
		transport, ok = out.client.Transport.(*http.Transport)
		require.True(t, ok)
		assert.Same(t, tlsConfig, transport.TLSClientConfig)
	})
}

func TestSyncRouteWithPeers(t *testing.T) {
	route := sandboxroute.Route{
		ID:              "sb-1",
		IP:              "1.2.3.4",
		Namespace:       "ns",
		Name:            "sb-1",
		UID:             types.UID("uid-sb-1"),
		ResourceVersion: "1",
	}
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
		{
			name:            "nil peers manager is a no-op",
			expectPeerCount: 0,
		},
		{
			name:            "no peers is a no-op without an outbound client",
			peers:           staticPeers{},
			expectPeerCount: 0,
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
			outbound: NewTestPeerOutbound(&http.Client{Transport: errRoundTripper{resp: &http.Response{
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
			for _, snapshot := range []bool{false, true} {
				peerCount.Set(-1)
				var err error
				if snapshot {
					var peerList []peers.Peer
					if tt.peers != nil {
						peerList = tt.peers.GetPeers()
					}
					err = SyncRouteWithPeerList(t.Context(), peerList, route, tt.outbound)
				} else {
					err = SyncRouteWithPeers(t.Context(), tt.peers, route, tt.outbound)
				}
				if tt.expectErr == "" {
					assert.NoError(t, err, "snapshot=%t", snapshot)
				} else {
					assert.ErrorContains(t, err, tt.expectErr, "snapshot=%t", snapshot)
				}
				assert.Equal(t, tt.expectPeerCount, testutil.ToFloat64(peerCount), "snapshot=%t", snapshot)
			}
		})
	}
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
			err := SyncRouteWithPeerList(ctx, []peers.Peer{{Name: "sm-test", IP: "127.0.0.1"}}, route, out)
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
