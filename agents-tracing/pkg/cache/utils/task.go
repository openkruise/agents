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

package utils

import (
	"context"
	"fmt"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WaitTask packages a WaitAction with its Update and Check funcs so that callers
// cannot accidentally pair the same (object, action) with different checkers.
// A task may be lazy, acquiring its wait hook during Wait, or pre-acquired,
// acquiring the wait hook during construction. Pre-acquired tasks are
// single-use: Wait releases the hook when it returns, and Release is safe to
// defer after construction as cleanup for paths that do not reach Wait. The
// zero value is not usable.
//
// Immutability:
//   - All fields are unexported; once constructed by a factory, callers can use
//     Wait(timeout), and pre-acquired callers may use Release to abandon an
//     unused task before Wait.
//   - The caller ctx is captured at construction time because UpdateFunc[T] has
//     no ctx parameter; passing a different ctx to Wait would leak across the
//     Update closure.
type WaitTask[T client.Object] struct {
	ctx       context.Context
	waitHooks *sync.Map
	key       string
	action    WaitAction
	object    T
	update    UpdateFunc[T]
	check     CheckFunc[T]

	entry       *WaitEntry[T]
	releaseOnce sync.Once

	lifecycleMu sync.Mutex
	started     bool
	released    bool
}

// NewWaitTask builds a lazy WaitTask. Exported only so that the cache package
// (which is a sibling package, not utils) can construct instances inside its
// factory methods; production code outside pkg/cache MUST NOT call this
// directly — use the *cache.Cache.NewXxxTask factories instead.
func NewWaitTask[T client.Object](
	ctx context.Context,
	waitHooks *sync.Map,
	action WaitAction,
	object T,
	update UpdateFunc[T],
	check CheckFunc[T],
) *WaitTask[T] {
	return &WaitTask[T]{
		ctx:       ctx,
		waitHooks: waitHooks,
		key:       WaitHookKey(object),
		action:    action,
		object:    object,
		update:    update,
		check:     check,
	}
}

// NewAcquiredWaitTask builds a single-use WaitTask that acquires its wait hook
// immediately. Callers may defer Release after successful construction; Wait
// releases the acquired hook when it returns, and the deferred Release is
// idempotent cleanup for paths that do not reach Wait.
func NewAcquiredWaitTask[T client.Object](
	ctx context.Context,
	waitHooks *sync.Map,
	action WaitAction,
	object T,
	update UpdateFunc[T],
	check CheckFunc[T],
) (*WaitTask[T], error) {
	task := NewWaitTask(ctx, waitHooks, action, object, update, check)
	entry, err := AcquireEntry[T](waitHooks, task.key, action, func() *WaitEntry[T] {
		return NewWaitEntry(ctx, action, check)
	})
	if err != nil {
		return nil, err
	}
	task.entry = entry
	return task, nil
}

// Action returns the underlying wait action (read-only accessor, used by tests).
func (t *WaitTask[T]) Action() WaitAction { return t.action }

// Object returns the subject object (read-only accessor, used by tests).
func (t *WaitTask[T]) Object() T { return t.object }

// Release abandons an unused pre-acquired task and releases its wait hook.
// Release is idempotent, is a no-op for lazy tasks, and does not release a hook
// while Wait is actively running.
func (t *WaitTask[T]) Release() {
	if t.entry == nil {
		return
	}
	t.lifecycleMu.Lock()
	if t.started || t.released {
		t.lifecycleMu.Unlock()
		return
	}
	t.released = true
	t.lifecycleMu.Unlock()
	t.releaseEntry()
}

func (t *WaitTask[T]) releaseEntry() {
	t.releaseOnce.Do(func() {
		ReleaseEntry[T](t.waitHooks, t.key, t.entry)
	})
}

// Wait blocks until the task's Check func reports satisfied, the task's captured
// ctx is canceled, or timeout elapses. Lazy tasks acquire and release their hook
// during Wait. Pre-acquired tasks are single-use, reuse their stored hook, and
// release it when Wait returns.
func (t *WaitTask[T]) Wait(timeout time.Duration) error {
	if t.entry != nil {
		if !t.claimAcquiredWait() {
			return fmt.Errorf("pre-acquired wait task already used or released")
		}
		defer t.releaseEntry()
		return waitForAcquiredObjectSatisfied[T](
			t.ctx, t.entry, t.object, t.update, t.check, timeout,
		)
	}
	return WaitForObjectSatisfied[T](
		t.ctx, t.waitHooks, t.object, t.action, t.update, t.check, timeout,
	)
}

func (t *WaitTask[T]) claimAcquiredWait() bool {
	t.lifecycleMu.Lock()
	defer t.lifecycleMu.Unlock()
	if t.started || t.released {
		return false
	}
	t.started = true
	return true
}
