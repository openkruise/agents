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

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/pkg/utils/expectations"
)

type WaitAction string

const (
	WaitActionResume     WaitAction = "Resume"
	WaitActionPause      WaitAction = "Pause"
	WaitActionWaitReady  WaitAction = "WaitReady"
	WaitActionCheckpoint WaitAction = "Checkpoint"
	WaitActionPVCBind    WaitAction = "PVCBind"
)

// Polling covers missed informer notifications.
var defaultWaitPollInterval = 10 * time.Second

type CheckFunc[T client.Object] func(obj T) (bool, error)
type UpdateFunc[T client.Object] func(ctx context.Context, obj T) (T, error)

// WaitHookKey generates a unique key for wait hooks by combining object type with namespace/name.
// This prevents key collisions when different resource types (e.g., Sandbox and Checkpoint)
// share the same namespace and name.
func WaitHookKey[T client.Object](obj T) string {
	return fmt.Sprintf("%T/%s/%s", obj, obj.GetNamespace(), obj.GetName())
}

// WaitHookKeyFromRequest generates a wait hook key from a reconcile request.
// This is useful in controller reconcilers where only the request is available.
func WaitHookKeyFromRequest[T client.Object](req ctrl.Request) string {
	var typeHint T
	return fmt.Sprintf("%T/%s/%s", typeHint, req.Namespace, req.Name)
}

type WaitEntry[T client.Object] struct {
	Action WaitAction

	mu      sync.Mutex
	refs    int
	waiters map[*objectWaiter[T]]struct{}
}

// AcquireEntry increments the entry refcount only after confirming that the
// map still points at that entry while holding entry.mu. Together with
// ReleaseEntry holding the same lock across refs-- and CompareAndDelete, this
// prevents a waiter from acquiring an orphan entry that was just removed.
func AcquireEntry[T client.Object](waitHooks *sync.Map, key string, action WaitAction) (*WaitEntry[T], error) {
	for {
		value, _ := waitHooks.LoadOrStore(key, &WaitEntry[T]{Action: action})
		entry := value.(*WaitEntry[T])

		entry.mu.Lock()
		current, ok := waitHooks.Load(key)
		if !ok || current != entry {
			entry.mu.Unlock()
			continue
		}
		if entry.Action != action {
			entry.mu.Unlock()
			return nil, &WaitTaskConflictError{ExistingAction: entry.Action, NewAction: action}
		}
		entry.refs++
		entry.mu.Unlock()
		return entry, nil
	}
}

// ReleaseEntry is the counterpart of AcquireEntry. It holds entry.mu while
// decrementing refs and, when the last waiter exits, while deleting the same
// entry from the map with CompareAndDelete. A concurrent AcquireEntry that saw
// this entry before deletion must acquire entry.mu first, then re-check the map
// before incrementing refs, so it cannot resurrect or retain an orphan entry.
func ReleaseEntry[T client.Object](waitHooks *sync.Map, key string, entry *WaitEntry[T]) {
	entry.mu.Lock()
	defer entry.mu.Unlock()

	entry.refs--
	if entry.refs == 0 {
		waitHooks.CompareAndDelete(key, entry)
	}
}

// Observe delivers this exact informer snapshot to every active waiter.
// A pre-acquired task starts listening only after its caller has written the spec.
func (e *WaitEntry[T]) Observe(obj T) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for w := range e.waiters {
		w.observe(obj, nil)
	}
}

// RequestRead asks each waiter to confirm an informer miss through its own reader.
func (e *WaitEntry[T]) RequestRead() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for w := range e.waiters {
		select {
		case w.read <- struct{}{}:
		default:
		}
	}
}

type objectWaiter[T client.Object] struct {
	target T
	check  CheckFunc[T]
	ctx    context.Context
	cancel context.CancelFunc
	read   chan struct{}

	// Protected by the entry mutex. Completion cancels an in-flight Get.
	object    T
	err       error
	completed bool
}

func (w *objectWaiter[T]) observe(obj T, err error) {
	if w.completed || w.ctx.Err() != nil {
		return
	}
	if err == nil {
		if !expectations.IsResourceVersionNewer(w.target.GetResourceVersion(), obj.GetResourceVersion()) {
			return
		}
		if uid := w.target.GetUID(); uid != "" && obj.GetUID() != uid {
			err = fmt.Errorf("object %s UID changed while waiting: expected %s, got %s", client.ObjectKeyFromObject(w.target), uid, obj.GetUID())
		} else {
			var satisfied bool
			satisfied, err = w.check(obj)
			if !satisfied && err == nil {
				return
			}
		}
	}
	if w.ctx.Err() == nil {
		w.object, w.err, w.completed = obj, err, true
		w.cancel()
	}
}

func (e *WaitEntry[T]) wait(ctx context.Context, target T, update UpdateFunc[T], check CheckFunc[T], timeout time.Duration) (T, error) {
	var zero T
	start := time.Now()
	timeoutErr := &WaitNotSatisfiedError{Action: e.Action, Object: client.ObjectKeyFromObject(target), Timeout: timeout}
	// A non-positive timeout expires readCtx immediately, so the wait reports
	// not-satisfied without reading.
	readCtx, cancel := context.WithTimeoutCause(ctx, timeout, timeoutErr)
	defer cancel()
	w := &objectWaiter[T]{target: target, check: check, ctx: readCtx, cancel: cancel, read: make(chan struct{}, 1)}
	e.mu.Lock()
	if e.waiters == nil {
		e.waiters = make(map[*objectWaiter[T]]struct{})
	}
	e.waiters[w] = struct{}{}
	e.mu.Unlock()
	ticker := time.NewTicker(defaultWaitPollInterval)
	defer ticker.Stop()
	for readCtx.Err() == nil {
		obj, err := update(readCtx, target)
		e.mu.Lock()
		w.observe(obj, err)
		e.mu.Unlock()
		select {
		case <-readCtx.Done():
			// Normal exit: observe cancels readCtx once a snapshot completes the
			// wait (satisfied or error). Timeouts and parent cancellation also land here.
		case <-w.read:
		case <-ticker.C:
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.waiters, w)
	if w.completed {
		return w.object, w.err
	}
	if readCtx.Err() == context.Canceled {
		return zero, context.Canceled
	}
	if readCtx.Err() == context.DeadlineExceeded && context.Cause(readCtx) != timeoutErr {
		deadline, _ := readCtx.Deadline()
		return zero, &WaitNotSatisfiedError{Action: e.Action, Object: timeoutErr.Object, Timeout: max(0, deadline.Sub(start)), cause: context.DeadlineExceeded}
	}
	return zero, timeoutErr
}
