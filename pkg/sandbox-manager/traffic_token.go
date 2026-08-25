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

package sandbox_manager

import (
	"context"
	"errors"
	"runtime/debug"
	"sync"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"k8s.io/klog/v2"
)

// defaultTrafficTokenIssueTimeout bounds detached provider calls and leaves
// headroom for the process-level shutdown timeout to drain other components.
const defaultTrafficTokenIssueTimeout = 30 * time.Second

var errTrafficTokenIssuerPanic = errors.New("traffic access token issuer panicked")

// RefreshTrafficAccessTokenOptions identifies the caller and Sandbox for an
// explicit traffic-token refresh.
type RefreshTrafficAccessTokenOptions struct {
	Namespace string
	SandboxID string
	User      string
}

// RefreshTrafficAccessToken issues a new token without mutating the Sandbox.
func (m *SandboxManager) RefreshTrafficAccessToken(ctx context.Context, opts RefreshTrafficAccessTokenOptions) (infra.TrafficAccessToken, error) {
	return m.issueTrafficAccessToken(ctx, opts)
}

func (m *SandboxManager) issueTrafficAccessToken(ctx context.Context, opts RefreshTrafficAccessTokenOptions) (infra.TrafficAccessToken, error) {
	if opts.User == "" {
		return infra.TrafficAccessToken{}, managererrors.NewError(managererrors.ErrorBadRequest, "user is required")
	}
	if opts.SandboxID == "" {
		return infra.TrafficAccessToken{}, managererrors.NewError(managererrors.ErrorBadRequest, "sandbox ID is required")
	}

	sandbox, err := m.GetSandbox(ctx, opts.User, nil, infra.GetSandboxOptions{
		Namespace: opts.Namespace,
		SandboxID: opts.SandboxID,
	})
	if err != nil {
		return infra.TrafficAccessToken{}, err
	}
	if err := validateTrafficTokenSandbox(sandbox, opts.User); err != nil {
		return infra.TrafficAccessToken{}, err
	}
	if m.trafficTokenSingleflight == nil {
		return infra.TrafficAccessToken{}, managererrors.NewError(managererrors.ErrorInternal, "traffic access token singleflight is not configured")
	}

	flightKey := string(sandbox.GetUID())
	if flightKey != "" {
		// A reusable Sandbox CR can serve multiple deliveries. Include the
		// delivery ID so a recycled CR cannot join the previous delivery's flight.
		flightKey += "\x00" + opts.SandboxID
	} else {
		flightKey = opts.SandboxID
	}
	flight, leader := m.trafficTokenSingleflight.acquire(flightKey)
	if leader {
		// The result is shared with every waiter, so one HTTP client's
		// cancellation must not abort issuance for other callers. The independent
		// timeout carried by the lifecycle keeps the detached provider call bounded.
		issueCtx, finishIssue, ok := m.trafficTokenIssues.begin(ctx)
		if !ok {
			m.trafficTokenSingleflight.complete(flightKey, flight, infra.TrafficAccessToken{}, managererrors.NewError(managererrors.ErrorUnavailable, "sandbox manager is stopping"))
		} else {
			go func() {
				var result infra.TrafficAccessToken
				var issueErr error
				defer func() {
					panicked := recover() != nil
					if panicked {
						result = infra.TrafficAccessToken{}
						issueErr = managererrors.NewError(managererrors.ErrorInternal, "failed to issue traffic access token")
					}
					m.trafficTokenSingleflight.complete(flightKey, flight, result, issueErr)
					finishIssue()
					if panicked {
						klog.FromContext(issueCtx).Error(errTrafficTokenIssuerPanic, "panic issuing traffic access token",
							"sandboxID", opts.SandboxID,
							"stack", string(debug.Stack()))
					}
				}()

				result, issueErr = m.infra.IssueTrafficAccessToken(issueCtx, infra.IssueTrafficAccessTokenOptions{
					Namespace:    opts.Namespace,
					SandboxID:    opts.SandboxID,
					TokenOptions: m.trafficTokenOptions,
					Validate: func(sandbox infra.Sandbox) error {
						return validateTrafficTokenSandbox(sandbox, opts.User)
					},
				})
				if issueErr != nil && managererrors.GetErrCode(issueErr) == managererrors.ErrorUnknown {
					if errors.Is(issueErr, infra.ErrSandboxNotFound) {
						issueErr = managererrors.NewError(managererrors.ErrorNotFound, "sandbox %s not found", opts.SandboxID)
					} else {
						issueErr = managererrors.WrapError(managererrors.ErrorUnavailable, issueErr, "failed to issue traffic access token: %v", issueErr)
					}
				}
			}()
		}
	}

	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.Canceled) {
			klog.FromContext(ctx).V(2).Info("stopped waiting for traffic access token issuance", "sandboxID", opts.SandboxID, "reason", ctx.Err())
		} else {
			klog.FromContext(ctx).Error(ctx.Err(), "failed waiting for traffic access token issuance", "sandboxID", opts.SandboxID)
		}
		return infra.TrafficAccessToken{}, managererrors.WrapError(managererrors.ErrorUnavailable, ctx.Err(), "waiting for traffic access token issuance")
	case <-flight.done:
		if flight.err != nil {
			return infra.TrafficAccessToken{}, flight.err
		}
		return flight.result, nil
	}
}

// trafficTokenIssueLifecycle bounds and drains detached traffic-token issuances.
//
// Issuance runs in a goroutine detached from the requesting HTTP context so one
// client's cancellation cannot abort work shared with other waiters. Detachment
// raises two questions this lifecycle answers:
//
//   - Bound: every issuance gets an independent timeout (default 30s), so a
//     hung provider cannot keep a flight open forever.
//   - Drain: stop refuses new issuances, cancels every in-flight one through
//     the lifecycle context, and waits for them to finish before the process
//     exits.
//
// Invariant: the stopped check and the WaitGroup increment in begin happen
// under the same mutex, so stop's drain can never miss an admitted issuance.
type trafficTokenIssueLifecycle struct {
	mu sync.Mutex
	// ctx is the process-level lifecycle context; cancelling it cancels every
	// in-flight issuance. nil until init or the first begin.
	ctx     context.Context
	cancel  context.CancelFunc
	stopped bool
	// wg tracks in-flight issuances so stop can drain them.
	wg sync.WaitGroup
	// timeout bounds each detached issuance. A non-positive value falls back to
	// defaultTrafficTokenIssueTimeout.
	timeout time.Duration
}

// init binds the lifecycle context to the manager's run context. Called from
// Run; begin tolerates init never running.
func (l *trafficTokenIssueLifecycle) init(parent context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ctx == nil && !l.stopped {
		l.ctx, l.cancel = context.WithCancel(parent)
	}
}

// begin admits one detached issuance. It returns:
//
//   - issueCtx: context for the issuance goroutine. It carries the request's
//     values (logger, tracing) but not its cancellation, is bounded by the
//     lifecycle timeout, and is canceled when the lifecycle stops.
//   - finish: cleanup the caller must invoke exactly once after issuance
//     returns (success or panic); it detaches the shutdown hook and reports
//     completion to stop's drain.
//   - ok: false once the manager is stopping; the caller must fail the flight
//     without starting work.
//
// If init never ran (a refresh arrives before Run), the lifecycle context
// falls back to context.Background; stop can still cancel and drain it.
func (l *trafficTokenIssueLifecycle) begin(requestCtx context.Context) (context.Context, func(), bool) {
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		return nil, nil, false
	}
	if l.ctx == nil {
		l.ctx, l.cancel = context.WithCancel(context.Background())
	}
	lifecycleCtx := l.ctx
	timeout := l.timeout
	// Admit under the same lock that checks stopped: after stop sets stopped,
	// no new issuance passes here, and every issuance admitted before that
	// already incremented wg, so stop's drain observes all of them.
	l.wg.Add(1)
	l.mu.Unlock()

	if timeout <= 0 {
		timeout = defaultTrafficTokenIssueTimeout
	}
	// Detach from the request's cancellation while keeping its values, then
	// cap the detached work with an independent timeout.
	issueCtx, cancel := context.WithTimeout(context.WithoutCancel(requestCtx), timeout)
	// Bridge process shutdown into this issuance: when the lifecycle context
	// is canceled, cancel issueCtx too. finish stops the hook so a completed
	// issuance does not leak it.
	stopLifecycleCancel := context.AfterFunc(lifecycleCtx, cancel)
	return issueCtx, func() {
		stopLifecycleCancel()
		cancel()
		l.wg.Done()
	}, true
}

// stop shuts the lifecycle down in three steps:
//
//  1. gate: refuse new issuances (begin returns !ok afterwards);
//  2. broadcast: cancel the lifecycle context, which fires every shutdown
//     hook installed by begin and cancels each in-flight issueCtx;
//  3. drain: wait for every admitted issuance to finish, bounded by ctx so a
//     hung provider cannot block process shutdown indefinitely.
//
// Returns ctx.Err() when the drain times out; callers log and continue.
func (l *trafficTokenIssueLifecycle) stop(ctx context.Context) error {
	l.mu.Lock()
	// Step 1: gate. Pairs with the stopped check in begin.
	l.stopped = true
	if l.cancel != nil {
		// Step 2: broadcast cancellation to all in-flight issuances.
		l.cancel()
	}
	l.mu.Unlock()

	// Step 3: drain. Every begin admitted before the gate closed calls
	// finish -> wg.Done, so wg.Wait converges once they all return.
	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

func validateTrafficTokenSandbox(sandbox infra.Sandbox, user string) error {
	route, err := sandbox.GetRoute()
	if err != nil {
		return managererrors.NewError(managererrors.ErrorUnavailable, "failed to resolve sandbox route: %v", err)
	}
	if route.Owner != user {
		return managererrors.NewError(managererrors.ErrorNotAllowed, "sandbox %s is not owned", sandbox.GetSandboxID())
	}
	state, reason := sandbox.GetState()
	if state != v1alpha1.SandboxStateRunning && state != v1alpha1.SandboxStatePaused {
		return managererrors.NewError(managererrors.ErrorConflict, "sandbox %s cannot refresh traffic access token in state %s (%s)", sandbox.GetSandboxID(), state, reason)
	}
	if !route.RequireTrafficAuth {
		return managererrors.NewError(managererrors.ErrorConflict, "sandbox %s does not enable traffic JWT authentication", sandbox.GetSandboxID())
	}
	return nil
}

type trafficTokenFlight struct {
	done   chan struct{}
	result infra.TrafficAccessToken
	err    error
}

type trafficTokenSingleflight struct {
	mu      sync.Mutex
	flights map[string]*trafficTokenFlight
}

func newTrafficTokenSingleflight() *trafficTokenSingleflight {
	return &trafficTokenSingleflight{flights: make(map[string]*trafficTokenFlight)}
}

func (s *trafficTokenSingleflight) acquire(key string) (flight *trafficTokenFlight, leader bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if flight := s.flights[key]; flight != nil {
		return flight, false
	}
	flight = &trafficTokenFlight{done: make(chan struct{})}
	s.flights[key] = flight
	return flight, true
}

func (s *trafficTokenSingleflight) complete(key string, flight *trafficTokenFlight, result infra.TrafficAccessToken, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	flight.result = result
	flight.err = err
	if s.flights[key] == flight {
		delete(s.flights, key)
	}
	close(flight.done)
}
