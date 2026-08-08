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
	"sync"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

// RefreshTrafficAccessTokenOptions identifies the caller and Sandbox for an
// explicit traffic-token refresh.
type RefreshTrafficAccessTokenOptions struct {
	Namespace string
	SandboxID string
	User      string
}

// RefreshTrafficAccessToken issues a new token without mutating the Sandbox.
func (m *SandboxManager) RefreshTrafficAccessToken(ctx context.Context, opts RefreshTrafficAccessTokenOptions) (infra.TrafficAccessToken, error) {
	result, _, err := m.issueTrafficAccessToken(ctx, opts, false, true)
	return result, err
}

// BootstrapTrafficAccessToken issues a token for connect responses when the
// Sandbox requires traffic authentication. Unprotected Sandboxes are skipped.
func (m *SandboxManager) BootstrapTrafficAccessToken(ctx context.Context, opts RefreshTrafficAccessTokenOptions) (infra.TrafficAccessToken, bool, error) {
	return m.issueTrafficAccessToken(ctx, opts, true, false)
}

func (m *SandboxManager) issueTrafficAccessToken(ctx context.Context, opts RefreshTrafficAccessTokenOptions, allowUnprotected, enforceInterval bool) (infra.TrafficAccessToken, bool, error) {
	if opts.User == "" {
		return infra.TrafficAccessToken{}, false, managererrors.NewError(managererrors.ErrorBadRequest, "user is required")
	}
	if opts.SandboxID == "" {
		return infra.TrafficAccessToken{}, false, managererrors.NewError(managererrors.ErrorBadRequest, "sandbox ID is required")
	}

	sandbox, err := m.GetSandbox(ctx, opts.User, nil, infra.GetSandboxOptions{
		Namespace: opts.Namespace,
		SandboxID: opts.SandboxID,
	})
	if err != nil {
		return infra.TrafficAccessToken{}, false, err
	}
	if allowUnprotected && !sandbox.GetRoute().RequireTrafficAuth {
		return infra.TrafficAccessToken{}, false, nil
	}
	if err := validateTrafficTokenSandbox(sandbox, opts.User); err != nil {
		return infra.TrafficAccessToken{}, false, err
	}
	if m.trafficTokenLimiter == nil {
		return infra.TrafficAccessToken{}, false, managererrors.NewError(managererrors.ErrorInternal, "traffic access token limiter is not configured")
	}

	limiterKey := string(sandbox.GetUID())
	if limiterKey == "" {
		limiterKey = opts.SandboxID
	}
	flight, leader, ok := m.trafficTokenLimiter.acquire(limiterKey, enforceInterval)
	if !ok {
		return infra.TrafficAccessToken{}, false, managererrors.NewError(managererrors.ErrorRateLimited, "traffic access token refresh is rate limited")
	}
	if !leader {
		select {
		case <-ctx.Done():
			return infra.TrafficAccessToken{}, false, managererrors.NewError(managererrors.ErrorUnavailable, "waiting for traffic access token issuance: %v", ctx.Err())
		case <-flight.done:
			if flight.err != nil {
				return infra.TrafficAccessToken{}, false, flight.err
			}
			return flight.result, true, nil
		}
	}

	result, issueErr := m.infra.IssueTrafficAccessToken(ctx, infra.IssueTrafficAccessTokenOptions{
		Namespace:    opts.Namespace,
		SandboxID:    opts.SandboxID,
		TokenOptions: m.trafficTokenOptions,
		Validate: func(sandbox infra.Sandbox) error {
			return validateTrafficTokenSandbox(sandbox, opts.User)
		},
	})
	if issueErr != nil && managererrors.GetErrCode(issueErr) == managererrors.ErrorUnknown {
		if errors.Is(issueErr, cache.ErrSandboxNotFound) {
			issueErr = managererrors.NewError(managererrors.ErrorNotFound, "sandbox %s not found", opts.SandboxID)
		} else {
			issueErr = managererrors.NewError(managererrors.ErrorUnavailable, "failed to issue traffic access token: %v", issueErr)
		}
	}
	m.trafficTokenLimiter.complete(limiterKey, flight, result, issueErr)
	if issueErr != nil {
		return infra.TrafficAccessToken{}, false, issueErr
	}
	return result, true, nil
}

func validateTrafficTokenSandbox(sandbox infra.Sandbox, user string) error {
	if sandbox.GetRoute().Owner != user {
		return managererrors.NewError(managererrors.ErrorNotAllowed, "sandbox %s is not owned", sandbox.GetSandboxID())
	}
	state, reason := sandbox.GetState()
	if state != v1alpha1.SandboxStateRunning && state != v1alpha1.SandboxStatePaused {
		return managererrors.NewError(managererrors.ErrorConflict, "sandbox %s cannot refresh traffic access token in state %s (%s)", sandbox.GetSandboxID(), state, reason)
	}
	if !sandbox.GetRoute().RequireTrafficAuth {
		return managererrors.NewError(managererrors.ErrorConflict, "sandbox %s does not enable traffic JWT authentication", sandbox.GetSandboxID())
	}
	return nil
}

type trafficTokenLimitEntry struct {
	flight *trafficTokenFlight
	last   time.Time
}

type trafficTokenFlight struct {
	done   chan struct{}
	result infra.TrafficAccessToken
	err    error
}

type trafficTokenLimiter struct {
	mu            sync.Mutex
	entries       map[string]trafficTokenLimitEntry
	minInterval   time.Duration
	now           func() time.Time
	lastCleanupAt time.Time
}

func newTrafficTokenLimiter(minInterval time.Duration, now func() time.Time) *trafficTokenLimiter {
	return &trafficTokenLimiter{entries: make(map[string]trafficTokenLimitEntry), minInterval: minInterval, now: now}
}

func (l *trafficTokenLimiter) acquire(key string, enforceInterval bool) (flight *trafficTokenFlight, leader, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.cleanup(now)
	entry := l.entries[key]
	if entry.flight != nil {
		return entry.flight, false, true
	}
	if elapsed := now.Sub(entry.last); enforceInterval && !entry.last.IsZero() && elapsed < l.minInterval {
		return nil, false, false
	}
	flight = &trafficTokenFlight{done: make(chan struct{})}
	entry.flight = flight
	entry.last = now
	l.entries[key] = entry
	return flight, true, true
}

func (l *trafficTokenLimiter) complete(key string, flight *trafficTokenFlight, result infra.TrafficAccessToken, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	flight.result = result
	flight.err = err
	entry := l.entries[key]
	if entry.flight == flight {
		entry.flight = nil
		l.entries[key] = entry
	}
	close(flight.done)
}

func (l *trafficTokenLimiter) cleanup(now time.Time) {
	if !l.lastCleanupAt.IsZero() && now.Sub(l.lastCleanupAt) < l.minInterval {
		return
	}
	for key, entry := range l.entries {
		if entry.flight == nil && now.Sub(entry.last) >= l.minInterval {
			delete(l.entries, key)
		}
	}
	l.lastCleanupAt = now
}
