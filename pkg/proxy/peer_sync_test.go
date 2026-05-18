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
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsRetryablePeerError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context canceled", context.Canceled, false},
		{"context deadline", context.DeadlineExceeded, false},
		{"wrapped context deadline", fmt.Errorf("Post: %w", context.DeadlineExceeded), false},
		{"http 400", &peerStatusError{ip: "1.2.3.4", code: http.StatusBadRequest}, false},
		{"http 404", &peerStatusError{ip: "1.2.3.4", code: http.StatusNotFound}, false},
		{"http 409", &peerStatusError{ip: "1.2.3.4", code: http.StatusConflict}, false},
		{"http 429", &peerStatusError{ip: "1.2.3.4", code: http.StatusTooManyRequests}, true},
		{"http 500", &peerStatusError{ip: "1.2.3.4", code: http.StatusInternalServerError}, true},
		{"http 503", &peerStatusError{ip: "1.2.3.4", code: http.StatusServiceUnavailable}, true},
		{"transport error", fmt.Errorf("dial tcp 1.2.3.4:7789: connect: connection refused"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isRetryablePeerError(tc.err))
		})
	}
}

// The whole point of the fix: the inter-attempt sleep budget must stay strictly
// below a single per-request timeout, otherwise retries dominate the latency.
func TestPeerSyncBackoffBudgetUnderRequestTimeout(t *testing.T) {
	b := peerSyncBackoff
	var total time.Duration
	for i := 0; i < peerSyncBackoff.Steps-1; i++ {
		total += b.Step()
	}
	assert.Less(t, total, consts.RequestPeerTimeout,
		"total retry sleep budget %s must be < per-request timeout %s", total, consts.RequestPeerTimeout)
}

// countingRoundTripper records the peak number of concurrently in-flight
// requests so the test can assert the fan-out honours the concurrency cap.
type countingRoundTripper struct {
	inFlight int64
	max      int64
}

func (c *countingRoundTripper) RoundTrip(_ *http.Request) (*http.Response, error) {
	n := atomic.AddInt64(&c.inFlight, 1)
	for {
		old := atomic.LoadInt64(&c.max)
		if n <= old || atomic.CompareAndSwapInt64(&c.max, old, n) {
			break
		}
	}
	time.Sleep(15 * time.Millisecond)
	atomic.AddInt64(&c.inFlight, -1)
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Body:       io.NopCloser(bytes.NewReader(nil)),
		Header:     make(http.Header),
	}, nil
}

func TestSyncRouteWithPeers_ConcurrencyBounded(t *testing.T) {
	rt := &countingRoundTripper{}
	origClient := requestPeerClient
	requestPeerClient = &http.Client{Timeout: 5 * time.Second, Transport: rt}
	defer func() { requestPeerClient = origClient }()

	members := make([]peers.Peer, 0, 200)
	for i := 0; i < 200; i++ {
		members = append(members, peers.Peer{IP: fmt.Sprintf("10.1.%d.%d", i/256, i%256), Name: fmt.Sprintf("n-%d", i)})
	}
	s := newTestServer(newMockPeers(members...))

	err := s.SyncRouteWithPeers(Route{ID: "sb-conc", IP: "1.2.3.4", ResourceVersion: "1"})
	require.NoError(t, err)

	peak := atomic.LoadInt64(&rt.max)
	assert.LessOrEqual(t, peak, int64(peerSyncMaxConcurrency),
		"peak in-flight %d must not exceed cap %d", peak, peerSyncMaxConcurrency)
	assert.Greater(t, peak, int64(1), "fan-out should run peers concurrently, got peak %d", peak)
}
