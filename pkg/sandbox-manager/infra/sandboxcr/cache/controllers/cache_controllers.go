/*
Copyright 2025.

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

package controllers

import (
	"context"
	"sync"

	cacheutils "github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr/cache/utils"

	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	managerutils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// CustomReconcileHandler is a custom reconcile handler.
type CustomReconcileHandler[T client.Object] func(ctx context.Context, obj T, notFound bool) (ctrl.Result, error)

// CustomReconciler allows for multiple reconcile handlers to be added to a single controller.
type CustomReconciler[T client.Object] struct {
	client.Client
	Name      string
	Scheme    *runtime.Scheme
	handlers  []CustomReconcileHandler[T]
	NewObject func() T
}

func (c *CustomReconciler[T]) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = logs.NewContextFrom(ctx, "reconciler", c.Name, "request", req.String())
	log := klog.FromContext(ctx)

	if len(c.handlers) == 0 {
		log.V(consts.DebugLogLevel).Info("reconcile skipped for no handlers")
		return ctrl.Result{}, nil
	}

	obj := c.NewObject()
	var notFound bool
	err := c.Get(ctx, req.NamespacedName, obj)
	if err != nil {
		if errors.IsNotFound(err) {
			notFound = true
			obj.SetNamespace(req.Namespace)
			obj.SetName(req.Name)
			managerutils.ResourceVersionExpectationDelete(obj)
		} else {
			log.Error(err, "failed to get SandboxSet")
			return ctrl.Result{}, err
		}
	}
	if !notFound {
		managerutils.ResourceVersionExpectationObserve(obj)
	}
	for _, handler := range c.handlers {
		result, err := handler(ctx, obj, notFound)
		if err != nil {
			return result, err
		}
	}
	return ctrl.Result{}, nil
}

// AddReconcileHandlers must be called before controller is started.
func (c *CustomReconciler[T]) AddReconcileHandlers(handler ...CustomReconcileHandler[T]) {
	c.handlers = append(c.handlers, handler...)
}

// WaitReconciler is a generic reconciler that checks wait hooks for a specific object type.
// It replicates the logic of addWaiterHandler[T].
type WaitReconciler[T client.Object] struct {
	client.Client
	Name      string
	Scheme    *runtime.Scheme
	waitHooks *sync.Map // Key: waitHookKey (type-namespace-name), Value: *WaitEntry[T]
	NewObject func() T
}

func (r *WaitReconciler[T]) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = logs.NewContextFrom(ctx, "reconciler", r.Name, "request", req.String())
	log := klog.FromContext(ctx)
	waitKey := cacheutils.WaitHookKeyFromRequest[T](req)

	obj := r.NewObject()
	err := r.Get(ctx, req.NamespacedName, obj)
	if err != nil {
		if errors.IsNotFound(err) {
			log.V(consts.DebugLogLevel).Info("object not found, may have been deleted")
			if entry, ok := r.loadWaitHook(waitKey); ok {
				entry.Close()
				log.V(consts.DebugLogLevel).Info("existing wait entry closed")
			}
			managerutils.ResourceVersionExpectationDelete(obj)
		} else {
			log.Error(err, "failed to get object")
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	managerutils.ResourceVersionExpectationObserve(obj)
	log.V(consts.DebugLogLevel).Info("object with wait hook changed", "resourceVersion", obj.GetResourceVersion())
	r.checkWaitHooks(waitKey, obj)
	return ctrl.Result{}, nil
}

func (r *WaitReconciler[T]) checkWaitHooks(key string, obj T) {
	entry, ok := r.loadWaitHook(key)
	if !ok {
		return
	}
	// If the wait context is already canceled, skip
	if entry.Context().Err() != nil {
		return
	}
	satisfied, err := entry.Check(obj)
	if satisfied || err != nil {
		entry.Close()
	}
}

func (r *WaitReconciler[T]) loadWaitHook(key string) (*cacheutils.WaitEntry[T], bool) {
	if r.waitHooks == nil {
		return nil, false
	}
	value, ok := r.waitHooks.Load(key)
	if !ok {
		return nil, false
	}
	entry, ok := value.(*cacheutils.WaitEntry[T])
	if !ok {
		return nil, false
	}
	return entry, true
}

// CacheControllerHandlers holds references to the custom reconcilers
// so that external callers can register event handlers via AddEventHandler.
type CacheControllerHandlers struct {
	SandboxCustomReconciler    *CacheSandboxCustomReconciler
	SandboxSetCustomReconciler *CacheSandboxSetCustomReconciler
}

// SetupCacheControllersWithManager registers all cache controllers with the given manager
// and returns a CacheControllerHandlers struct for registering custom event handlers.
// The waitHooks parameter is shared with the Cache instance.
func SetupCacheControllersWithManager(mgr manager.Manager, waitHooks *sync.Map) (*CacheControllerHandlers, error) {
	if err := AddCacheSandboxWaitReconciler(mgr, waitHooks); err != nil {
		return nil, err
	}
	if err := AddCacheCheckpointWaitReconciler(mgr, waitHooks); err != nil {
		return nil, err
	}
	sandboxCustomReconciler, err := AddCacheSandboxCustomReconciler(mgr)
	if err != nil {
		return nil, err
	}
	sandboxSetCustomReconciler, err := AddCacheSandboxSetCustomReconciler(mgr)
	if err != nil {
		return nil, err
	}
	return &CacheControllerHandlers{
		SandboxCustomReconciler:    sandboxCustomReconciler,
		SandboxSetCustomReconciler: sandboxSetCustomReconciler,
	}, nil
}
