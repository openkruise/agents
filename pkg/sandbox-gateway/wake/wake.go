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
	"errors"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/sync/singleflight"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	timeoututils "github.com/openkruise/agents/pkg/utils/timeout"
)

var (
	ErrWakeDisabled    = errors.New("wake disabled")
	ErrSandboxNotFound = errors.New("sandbox not found")
	ErrUnauthorized    = errors.New("wake unauthorized")
	ErrWakeFailed      = errors.New("wake failed")
	ErrTransport       = errors.New("wake transport error")
)

// neverWakeConnectTimeoutSeconds is the placeholder timeout the gateway sends to
// /connect for a "timeout:never" sandbox. Never-timeout sandboxes keep a zero
// deadline through pause, so the manager's connect handler ignores this value
// (currentEndAt.IsZero()); we only send a positive number because the connect
// API rejects timeouts <= 0.
const neverWakeConnectTimeoutSeconds = 1

type E2BConnector interface {
	Connect(ctx context.Context, sandboxID string, timeoutSeconds int) (int, error)
}

type RouteRegistry interface {
	Get(id string) (proxy.Route, bool)
}

type Waker struct {
	Connector       E2BConnector
	Registry        RouteRegistry
	DetachedTimeout time.Duration
	RetryBackoff    time.Duration
	PollInterval    time.Duration

	group singleflight.Group
}

func NewWaker(connector E2BConnector) *Waker {
	return &Waker{
		Connector:       connector,
		Registry:        registry.GetRegistry(),
		DetachedTimeout: DetachedContextTimeout,
		RetryBackoff:    500 * time.Millisecond,
		PollInterval:    500 * time.Millisecond,
	}
}

func (w *Waker) WakeAndWait(filterCtx context.Context, sandboxID string, annotation string) error {
	cfg, enabled := timeoututils.ParseWakeOnTraffic(annotation)
	if !enabled {
		return ErrWakeDisabled
	}
	timeoutSeconds := cfg.TimeoutSeconds
	if cfg.Never {
		timeoutSeconds = neverWakeConnectTimeoutSeconds
	}

	for {
		if err := filterCtx.Err(); err != nil {
			return err
		}
		status, err := w.connectOnce(filterCtx, sandboxID, timeoutSeconds)
		if err != nil {
			return err
		}
		switch status {
		case http.StatusOK, http.StatusCreated:
			return w.pollRunning(filterCtx, sandboxID)
		case http.StatusConflict:
			if err := w.waitRetryBackoff(filterCtx); err != nil {
				return err
			}
		case http.StatusNotFound:
			return ErrSandboxNotFound
		case http.StatusUnauthorized, http.StatusForbidden:
			return ErrUnauthorized
		default:
			return fmt.Errorf("%w: status %d", ErrWakeFailed, status)
		}
	}
}

func (w *Waker) connectOnce(filterCtx context.Context, sandboxID string, timeoutSeconds int) (int, error) {
	if w.Connector == nil {
		return 0, fmt.Errorf("%w: connector is nil", ErrWakeFailed)
	}
	ch := w.group.DoChan(sandboxID, func() (interface{}, error) {
		detachedTimeout := w.DetachedTimeout
		if detachedTimeout <= 0 {
			detachedTimeout = DetachedContextTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), detachedTimeout)
		defer cancel()
		status, err := w.Connector.Connect(ctx, sandboxID, timeoutSeconds)
		if err != nil {
			return 0, fmt.Errorf("%w: %v", ErrTransport, err)
		}
		return status, nil
	})

	select {
	case <-filterCtx.Done():
		return 0, filterCtx.Err()
	case result := <-ch:
		if result.Err != nil {
			return 0, result.Err
		}
		status, ok := result.Val.(int)
		if !ok {
			return 0, fmt.Errorf("%w: unexpected connect result %T", ErrWakeFailed, result.Val)
		}
		return status, nil
	}
}

func (w *Waker) waitRetryBackoff(ctx context.Context) error {
	backoff := w.RetryBackoff
	if backoff <= 0 {
		backoff = 500 * time.Millisecond
	}
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (w *Waker) pollRunning(ctx context.Context, sandboxID string) error {
	reg := w.Registry
	if reg == nil {
		reg = registry.GetRegistry()
	}
	interval := w.PollInterval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		route, ok := reg.Get(sandboxID)
		if !ok {
			return ErrSandboxNotFound
		}
		if route.State == agentsv1alpha1.SandboxStateRunning {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
