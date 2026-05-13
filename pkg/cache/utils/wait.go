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

	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
)

type WaitAction string

const (
	WaitActionResume     WaitAction = "Resume"
	WaitActionPause      WaitAction = "Pause"
	WaitActionWaitReady  WaitAction = "WaitReady"
	WaitActionCheckpoint WaitAction = "Checkpoint"
)

// defaultWaitPollInterval is the interval for the ticker-based polling fallback.
// When the event-driven WaitReconciler fails to close the wait entry (due to
// transient errors, controller backoff, or queue delays), the polling ticker
// provides an independent second path that periodically re-checks whether the
// condition is satisfied. Both the ticker and the reconciler read from the same
// informer cache, so the ticker cannot on its own overcome informer staleness
// caused by watch connection loss before a re-list completes.
var defaultWaitPollInterval = 10 * time.Second

type CheckFunc[T client.Object] func(obj T) (bool, error)
type UpdateFunc[T client.Object] func(obj T) (T, error)

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

	ctx       context.Context
	done      chan struct{}
	checker   CheckFunc[T]
	closeOnce sync.Once

	mu   sync.Mutex
	refs int
}

func NewWaitEntry[T client.Object](ctx context.Context, action WaitAction, checker CheckFunc[T]) *WaitEntry[T] {
	return &WaitEntry[T]{
		ctx:     ctx,
		Action:  action,
		checker: checker,
		done:    make(chan struct{}),
	}
}

// AcquireEntry increments the entry refcount only after confirming that the
// map still points at that entry while holding entry.mu. Together with
// ReleaseEntry holding the same lock across refs-- and CompareAndDelete, this
// prevents a waiter from acquiring an orphan entry that was just removed.
//
// A same-action late joiner may still acquire an entry whose done channel has
// already been closed but has not yet been released from the map. That is
// intentional for the current wait flow: Close is triggered from the same
// informer/cache object that later post-acquire and double-check reads use, so
// the late joiner should observe the satisfied state immediately and return.
func AcquireEntry[T client.Object](waitHooks *sync.Map, key string, action WaitAction, newEntry func() *WaitEntry[T]) (*WaitEntry[T], error) {
	for {
		value, _ := waitHooks.LoadOrStore(key, newEntry())
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

func (e *WaitEntry[T]) Close() {
	e.closeOnce.Do(func() {
		close(e.done)
	})
}

func (e *WaitEntry[T]) Done() <-chan struct{} {
	return e.done
}

func (e *WaitEntry[T]) Context() context.Context {
	return e.ctx
}

func (e *WaitEntry[T]) Check(obj T) (bool, error) {
	return e.checker(obj)
}

func WaitForObjectSatisfied[T client.Object](ctx context.Context, waitHooks *sync.Map, obj T, action WaitAction,
	update UpdateFunc[T], satisfiedFunc CheckFunc[T], timeout time.Duration) error {
	key := WaitHookKey(obj)
	objKey := client.ObjectKeyFromObject(obj)
	log := klog.FromContext(ctx).V(consts.DebugLogLevel).WithValues("waitHookKey", key, "object", objKey)
	satisfied, err := satisfiedFunc(obj)
	if satisfied || err != nil {
		log.Info("no need to wait for satisfied", "satisfied", satisfied, "error", err)
		return err
	}
	if timeout <= 0 {
		log.Info("waiting is skipped due to zero timeout")
		return &WaitNotSatisfiedError{Object: objKey, Action: action}
	}
	entry, err := AcquireEntry[T](waitHooks, key, action, func() *WaitEntry[T] {
		return NewWaitEntry(ctx, action, satisfiedFunc)
	})
	if err != nil {
		log.Error(err, "wait hook conflict", "new", action)
		return err
	}
	log.Info("wait hook acquired", "action", action)
	defer func() {
		ReleaseEntry[T](waitHooks, key, entry)
		log.Info("wait hook released")
	}()

	return waitForAcquiredObjectSatisfied(ctx, entry, obj, update, satisfiedFunc, timeout)
}

func waitForAcquiredObjectSatisfied[T client.Object](ctx context.Context, entry *WaitEntry[T], obj T,
	update UpdateFunc[T], satisfiedFunc CheckFunc[T], timeout time.Duration) error {
	log := klog.FromContext(ctx).V(consts.DebugLogLevel).WithValues("object", client.ObjectKeyFromObject(obj))
	satisfied, err := CheckObjectSatisfied(ctx, obj, update, satisfiedFunc)
	if satisfied || err != nil {
		log.Info("post-acquire satisfaction check completed", "satisfied", satisfied, "error", err)
		return err
	}
	if timeout <= 0 {
		log.Info("waiting is skipped due to zero timeout")
		return fmt.Errorf("object is not satisfied")
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Polling ticker serves as an independent fallback for the event-driven path
	// (entry.Done). It guards against scenarios where the WaitReconciler fails to
	// call entry.Close() — for example, when the reconciler encounters a transient
	// Get error, is delayed by controller-runtime backoff, or the checkWaitHooks
	// logic does not fire. The ticker runs on a fixed interval independent of the
	// controller-runtime event queue.
	//
	// Note: the ticker reads from the same informer cache as the reconciler via
	// the update function, so it does not bypass informer staleness caused by
	// watch event loss. In those cases recovery still depends on the informer
	// re-listing on its own.
	ticker := time.NewTicker(defaultWaitPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-entry.Done():
			log.Info("satisfied signal received")
			return DoubleCheckObjectSatisfied(ctx, obj, update, satisfiedFunc)
		case <-ticker.C:
			satisfied, err := CheckObjectSatisfied(ctx, obj, update, satisfiedFunc)
			if err != nil {
				return err
			}
			if satisfied {
				log.Info("object satisfied by polling check")
				return nil
			}
		case <-waitCtx.Done():
			log.Info("stop waiting for object satisfied: context canceled", "reason", waitCtx.Err())
			return DoubleCheckObjectSatisfied(ctx, obj, update, satisfiedFunc)
		}
	}
}

func CheckObjectSatisfied[T client.Object](ctx context.Context, obj T, update UpdateFunc[T], satisfiedFunc CheckFunc[T]) (bool, error) {
	log := klog.FromContext(ctx).WithValues("object", klog.KObj(obj))
	updated, err := update(obj)
	if err != nil {
		log.Error(err, "failed to get object while checking satisfaction")
		return false, err
	}
	satisfied, err := satisfiedFunc(updated)
	if err != nil {
		log.Error(err, "failed to check object satisfied")
		return false, err
	}
	return satisfied, nil
}

func DoubleCheckObjectSatisfied[T client.Object](ctx context.Context, obj T, update UpdateFunc[T], satisfiedFunc CheckFunc[T]) error {
	log := klog.FromContext(ctx).WithValues("object", klog.KObj(obj))
	satisfied, err := CheckObjectSatisfied(ctx, obj, update, satisfiedFunc)
	if err != nil {
		return err
	}
	if !satisfied {
		err = &WaitNotSatisfiedError{Object: client.ObjectKeyFromObject(obj), DuringDoubleCheck: true}
		log.Error(err, "object not satisfied")
		return err
	}
	return nil
}
