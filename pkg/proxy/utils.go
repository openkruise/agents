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

package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
)

// requestPeerTransport is shared across all peer route-sync requests so
// connections are pooled and reused. The default transport caps idle
// connections per host at 2, so a burst of route changes to many peers keeps
// opening fresh connections instead of reusing them.
var requestPeerTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 256
	t.MaxIdleConnsPerHost = 32
	t.MaxConnsPerHost = 64
	t.IdleConnTimeout = 90 * time.Second
	return t
}()

// requestPeerClient is package-level (and reassigned in tests) so the tuned
// transport above is shared across every peer request.
var requestPeerClient = &http.Client{
	Timeout:   consts.RequestPeerTimeout,
	Transport: requestPeerTransport,
}

// peerStatusError is returned when a peer answers with a non-2xx status. It
// carries the status code so retry classification can tell a transient 5xx
// apart from a permanent 4xx.
type peerStatusError struct {
	ip   string
	code int
}

func (e *peerStatusError) Error() string {
	return fmt.Sprintf("request to peer %s failed with status code: %d", e.ip, e.code)
}

// isRetryablePeerError reports whether a failed peer request is worth retrying.
// A context error means the per-peer budget is already spent; an HTTP 4xx
// (other than 429) is permanent and must not be retried; transport/network
// failures are transient.
func isRetryablePeerError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var statusErr *peerStatusError
	if errors.As(err, &statusErr) {
		return statusErr.code == http.StatusTooManyRequests || statusErr.code >= 500
	}
	return true
}

func requestPeer(ctx context.Context, method, ip, path string, body []byte) error {
	var buf io.Reader
	if len(body) > 0 {
		buf = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://%s:%d%s", ip, SystemPort, path), buf)
	if err != nil {
		return err
	}

	resp, err := requestPeerClient.Do(request)
	if err != nil {
		return err
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &peerStatusError{ip: ip, code: resp.StatusCode}
	}

	return nil
}
