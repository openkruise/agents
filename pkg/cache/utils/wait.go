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
}

func NewWaitEntry[T client.Object](ctx context.Context, action WaitAction, checker CheckFunc[T]) *WaitEntry[T] {
	return &WaitEntry[T]{
		ctx:     ctx,
		Action:  action,
		checker: checker,
		done:    make(chan struct{}),
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
		return fmt.Errorf("sandbox is not satisfied")
	}
	value, exists := waitHooks.LoadOrStore(key, NewWaitEntry(ctx, action, satisfiedFunc))
	if exists {
		log.Info("reuse existing wait hook")
	} else {
		log.Info("wait hook created")
	}
	entry := value.(*WaitEntry[T])
	if entry.Action != action {
		err := fmt.Errorf("another action(%s)'s wait task already exists", entry.Action)
		log.Error(err, "wait hook conflict", "existing", entry.Action, "new", action)
		return err
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer func() {
		cancel()
		waitHooks.Delete(key)
		log.Info("wait hook deleted")
	}()

	select {
	case <-entry.Done():
		log.Info("satisfied signal received")
		return DoubleCheckObjectSatisfied(ctx, obj, update, satisfiedFunc)
	case <-waitCtx.Done():
		log.Info("stop waiting for sandbox satisfied: context canceled", "reason", waitCtx.Err())
		return DoubleCheckObjectSatisfied(ctx, obj, update, satisfiedFunc)
	}
}

func DoubleCheckObjectSatisfied[T client.Object](ctx context.Context, obj T, update UpdateFunc[T], satisfiedFunc CheckFunc[T]) error {
	log := klog.FromContext(ctx).WithValues("object", klog.KObj(obj))
	updated, err := update(obj)
	if err != nil {
		log.Error(err, "failed to get sandbox while double checking")
		return err
	}
	satisfied, err := satisfiedFunc(updated)
	if err != nil {
		log.Error(err, "failed to check sandbox satisfied")
		return err
	}
	if !satisfied {
		err = fmt.Errorf("sandbox is not satisfied during double check")
		log.Error(err, "sandbox not satisfied")
		return err
	}
	return nil
}
