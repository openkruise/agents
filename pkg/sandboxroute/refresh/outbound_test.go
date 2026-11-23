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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

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
			peerCount.Set(-1)
			err := SyncRouteWithPeers(t.Context(), tt.peers, route, tt.outbound)
			if tt.expectErr == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, tt.expectErr)
			}
			assert.Equal(t, tt.expectPeerCount, testutil.ToFloat64(peerCount))
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
