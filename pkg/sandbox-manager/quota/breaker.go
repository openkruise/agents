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

package quota

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	defaultBreakerN = 3
	defaultBreakerD = 30 * time.Second
)

// BreakerBackend wraps a Backend and fails fast after repeated acquire failures.
type BreakerBackend struct {
	// inner is the backend protected by the breaker.
	inner Backend
	// n is the number of consecutive failures required to open the breaker.
	n int
	// d is how long the breaker stays open before a half-open probe.
	d time.Duration
	// now returns the current time and is replaceable in tests.
	now func() time.Time

	// mu protects breaker state.
	mu sync.Mutex
	// consecutiveErr counts backend failures since the last successful acquire.
	consecutiveErr int
	// openedUntil marks the end of the open period; acquire calls fail fast before it.
	openedUntil time.Time
	// halfOpen means the open period ended and one probe acquire is checking recovery.
	halfOpen bool
}

func NewBreakerBackend(inner Backend, n int, d time.Duration) *BreakerBackend {
	if n <= 0 {
		n = defaultBreakerN
	}
	if d <= 0 {
		d = defaultBreakerD
	}
	return &BreakerBackend{
		inner: inner,
		n:     n,
		d:     d,
		now:   time.Now,
	}
}

func (b *BreakerBackend) Acquire(ctx context.Context, p AcquireParams) (err error) {
	return b.call("acquire", func() error {
		return b.inner.Acquire(ctx, p)
	})
}

func (b *BreakerBackend) Release(ctx context.Context, user, lockString string) error {
	return b.call("release", func() error {
		return b.inner.Release(ctx, user, lockString)
	})
}

// ListEntries and Cleanup intentionally bypass the breaker; Cleanup must keep trying the backend so API-key deletion can clear leaked quota state.
func (b *BreakerBackend) ListEntries(ctx context.Context, user string) (map[string]Entry, error) {
	entries, err := b.inner.ListEntries(ctx, user)
	return entries, breakerError(err)
}

func (b *BreakerBackend) Cleanup(ctx context.Context, user string) error {
	return breakerError(b.inner.Cleanup(ctx, user))
}

func (b *BreakerBackend) call(op string, fn func() error) (err error) {
	if err = b.beforeCall(); err != nil {
		return err
	}
	defer func() {
		// Keep breaker state consistent if the wrapped backend panics; rethrow below.
		if recovered := recover(); recovered != nil {
			b.afterCall(fmt.Errorf("%w: panic in quota backend %s", ErrBackendUnavailable, op))
			panic(recovered)
		}
		b.afterCall(err)
		err = breakerError(err)
	}()
	err = fn()
	return err
}

func (b *BreakerBackend) beforeCall() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.now().Before(b.openedUntil) {
		// breaker is open, fail fast
		return ErrBackendUnavailable
	}
	if !b.openedUntil.IsZero() {
		// 1. breaker is open but may be about to close
		if b.halfOpen {
			// 3. the probe acquire failed, breaker stays open
			return ErrBackendUnavailable
		}
		// 2.1. allow only one probe acquire to check recovery
		b.halfOpen = true
		breakerStateTotal.WithLabelValues("half_open").Inc()
	}
	return nil
}

func (b *BreakerBackend) afterCall(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err == nil || errors.Is(err, ErrQuotaExceeded) {
		// acquire succeeded (no error or quota exceeded), close the breaker
		if !b.openedUntil.IsZero() || b.halfOpen {
			breakerStateTotal.WithLabelValues("closed").Inc()
		}
		b.consecutiveErr = 0
		b.openedUntil = time.Time{}
		b.halfOpen = false
		return
	}

	if b.halfOpen {
		// 2.2. the probe acquire failed, re-open the breaker
		b.open()
		return
	}

	// the breaker opens for too many failures
	b.consecutiveErr++
	if b.consecutiveErr < b.n {
		return
	}
	b.open()
}

func (b *BreakerBackend) open() {
	b.consecutiveErr = 0
	b.halfOpen = false
	b.openedUntil = b.now().Add(b.d)
	breakerStateTotal.WithLabelValues("open").Inc()
	breakerOpenDurationSeconds.Observe(b.d.Seconds())
}

func breakerError(err error) error {
	if err == nil || errors.Is(err, ErrQuotaExceeded) || errors.Is(err, ErrBackendUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
}
