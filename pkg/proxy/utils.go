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

	"github.com/openkruise/agents/pkg/sandboxroute/refresh"
	"k8s.io/apimachinery/pkg/util/wait"
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

var requestPeerClient = &http.Client{
	Timeout: RequestPeerTimeout,
}

func requestPeer(ctx context.Context, method, ip, path string, body []byte) error {
	var buf io.Reader
	if len(body) > 0 {
		buf = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://%s:%d%s", ip, refresh.DefaultPort, path), buf)
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

	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return fmt.Errorf("%w: request to peer %s failed with status code: %d", errPeerRejected, ip, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("request to peer %s failed with status code: %d", ip, resp.StatusCode)
	}

	return nil
}

func requestPeerWithRetry(ctx context.Context, method, ip, path string, body []byte) error {
	var lastErr error
	err := wait.ExponentialBackoffWithContext(ctx, wait.Backoff{
		Steps:    peerSyncRetrySteps,
		Duration: peerSyncRetryInterval,
		Factor:   1,
	}, func(ctx context.Context) (bool, error) {
		lastErr = requestPeer(ctx, method, ip, path, body)
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
