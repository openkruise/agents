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
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WaitTask packages a WaitAction with its Update and Check funcs so that callers
// cannot accidentally pair the same (object, action) with different checkers.
// Instances MUST be created via *cache.Cache factory methods (NewSandboxPauseTask
// etc.). The zero value is not usable.
//
// Immutability:
//   - All fields are unexported; once constructed by a factory, callers can only
//     invoke Wait(timeout).
//   - The caller ctx is captured at construction time because UpdateFunc[T] has
//     no ctx parameter; passing a different ctx to Wait would leak across the
//     Update closure.
type WaitTask[T client.Object] struct {
	ctx       context.Context
	waitHooks *sync.Map
	action    WaitAction
	object    T
	update    UpdateFunc[T]
	check     CheckFunc[T]
}

// NewWaitTask builds a WaitTask. Exported only so that the cache package (which
// is a sibling package, not utils) can construct instances inside its factory
// methods; production code outside pkg/cache MUST NOT call this directly — use
// the *cache.Cache.NewXxxTask factories instead.
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
		action:    action,
		object:    object,
		update:    update,
		check:     check,
	}
}

// Action returns the underlying wait action (read-only accessor, used by tests).
func (t *WaitTask[T]) Action() WaitAction { return t.action }

// Object returns the subject object (read-only accessor, used by tests).
func (t *WaitTask[T]) Object() T { return t.object }

// Wait blocks until the task's Check func reports satisfied, the task's captured
// ctx is canceled, or timeout elapses. It is a thin forwarder to
// WaitForObjectSatisfied and preserves the existing semantics bit-for-bit.
func (t *WaitTask[T]) Wait(timeout time.Duration) error {
	return WaitForObjectSatisfied[T](
		t.ctx, t.waitHooks, t.object, t.action, t.update, t.check, timeout,
	)
}
