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
	"time"

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
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	managerutils "github.com/openkruise/agents/pkg/utils/sandbox-manager/expectationutils"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
)

// Cache is a controller-runtime based cache that replaces the legacy informer-based Cache.
type Cache struct {
	client             ctrlclient.Client
	reader             ctrlclient.Reader
	mgr                ctrl.Manager
	waitHooks          *sync.Map
	cancelFunc         context.CancelFunc
	listSandboxesGroup singleflight.Group
	controllers        *controllers.CacheControllerHandlers
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
		if err := c.mgr.Start(mgrCtx); err != nil {
			log.Error(err, "controller manager exited with error")
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

// GetPersistentVolume looks up a cluster-scoped PersistentVolume by name.
func (c *Cache) GetPersistentVolume(name string) (*corev1.PersistentVolume, error) {
	pv := &corev1.PersistentVolume{}
	err := c.client.Get(context.Background(), types.NamespacedName{Name: name}, pv)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("persistentvolume %s not found in cache", name)
		}
		return nil, fmt.Errorf("failed to get persistentvolume %s from cache: %w", name, err)
	}
	return pv, nil
}

// GetSecret looks up a namespaced Secret by namespace and name.
func (c *Cache) GetSecret(namespace, name string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	err := c.client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, secret)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("secret %s/%s not found in cache", namespace, name)
		}
		return nil, fmt.Errorf("failed to get secret %s/%s from cache: %w", namespace, name, err)
	}
	return secret, nil
}

// GetConfigmap looks up a namespaced ConfigMap. Returns (nil, nil) when not found.
func (c *Cache) GetConfigmap(namespace, name string) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{}
	err := c.client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, cm)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get configmap %s/%s from cache: %v", namespace, name, err)
	}
	return cm, nil
}

// GetSandboxTemplate retrieves a SandboxTemplate by namespace and name.
func (c *Cache) GetSandboxTemplate(namespace, name string) (*agentsv1alpha1.SandboxTemplate, error) {
	tmpl := &agentsv1alpha1.SandboxTemplate{}
	err := c.client.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, tmpl)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("sandboxtemplate %s/%s not found in cache", namespace, name)
		}
		return nil, fmt.Errorf("failed to get sandboxtemplate %s/%s from cache: %w", namespace, name, err)
	}
	return tmpl, nil
}

// GetClaimedSandbox retrieves a sandbox by its logical sandbox ID.
func (c *Cache) GetClaimedSandbox(sandboxID string) (*agentsv1alpha1.Sandbox, error) {
	list := &agentsv1alpha1.SandboxList{}
	err := c.client.List(context.Background(), list, ctrlclient.MatchingFields{IndexClaimedSandboxID: sandboxID})
	if err != nil {
		return nil, err
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("sandbox %s not found in cache", sandboxID)
	}
	if len(list.Items) > 1 {
		return nil, fmt.Errorf("multiple sandboxes found with id %s", sandboxID)
	}
	managerutils.ResourceVersionExpectationObserve(&list.Items[0])
	return &list.Items[0], nil
}

// GetCheckpoint retrieves a Checkpoint by its logical checkpoint ID.
func (c *Cache) GetCheckpoint(checkpointID string) (*agentsv1alpha1.Checkpoint, error) {
	list := &agentsv1alpha1.CheckpointList{}
	err := c.client.List(context.Background(), list, ctrlclient.MatchingFields{IndexCheckpointID: checkpointID})
	if err != nil {
		return nil, err
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("checkpoint %s not found in cache", checkpointID)
	}
	if len(list.Items) > 1 {
		return nil, fmt.Errorf("multiple checkpoints found with id %s", checkpointID)
	}
	managerutils.ResourceVersionExpectationObserve(&list.Items[0])
	return &list.Items[0], nil
}

// GetSandboxSet retrieves a SandboxSet by name.
func (c *Cache) GetSandboxSet(name string) (*agentsv1alpha1.SandboxSet, error) {
	list := &agentsv1alpha1.SandboxSetList{}
	err := c.client.List(context.Background(), list, ctrlclient.MatchingFields{IndexTemplateID: name})
	if err != nil {
		return nil, fmt.Errorf("failed to get sandboxset %s from cache: %w", name, err)
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("sandboxset %s not found in cache", name)
	}
	managerutils.ResourceVersionExpectationObserve(&list.Items[0])
	return &list.Items[0], nil
}

// ListSandboxWithUser returns all sandboxes owned by the given user.
func (c *Cache) ListSandboxWithUser(user string) ([]*agentsv1alpha1.Sandbox, error) {
	list := &agentsv1alpha1.SandboxList{}
	err := c.client.List(context.Background(), list, ctrlclient.MatchingFields{IndexUser: user})
	if err != nil {
		return nil, err
	}
	result := make([]*agentsv1alpha1.Sandbox, 0, len(list.Items))
	for i := range list.Items {
		managerutils.ResourceVersionExpectationObserve(&list.Items[i])
		result = append(result, &list.Items[i])
	}
	return result, nil
}

// ListCheckpointsWithUser returns all checkpoints owned by the given user.
func (c *Cache) ListCheckpointsWithUser(user string) ([]*agentsv1alpha1.Checkpoint, error) {
	list := &agentsv1alpha1.CheckpointList{}
	err := c.client.List(context.Background(), list, ctrlclient.MatchingFields{IndexUser: user})
	if err != nil {
		return nil, err
	}
	result := make([]*agentsv1alpha1.Checkpoint, 0, len(list.Items))
	for i := range list.Items {
		result = append(result, &list.Items[i])
	}
	return result, nil
}

// ListSandboxesInPool returns all sandboxes in the pool for the given template.
func (c *Cache) ListSandboxesInPool(pool string) ([]*agentsv1alpha1.Sandbox, error) {
	resultVal, err, _ := c.listSandboxesGroup.Do(pool, func() (any, error) {
		list := &agentsv1alpha1.SandboxList{}
		if err := c.client.List(context.Background(), list, ctrlclient.MatchingFields{IndexSandboxPool: pool}); err != nil {
			return nil, err
		}
		result := make([]*agentsv1alpha1.Sandbox, 0, len(list.Items))
		for i := range list.Items {
			managerutils.ResourceVersionExpectationObserve(&list.Items[i])
			result = append(result, &list.Items[i])
		}
		return result, nil
	})
	if err != nil {
		return nil, err
	}
	return resultVal.([]*agentsv1alpha1.Sandbox), nil
}

// ListAllSandboxes returns all sandboxes in the cache.
func (c *Cache) ListAllSandboxes() []*agentsv1alpha1.Sandbox {
	list := &agentsv1alpha1.SandboxList{}
	if err := c.client.List(context.Background(), list); err != nil {
		return nil
	}
	result := make([]*agentsv1alpha1.Sandbox, 0, len(list.Items))
	for i := range list.Items {
		result = append(result, &list.Items[i])
	}
	return result
}

func (c *Cache) ListSandboxSets(namespace string) ([]*agentsv1alpha1.SandboxSet, error) {
	list := &agentsv1alpha1.SandboxSetList{}
	var opts []ctrlclient.ListOption
	if namespace != "" {
		opts = append(opts, ctrlclient.InNamespace(namespace))
	}
	if err := c.client.List(context.Background(), list, opts...); err != nil {
		return nil, err
	}
	result := make([]*agentsv1alpha1.SandboxSet, 0, len(list.Items))
	for i := range list.Items {
		result = append(result, &list.Items[i])
	}
	return result, nil
}

// WaitForSandboxSatisfied blocks until the sandbox satisfies the condition.
func (c *Cache) WaitForSandboxSatisfied(ctx context.Context, sbx *agentsv1alpha1.Sandbox, action cacheutils.WaitAction,
	satisfiedFunc cacheutils.CheckFunc[*agentsv1alpha1.Sandbox], timeout time.Duration) error {

	key := types.NamespacedName{Namespace: sbx.Namespace, Name: sbx.Name}
	sandboxID := sandboxutils.GetSandboxID(sbx)

	return cacheutils.WaitForObjectSatisfied[*agentsv1alpha1.Sandbox](ctx, c.waitHooks, sbx, action,
		func(_ *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
			got, err := c.GetClaimedSandbox(sandboxID)
			if err == nil {
				return got, nil
			}
			got = &agentsv1alpha1.Sandbox{}
			err = c.reader.Get(ctx, key, got)
			return got, err
		},
		satisfiedFunc, timeout)
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

// WaitForCheckpointSatisfied blocks until the checkpoint satisfies the condition.
func (c *Cache) WaitForCheckpointSatisfied(ctx context.Context, checkpoint *agentsv1alpha1.Checkpoint, action cacheutils.WaitAction,
	satisfiedFunc cacheutils.CheckFunc[*agentsv1alpha1.Checkpoint], timeout time.Duration) (*agentsv1alpha1.Checkpoint, error) {

	err := cacheutils.WaitForObjectSatisfied[*agentsv1alpha1.Checkpoint](ctx, c.waitHooks, checkpoint, action,
		func(cp *agentsv1alpha1.Checkpoint) (*agentsv1alpha1.Checkpoint, error) {
			return c.refreshCheckpoint(ctx, cp)
		},
		satisfiedFunc, timeout)
	if err != nil {
		return nil, err
	}
	return c.refreshCheckpoint(ctx, checkpoint)
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
