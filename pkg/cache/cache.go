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

package cache

import (
	"context"
	"fmt"
	"sync"

	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/controllers"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
)

// Cache is a controller-runtime based cache that replaces the legacy informer-based Cache.
type Cache struct {
	client        ctrlclient.Client
	reader        ctrlclient.Reader
	mgr           ctrl.Manager
	waitHooks     *sync.Map
	cancelFunc    context.CancelFunc
	indexGetGroup singleflight.Group
	controllers   *controllers.CacheControllerHandlers
}

// BuildCacheConfig creates the informer filter configuration for the cache.
// It returns a byObject map that configures per-object informer filtering based on resource scope.
// This configuration is shared between NewControllerManager (production) and NewTestCache (testing)
// to ensure consistent behavior.
//
// # Informer Filter Options
//
// A — Custom resources (sandbox namespace + optional label selector):
//
//	Sandbox, SandboxSet, Checkpoint, SandboxTemplate
//
// B — System namespace resources (requires opts.SystemNamespace to be set):
//
//	Secret, ConfigMap
//
// C — Cluster-scoped resources (no namespace filtering):
//
//	PersistentVolume
func BuildCacheConfig(opts config.SandboxManagerOptions) (map[ctrlclient.Object]ctrlcache.ByObject, error) {
	// Parse label selector if configured
	var labelSelector labels.Selector
	if opts.SandboxLabelSelector != "" {
		var err error
		labelSelector, err = labels.Parse(opts.SandboxLabelSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to parse sandbox label selector %q: %w", opts.SandboxLabelSelector, err)
		}
	}

	// Configure per-object informer filtering.
	// Note: UnsafeDisableDeepCopy is set globally via DefaultUnsafeDisableDeepCopy
	// in NewControllerManager, so per-object and per-call settings are unnecessary.
	byObject := map[ctrlclient.Object]ctrlcache.ByObject{}

	// Custom resources: namespace + label filtering.
	customObjConfig := ctrlcache.ByObject{}
	if opts.SandboxNamespace != "" {
		customObjConfig.Namespaces = map[string]ctrlcache.Config{
			opts.SandboxNamespace: {},
		}
	}
	if labelSelector != nil {
		customObjConfig.Label = labelSelector
	}
	byObject[&agentsv1alpha1.Sandbox{}] = customObjConfig
	byObject[&agentsv1alpha1.SandboxSet{}] = customObjConfig
	byObject[&agentsv1alpha1.Checkpoint{}] = customObjConfig
	byObject[&agentsv1alpha1.SandboxTemplate{}] = customObjConfig

	// System namespace resources
	if opts.SystemNamespace != "" {
		sysNsConfig := ctrlcache.ByObject{
			Namespaces: map[string]ctrlcache.Config{
				opts.SystemNamespace: {},
			},
		}
		byObject[&corev1.Secret{}] = sysNsConfig
		byObject[&corev1.ConfigMap{}] = sysNsConfig
	}

	// Cluster-scoped resources
	byObject[&corev1.PersistentVolume{}] = ctrlcache.ByObject{}

	return byObject, nil
}

// NewControllerManager creates a controller-runtime manager configured for the sandbox manager cache.
// It configures informer filtering based on resource scope and returns a manager
// that must be passed to NewCache.
func NewControllerManager(cfg *rest.Config, opts config.SandboxManagerOptions) (ctrl.Manager, error) {
	if cfg == nil {
		return nil, errors.NewBadRequest("rest config cannot be nil")
	}
	// Create scheme for controller manager
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agentsv1alpha1.AddToScheme(scheme))

	// Build cache configuration with informer filtering
	byObject, err := BuildCacheConfig(opts)
	if err != nil {
		return nil, err
	}

	// Create manager with unnecessary features disabled
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Cache:                  ctrlcache.Options{ByObject: byObject, DefaultUnsafeDisableDeepCopy: ptr.To(true)},
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "",
		LeaderElection:         false,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create controller manager: %w", err)
	}

	return mgr, nil
}

// NewCache creates a new Cache instance from a pre-configured controller manager.
// The metadata must have been returned by NewControllerManager.
func NewCache(mgr ctrl.Manager) (*Cache, error) {
	waitHooks := &sync.Map{}
	handlers, err := controllers.SetupCacheControllersWithManager(mgr, waitHooks)
	if err != nil {
		return nil, fmt.Errorf("failed to setup cache controllers: %w", err)
	}
	// Register field indexes
	if err := AddIndexesToCache(mgr.GetCache()); err != nil {
		return nil, fmt.Errorf("failed to add indexes to cache: %w", err)
	}

	return &Cache{
		client:      mgr.GetClient(),
		reader:      mgr.GetAPIReader(),
		mgr:         mgr,
		waitHooks:   waitHooks,
		controllers: handlers,
	}, nil
}

// Run starts the controller manager and waits for cache sync.
func (c *Cache) Run(ctx context.Context) error {
	log := klog.FromContext(ctx)
	mgrCtx, cancel := context.WithCancel(ctx)
	c.cancelFunc = cancel
	go func() {
		// It is possible that the mgr is already started in another goroutine
		if err := c.mgr.Start(mgrCtx); err != nil {
			log.Error(err, "controller manager exited with error")
			panic(fmt.Errorf("failed to start controller manager: %w", err))
		}
	}()
	cache := c.mgr.GetCache()
	if cache != nil && !cache.WaitForCacheSync(ctx) {
		cancel()
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	log.V(consts.DebugLogLevel).Info("Cache started, caches synced")
	return nil
}

// Stop shuts down the controller manager.
func (c *Cache) Stop(ctx context.Context) {
	log := klog.FromContext(ctx)
	if c.cancelFunc != nil {
		c.cancelFunc()
	}
	log.V(consts.DebugLogLevel).Info("Cache stopped")
}

func (c *Cache) GetClaimedSandbox(ctx context.Context, opts GetClaimedSandboxOptions) (*agentsv1alpha1.Sandbox, error) {
	resultVal, err, _ := c.indexGetGroup.Do("claimed-sandbox:"+opts.Namespace+":"+opts.SandboxID, func() (any, error) {
		list := &agentsv1alpha1.SandboxList{}
		if err := listObjectWithUserAndNamespace(ctx, c.client, list, "", opts.Namespace, ctrlclient.MatchingFields{IndexClaimedSandboxID: opts.SandboxID}, ctrlclient.Limit(1)); err != nil {
			return nil, err
		}
		if len(list.Items) == 0 {
			return nil, fmt.Errorf("sandbox %s not found in cache", opts.SandboxID)
		}
		return &list.Items[0], nil
	})
	if err != nil {
		return nil, err
	}
	return resultVal.(*agentsv1alpha1.Sandbox), nil
}

func (c *Cache) GetCheckpoint(ctx context.Context, opts GetCheckpointOptions) (*agentsv1alpha1.Checkpoint, error) {
	resultVal, err, _ := c.indexGetGroup.Do("checkpoint-id:"+opts.Namespace+":"+opts.CheckpointID, func() (any, error) {
		list := &agentsv1alpha1.CheckpointList{}
		if err := listObjectWithUserAndNamespace(ctx, c.client, list, "", opts.Namespace, ctrlclient.MatchingFields{IndexCheckpointID: opts.CheckpointID}, ctrlclient.Limit(1)); err != nil {
			return nil, err
		}
		if len(list.Items) == 0 {
			return nil, fmt.Errorf("checkpoint %s not found in cache", opts.CheckpointID)
		}
		return &list.Items[0], nil
	})
	if err != nil {
		return nil, err
	}
	return resultVal.(*agentsv1alpha1.Checkpoint), nil
}

func (c *Cache) PickSandboxSet(ctx context.Context, opts PickSandboxSetOptions) (*agentsv1alpha1.SandboxSet, error) {
	resultVal, err, _ := c.indexGetGroup.Do("sandboxset-name:"+opts.Namespace+":"+opts.Name, func() (any, error) {
		list := &agentsv1alpha1.SandboxSetList{}
		if err := listObjectWithUserAndNamespace(ctx, c.client, list, "", opts.Namespace, ctrlclient.MatchingFields{IndexTemplateID: opts.Name}); err != nil {
			return nil, fmt.Errorf("failed to get sandboxset %s from cache: %w", opts.Name, err)
		}
		if len(list.Items) == 0 {
			return nil, fmt.Errorf("sandboxset %s not found in cache", opts.Name)
		}
		return &list.Items[0], nil
	})
	if err != nil {
		return nil, err
	}
	return resultVal.(*agentsv1alpha1.SandboxSet), nil
}

func listObjectWithUserAndNamespace[T ctrlclient.ObjectList](ctx context.Context, client ctrlclient.Client, list T, user, namespace string, listOpts ...ctrlclient.ListOption) error {
	if namespace != "" {
		listOpts = append(listOpts, ctrlclient.InNamespace(namespace))
	}
	if user != "" {
		listOpts = append(listOpts, ctrlclient.MatchingFields{IndexUser: user})
	}
	return client.List(ctx, list, listOpts...)
}

func (c *Cache) ListSandboxSets(ctx context.Context, opts ListSandboxSetsOptions) ([]*agentsv1alpha1.SandboxSet, error) {
	resultVal, err, _ := c.indexGetGroup.Do("sandboxsets:"+opts.Namespace, func() (any, error) {
		list := &agentsv1alpha1.SandboxSetList{}
		if err := listObjectWithUserAndNamespace(ctx, c.client, list, "", opts.Namespace); err != nil {
			return nil, err
		}
		result := make([]*agentsv1alpha1.SandboxSet, 0, len(list.Items))
		for i := range list.Items {
			result = append(result, &list.Items[i])
		}
		return result, nil
	})
	if err != nil {
		return nil, err
	}
	return resultVal.([]*agentsv1alpha1.SandboxSet), nil
}

func (c *Cache) ListSandboxes(ctx context.Context, opts ListSandboxesOptions) ([]*agentsv1alpha1.Sandbox, error) {
	list := &agentsv1alpha1.SandboxList{}
	if err := listObjectWithUserAndNamespace(ctx, c.client, list, opts.User, opts.Namespace); err != nil {
		return nil, err
	}
	result := make([]*agentsv1alpha1.Sandbox, 0, len(list.Items))
	for i := range list.Items {
		result = append(result, &list.Items[i])
	}
	return result, nil
}

func (c *Cache) ListCheckpoints(ctx context.Context, opts ListCheckpointsOptions) ([]*agentsv1alpha1.Checkpoint, error) {
	list := &agentsv1alpha1.CheckpointList{}
	if err := listObjectWithUserAndNamespace(ctx, c.client, list, opts.User, opts.Namespace); err != nil {
		return nil, err
	}
	result := make([]*agentsv1alpha1.Checkpoint, 0, len(list.Items))
	for i := range list.Items {
		result = append(result, &list.Items[i])
	}
	return result, nil
}

func (c *Cache) ListSandboxesInPool(ctx context.Context, opts ListSandboxesInPoolOptions) ([]*agentsv1alpha1.Sandbox, error) {
	resultVal, err, _ := c.indexGetGroup.Do("sandbox-pool:"+opts.Namespace+":"+opts.Pool, func() (any, error) {
		list := &agentsv1alpha1.SandboxList{}
		if err := listObjectWithUserAndNamespace(ctx, c.client, list, "", opts.Namespace, ctrlclient.MatchingFields{IndexSandboxPool: opts.Pool}); err != nil {
			return nil, err
		}
		result := make([]*agentsv1alpha1.Sandbox, 0, len(list.Items))
		for i := range list.Items {
			result = append(result, &list.Items[i])
		}
		return result, nil
	})
	if err != nil {
		return nil, err
	}
	return resultVal.([]*agentsv1alpha1.Sandbox), nil
}

// refreshCheckpoint retrieves the latest Checkpoint from cache.
func (c *Cache) refreshCheckpoint(ctx context.Context, cp *agentsv1alpha1.Checkpoint) (*agentsv1alpha1.Checkpoint, error) {
	fresh := &agentsv1alpha1.Checkpoint{}
	err := c.client.Get(ctx, types.NamespacedName{Namespace: cp.Namespace, Name: cp.Name}, fresh)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("checkpoint %s not found in cache", cp.Name)
		}
		return nil, fmt.Errorf("failed to get checkpoint %s from cache: %w", cp.Name, err)
	}
	return fresh, nil
}

// GetSandboxController returns the sandbox custom reconciler for external handler registration.
func (c *Cache) GetSandboxController() *controllers.CacheSandboxCustomReconciler {
	return c.controllers.SandboxCustomReconciler
}

// GetSandboxSetController returns the sandboxset custom reconciler for external handler registration.
func (c *Cache) GetSandboxSetController() *controllers.CacheSandboxSetCustomReconciler {
	return c.controllers.SandboxSetCustomReconciler
}

func (c *Cache) GetClient() ctrlclient.Client {
	return c.client
}

func (c *Cache) GetAPIReader() ctrlclient.Reader {
	return c.reader
}

// GetWaitHooks returns the internal waitHooks map used for wait simulation.
// This is only intended for test infrastructure use.
func (c *Cache) GetWaitHooks() *sync.Map {
	return c.waitHooks
}

// GetMockManager extracts the MockManager from a Cache created by NewTestCache.
// This is only intended for test use.
func (c *Cache) GetMockManager() *controllers.MockManager {
	mgr, ok := c.mgr.(*controllers.MockManager)
	if !ok {
		return nil
	}
	return mgr
}
