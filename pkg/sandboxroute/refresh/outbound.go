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
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/peersecurity"
	"github.com/openkruise/agents/pkg/sandboxroute"
)

const (
	RequestPeerTimeout = 100 * time.Millisecond
	// peerSyncRetrySteps and peerSyncRetryInterval match the historical
	// SyncRouteWithPeers backoff budget (10 attempts, 10ms apart).
	peerSyncRetrySteps    = 10
	peerSyncRetryInterval = 10 * time.Millisecond
)

// errPeerRejected marks a deterministic 4xx peer response that must not be retried.
var errPeerRejected = errors.New("peer rejected request")

// errPeerRedirect is returned instead of following an HTTP redirect.
var errPeerRedirect = errors.New("peer refresh does not follow redirects")

// PeerOutbound is one process's outbound peer HTTP client. The peer owner holds
// this value and every producer uses it; callers must not construct a second
// client with an independent security decision.
type PeerOutbound struct {
	scheme string
	client *http.Client
}

// InitializePeers loads startup security once, installs the gossip key on the
// existing peer manager, and constructs the process's outbound refresh client.
// Call before starting listeners or membership. The returned server TLS snapshot
// has no ClientAuth policy; each receiver must set its own. Empty inputs read no
// Secrets and permit non-memberlist peer implementations. Lifecycle and discovery
// scope remain with the caller, using the shared peers constructor and Start/Stop.
func InitializePeers(ctx context.Context, reader ctrlclient.Reader, peerManager peers.Peers, inputs peersecurity.Inputs) (*PeerOutbound, *tls.Config, error) {
	loadCtx, cancel := context.WithTimeout(ctx, peersecurity.LoadTimeout)
	defer cancel()
	secretKey, serverTLS, clientTLS, err := peersecurity.Load(loadCtx, reader, inputs)
	if err != nil {
		return nil, nil, fmt.Errorf("load peer security: %w", err)
	}
	if err := loadCtx.Err(); err != nil {
		return nil, nil, fmt.Errorf("load peer security: %w", err)
	}
	if len(secretKey) > 0 {
		memberlist, ok := peerManager.(*peers.MemberlistPeers)
		if !ok || memberlist == nil {
			return nil, nil, fmt.Errorf("memberlist secret key is configured but the peer manager does not accept it")
		}
		memberlist.SetSecretKey(secretKey)
	}
	return NewPeerOutbound(clientTLS), serverTLS, nil
}

// NewPeerOutbound builds the outbound peer client. A nil clientTLS keeps
// plaintext HTTP. Transport tuning mirrors http.DefaultTransport, except that
// environment proxies are unused and redirects are refused at the client level.
func NewPeerOutbound(clientTLS *tls.Config) *PeerOutbound {
	scheme := "http"
	if clientTLS != nil {
		scheme = "https"
	}
	return &PeerOutbound{
		scheme: scheme,
		client: newPeerHTTPClient(clientTLS),
	}
}

func newPeerHTTPClient(clientTLS *tls.Config) *http.Client {
	transport := &http.Transport{
		// Peer refreshes dial fixed IPs directly, never a proxy.
		Proxy:           nil,
		TLSClientConfig: clientTLS,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// A non-nil TLSClientConfig disables HTTP/2 auto-upgrade; keep it enabled.
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		// RequestPeerTimeout is the whole-request budget, TLS handshake included.
		Timeout:   RequestPeerTimeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errPeerRedirect
		},
	}
}

// CloseIdleConnections drops idle outbound peer connections.
func (o *PeerOutbound) CloseIdleConnections() {
	if o == nil || o.client == nil {
		return
	}
	o.client.CloseIdleConnections()
}

// SyncRouteWithPeers sends a route update to all peer gateways via HTTP POST /refresh.
// outbound is this process's peer-owner client. Callers must not construct a
// second client with an independent security decision.
func SyncRouteWithPeers(ctx context.Context, peersManager peers.Peers, route sandboxroute.Route, outbound *PeerOutbound) error {
	var peerList []peers.Peer
	if peersManager != nil {
		peerList = peersManager.GetPeers()
	}
	return SyncRouteWithPeerList(ctx, peerList, route, outbound)
}

// SyncRouteWithPeerList sends to the caller's snapshot, keeping target counts consistent.
// An empty snapshot remains a no-op; the caller owns skip logging.
func SyncRouteWithPeerList(ctx context.Context, peerList []peers.Peer, route sandboxroute.Route, outbound *PeerOutbound) error {
	// controller 仅发送路由，不提供 /refresh；过滤使用独立快照，不修改调用方列表。
	targets := make([]peers.Peer, 0, len(peerList))
	for _, peer := range peerList {
		if !strings.HasPrefix(peer.Name, peers.NodePrefixSandboxController) {
			targets = append(targets, peer)
		}
	}
	peerList = targets
	body, err := json.Marshal(route) // #nosec G117 -- AccessToken is serialized for peer /refresh authentication sync, never logged; the peer owner configures transport security.
	if err != nil {
		return err
	}
	peerCount.Set(float64(len(peerList)))

	if len(peerList) == 0 {
		return nil
	}
	if outbound == nil {
		return fmt.Errorf("peer outbound client is not configured")
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		peerErrs []error
	)

	for _, peer := range peerList {
		wg.Add(1)
		go func(peerIP string) {
			defer wg.Done()
			if requestErr := outbound.requestWithRetry(ctx, http.MethodPost, peerIP, Path, body); requestErr != nil {
				mu.Lock()
				peerErrs = append(peerErrs, requestErr)
				mu.Unlock()
			}
		}(peer.IP)
	}
	wg.Wait()

	return errors.Join(peerErrs...)
}

func (o *PeerOutbound) request(ctx context.Context, method, ip, path string, body []byte) error {
	if o == nil || o.client == nil {
		return fmt.Errorf("peer outbound client is not configured")
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return fmt.Errorf("invalid peer IP %q", ip)
	}
	var buf io.Reader
	if len(body) > 0 {
		buf = bytes.NewReader(body)
	}
	url := o.scheme + "://" + net.JoinHostPort(parsed.String(), strconv.Itoa(DefaultPort)) + path
	request, err := http.NewRequestWithContext(ctx, method, url, buf)
	if err != nil {
		return err
	}

	resp, err := o.client.Do(request)
	if err != nil {
		if o.scheme == "https" && isOutboundTLSError(err) {
			outboundTLSErrors.Inc()
		}
		return err
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)

	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return fmt.Errorf("%w: request to peer %s failed with status code: %d", errPeerRejected, ip, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("request to peer %s failed with status code: %d", ip, resp.StatusCode)
	}

	return nil
}

func (o *PeerOutbound) requestWithRetry(ctx context.Context, method, ip, path string, body []byte) error {
	var lastErr error
	err := wait.ExponentialBackoffWithContext(ctx, wait.Backoff{
		Steps:    peerSyncRetrySteps,
		Duration: peerSyncRetryInterval,
		Factor:   1,
	}, func(ctx context.Context) (bool, error) {
		lastErr = o.request(ctx, method, ip, path, body)
		if errors.Is(lastErr, errPeerRejected) {
			return false, lastErr
		}
		return lastErr == nil, nil
	})
	// On exhausted retries surface the last peer error; context cancellation
	// and deadline errors pass through unchanged.
	if wait.Interrupted(err) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return lastErr
	}
	return err
}

func isOutboundTLSError(err error) bool {
	if err == nil {
		return false
	}
	var (
		unknownAuth x509.UnknownAuthorityError
		hostname    x509.HostnameError
		invalid     x509.CertificateInvalidError
		verify      *tls.CertificateVerificationError
		record      tls.RecordHeaderError
		alert       tls.AlertError
	)
	return errors.As(err, &unknownAuth) ||
		errors.As(err, &hostname) ||
		errors.As(err, &invalid) ||
		errors.As(err, &verify) ||
		errors.As(err, &record) ||
		errors.As(err, &alert)
}
