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

package wake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
)

const (
	minPollBackoff = 50 * time.Millisecond
	maxPollBackoff = 500 * time.Millisecond
)

var (
	ErrAutoResumeDisabled = errors.New("auto-resume disabled")
	ErrInvalidPolicy      = errors.New("invalid auto-resume policy")
	ErrPausing            = errors.New("sandbox is pausing")
	ErrBadState           = errors.New("sandbox is not resumable")
	ErrGone               = errors.New("sandbox is gone")
	ErrNotFound           = errors.New("sandbox not found")
	ErrTransport          = errors.New("wake transport error")
)

// Module calls sandbox-manager's internal wake endpoint and waits for the local
// gateway registry to observe the sandbox as Running.
type Module struct {
	managerURL string
	httpClient *http.Client
	sf         singleflight.Group
	flightsMu  sync.Mutex
	flights    map[string]*flight
}

type flight struct {
	ctx     context.Context
	cancel  context.CancelFunc
	waiters int
	done    bool
}

func NewModule(managerURL string) *Module {
	return &Module{
		managerURL: strings.TrimRight(managerURL, "/"),
		httpClient: &http.Client{
			Transport: http.DefaultTransport,
		},
	}
}

func NewModuleWithClient(managerURL string, client *http.Client) *Module {
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport}
	}
	return &Module{
		managerURL: strings.TrimRight(managerURL, "/"),
		httpClient: client,
	}
}

func (m *Module) ManagerURL() string {
	if m == nil {
		return ""
	}
	return m.managerURL
}

func (m *Module) WakeAndWait(ctx context.Context, sandboxID string) error {
	if m == nil || m.managerURL == "" || m.httpClient == nil {
		return ErrTransport
	}

	flight := m.acquireFlight(sandboxID)
	result := m.sf.DoChan(sandboxID, func() (any, error) {
		defer m.finishFlight(sandboxID, flight)
		if err := m.callManager(flight.ctx, sandboxID); err != nil {
			return nil, err
		}
		return nil, waitUntilRunning(flight.ctx, sandboxID)
	})

	select {
	case r := <-result:
		m.releaseFlight(sandboxID, flight)
		return r.Err
	case <-ctx.Done():
		if m.releaseFlight(sandboxID, flight) {
			<-result
		}
		return ctx.Err()
	}
}

func (m *Module) acquireFlight(sandboxID string) *flight {
	m.flightsMu.Lock()
	defer m.flightsMu.Unlock()

	if m.flights == nil {
		m.flights = make(map[string]*flight)
	}
	f := m.flights[sandboxID]
	if f == nil || f.done {
		ctx, cancel := context.WithCancel(context.Background())
		f = &flight{ctx: ctx, cancel: cancel}
		m.flights[sandboxID] = f
	}
	f.waiters++
	return f
}

func (m *Module) releaseFlight(sandboxID string, f *flight) bool {
	m.flightsMu.Lock()
	defer m.flightsMu.Unlock()

	if current := m.flights[sandboxID]; current != f || f.done {
		return false
	}
	if f.waiters > 0 {
		f.waiters--
	}
	if f.waiters == 0 {
		f.cancel()
		f.done = true
		delete(m.flights, sandboxID)
		m.sf.Forget(sandboxID)
		return true
	}
	return false
}

func (m *Module) finishFlight(sandboxID string, f *flight) {
	m.flightsMu.Lock()
	defer m.flightsMu.Unlock()

	if current := m.flights[sandboxID]; current != f {
		return
	}
	f.done = true
	delete(m.flights, sandboxID)
	f.cancel()
}

func (m *Module) callManager(ctx context.Context, sandboxID string) error {
	endpoint := m.managerURL + "/wake/" + url.PathEscape(sandboxID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, http.NoBody)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTransport, err)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("%w: %v", ErrTransport, err)
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)

	result, decodeErr := decodeWakeResult(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnprocessableEntity:
		if decodeErr != nil {
			return fmt.Errorf("%w: decode wake result: %v", ErrTransport, decodeErr)
		}
		if result.Action == proxy.WakeActionInvalidAutoResumePolicy {
			return ErrInvalidPolicy
		}
		if result.Action == proxy.WakeActionAutoResumeDisabled {
			return ErrAutoResumeDisabled
		}
		return fmt.Errorf("%w: unexpected wake action %q", ErrTransport, result.Action)
	case http.StatusConflict:
		if decodeErr == nil && result.Action == proxy.WakeActionPausing {
			return ErrPausing
		}
		return ErrBadState
	case http.StatusGone:
		return ErrGone
	case http.StatusNotFound:
		return ErrNotFound
	default:
		if resp.StatusCode >= http.StatusInternalServerError {
			return fmt.Errorf("%w: manager returned %d", ErrTransport, resp.StatusCode)
		}
		return fmt.Errorf("%w: manager returned %d", ErrTransport, resp.StatusCode)
	}
}

func decodeWakeResult(body io.Reader) (proxy.WakeResult, error) {
	var result proxy.WakeResult
	if body == nil {
		return result, io.EOF
	}
	err := json.NewDecoder(body).Decode(&result)
	return result, err
}

func waitUntilRunning(ctx context.Context, sandboxID string) error {
	backoff := minPollBackoff
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		route, ok := registry.GetRegistry().Get(sandboxID)
		if ok && route.State == agentsv1alpha1.SandboxStateRunning {
			return nil
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}

		if backoff < maxPollBackoff {
			backoff *= 2
			if backoff > maxPollBackoff {
				backoff = maxPollBackoff
			}
		}
	}
}

func MapErrToReply(err error) (status int, body string, retryAfter int, code string) {
	switch {
	case err == nil:
		return 0, "", -1, ""
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return 0, "", -1, ""
	case errors.Is(err, ErrAutoResumeDisabled):
		return http.StatusBadGateway, "healthy sandbox not found", -1, "sandbox_not_running"
	case errors.Is(err, ErrInvalidPolicy):
		return http.StatusServiceUnavailable, "invalid auto-resume policy", 0, "sandbox_wake_invalid_policy"
	case errors.Is(err, ErrPausing):
		return http.StatusServiceUnavailable, "sandbox is pausing", 5, "sandbox_wake_pausing"
	case errors.Is(err, ErrBadState):
		return http.StatusServiceUnavailable, "sandbox is not resumable", 15, "sandbox_wake_bad_state"
	case errors.Is(err, ErrGone), errors.Is(err, ErrNotFound):
		return http.StatusBadGateway, "sandbox not found", -1, "sandbox_not_found"
	default:
		return http.StatusServiceUnavailable, "failed to wake sandbox", 5, "sandbox_wake_failed"
	}
}
