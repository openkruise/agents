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

type breakerBackend struct {
	inner Backend
	n     int
	d     time.Duration
	now   func() time.Time

	mu             sync.Mutex
	consecutiveErr int
	openedUntil    time.Time
	halfOpen       bool
}

func NewBreakerBackend(inner Backend, n int, d time.Duration) *breakerBackend {
	if n <= 0 {
		n = defaultBreakerN
	}
	if d <= 0 {
		d = defaultBreakerD
	}
	return &breakerBackend{
		inner: inner,
		n:     n,
		d:     d,
		now:   time.Now,
	}
}

func (b *breakerBackend) Acquire(ctx context.Context, p AcquireParams) (err error) {
	if err = b.beforeCall(); err != nil {
		return err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			b.afterCall(fmt.Errorf("%w: panic in quota backend acquire", ErrBackendUnavailable))
			panic(recovered)
		}
		b.afterCall(err)
		err = breakerError(err)
	}()
	err = b.inner.Acquire(ctx, p)
	return err
}

func (b *breakerBackend) Release(ctx context.Context, user, lockString string) error {
	return breakerError(b.inner.Release(ctx, user, lockString))
}

func (b *breakerBackend) ListEntries(ctx context.Context, user string) (map[string]Entry, error) {
	entries, err := b.inner.ListEntries(ctx, user)
	return entries, breakerError(err)
}

func (b *breakerBackend) Cleanup(ctx context.Context, user string) error {
	return breakerError(b.inner.Cleanup(ctx, user))
}

func (b *breakerBackend) beforeCall() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.now().Before(b.openedUntil) {
		return ErrBackendUnavailable
	}
	if !b.openedUntil.IsZero() {
		if b.halfOpen {
			return ErrBackendUnavailable
		}
		b.halfOpen = true
		breakerStateTotal.WithLabelValues("half_open").Inc()
	}
	return nil
}

func (b *breakerBackend) afterCall(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err == nil || errors.Is(err, ErrQuotaExceeded) {
		if !b.openedUntil.IsZero() || b.halfOpen {
			breakerStateTotal.WithLabelValues("closed").Inc()
		}
		b.consecutiveErr = 0
		b.openedUntil = time.Time{}
		b.halfOpen = false
		return
	}

	if b.halfOpen {
		b.open()
		return
	}

	b.consecutiveErr++
	if b.consecutiveErr < b.n {
		return
	}

	// ponytail: single breaker for the one Redis backend; split per shard only if multi-Redis arrives.
	b.open()
}

func (b *breakerBackend) open() {
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
