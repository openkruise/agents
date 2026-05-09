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

package sandboxcr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/time/rate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/pkg/utils"
	managerutils "github.com/openkruise/agents/pkg/utils/sandbox-manager/expectationutils"
	"github.com/openkruise/agents/pkg/utils/sandbox-manager/proxyutils"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
)

var DefaultDeleteSandboxTemplate = deleteSandboxTemplate

func deleteSandboxTemplate(ctx context.Context, c client.Client, namespace, name string) error {
	return c.Delete(ctx, &v1alpha1.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}})
}

var DefaultDeleteCheckpointCR = deleteCheckpointCR

func deleteCheckpointCR(ctx context.Context, c client.Client, namespace, name string) error {
	return c.Delete(ctx, &v1alpha1.Checkpoint{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}})
}

type InfraBuilder struct {
	instance            *Infra
	skipRouteReconciler bool
}

var _ infra.Builder = (*InfraBuilder)(nil)

func NewInfraBuilder(opts config.SandboxManagerOptions) *InfraBuilder {
	return &InfraBuilder{
		instance: &Infra{
			reconcileRouteStopCh: make(chan struct{}),
			claimLockChannel:     make(chan struct{}, opts.MaxClaimWorkers),
			createLimiter:        rate.NewLimiter(rate.Limit(opts.MaxCreateQPS), opts.MaxCreateQPS),
		},
		skipRouteReconciler: opts.DisableRouteReconciliation,
	}
}

func (b *InfraBuilder) WithCache(cache cache.Provider) *InfraBuilder {
	b.instance.Cache = cache
	return b
}

func (b *InfraBuilder) WithAPIReader(reader client.Reader) *InfraBuilder {
	b.instance.APIReader = reader
	return b
}

func (b *InfraBuilder) WithProxy(proxy *proxy.Server) *InfraBuilder {
	b.instance.Proxy = proxy
	return b
}

func (b *InfraBuilder) Build() infra.Infrastructure {
	i := b.instance
	if c, ok := i.Cache.(*cache.Cache); ok {
		c.GetSandboxController().AddReconcileHandlers(i.reconcileSandbox)
	}
	if !b.skipRouteReconciler {
		go i.startRouteReconciler(RouteReconcileInterval)
	}
	return i
}

type Infra struct {
	Cache     cache.Provider
	APIReader client.Reader
	Proxy     *proxy.Server

	// For claiming sandbox
	pickCache        sync.Map
	claimLockChannel chan struct{}
	createLimiter    *rate.Limiter

	reconcileRouteStopCh chan struct{}
}

func (i *Infra) Run(ctx context.Context) error {
	return i.Cache.Run(ctx)
}

func (i *Infra) Stop(ctx context.Context) {
	close(i.reconcileRouteStopCh)
	i.Cache.Stop(ctx)
}

func (i *Infra) ClaimSandbox(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, infra.ClaimMetrics, error) {
	log := klog.FromContext(ctx)
	metrics := infra.ClaimMetrics{}

	opts, err := ValidateAndInitClaimOptions(opts)
	if err != nil {
		log.Error(err, "invalid claim options")
		return nil, metrics, err
	}

	claimCtx, cancel := context.WithTimeout(ctx, opts.ClaimTimeout)
	defer cancel()

	// Start claiming sandbox
	log.V(consts.DebugLogLevel).Info("claim sandbox options", "options", opts)
	metrics.Retries = -1 // starts from 0
	var claimedSandbox infra.Sandbox
	err = retry.OnError(wait.Backoff{
		Steps:    int(opts.ClaimTimeout / RetryInterval),
		Duration: RetryInterval,
		Factor:   LockBackoffFactor,
		Jitter:   LockJitter,
	}, func(err error) bool {
		return errors.As(err, &retriableError{})
	}, func() error {
		metrics.Retries++
		log.Info("try to claim sandbox", "retries", metrics.Retries)
		claimed, tryMetrics, claimErr := TryClaimSandbox(claimCtx, opts, &i.pickCache, i.Cache, i.claimLockChannel, i.createLimiter)
		metrics.Total += tryMetrics.Total
		metrics.Wait += tryMetrics.Wait
		metrics.PickAndLock += tryMetrics.PickAndLock
		metrics.WaitReady += tryMetrics.WaitReady
		metrics.InitRuntime += tryMetrics.InitRuntime
		metrics.CSIMount += tryMetrics.CSIMount
		metrics.LockType = tryMetrics.LockType
		metrics.MergePickSandboxFailures(tryMetrics.PickSandboxFailures)
		if tryMetrics.LastError != nil {
			metrics.LastError = tryMetrics.LastError
		}
		if claimErr == nil {
			claimedSandbox = claimed
		} else {
			metrics.RetryCost += tryMetrics.Total
		}
		return claimErr
	})
	return claimedSandbox, metrics, buildClaimError(err, metrics.LastError, metrics.PickSandboxFailures)
}

func buildClaimError(err error, lastError error, failures []infra.PickSandboxFailure) error {
	if err == nil {
		return nil
	}
	base := fmt.Sprintf("%v, last error: %v", err, lastError)
	if len(failures) == 0 {
		return fmt.Errorf("%s", base)
	}
	raw, marshalErr := json.Marshal(failures)
	if marshalErr != nil {
		return fmt.Errorf("%s, pick sandbox failures marshal error: %v", base, marshalErr)
	}
	return fmt.Errorf("%s, pick sandbox failures: %s", base, string(raw))
}

func (i *Infra) CloneSandbox(ctx context.Context, opts infra.CloneSandboxOptions) (infra.Sandbox, infra.CloneMetrics, error) {
	log := klog.FromContext(ctx)
	opts, err := ValidateAndInitCloneOptions(opts)
	if err != nil {
		log.Error(err, "invalid clone options")
		return nil, infra.CloneMetrics{}, err
	}
	log.Info("clone options", "options", opts)
	opts.CreateLimiter = i.createLimiter
	sandbox, metrics, err := CloneSandbox(ctx, opts, i.Cache)
	if err != nil {
		log.Error(err, "failed to clone sandbox")
		return nil, metrics, err
	}
	log.Info("sandbox cloned", "sandbox", klog.KObj(sandbox))
	return sandbox, metrics, nil
}

func (i *Infra) DeleteCheckpoint(ctx context.Context, opts infra.DeleteCheckpointOptions) error {
	log := klog.FromContext(ctx).WithValues("checkpointID", opts.CheckpointID, "namespace", opts.Namespace)

	// Step 1: Find checkpoint and template
	tmpl, cp, _, err := findCheckpointAndTemplateById(ctx, infra.CloneSandboxOptions{
		Namespace: opts.Namespace, CheckPointID: opts.CheckpointID, SkipWaitCheckpoint: true,
	}, i.Cache, infra.CloneMetrics{})
	if err != nil {
		log.Error(err, "failed to find checkpoint and template")
		return managererrors.NewError(managererrors.ErrorNotFound, "%s", err.Error())
	}

	// Step 2: Verify ownership if Owner is specified
	if user := opts.User; user != "" && cp.GetAnnotations()[v1alpha1.AnnotationOwner] != user {
		return managererrors.NewError(managererrors.ErrorNotAllowed, "checkpoint %s is not owned by user %s", opts.CheckpointID, user)
	}

	// Step 3: Delete the SandboxTemplate
	log.Info("deleting sandbox template", "template", klog.KObj(tmpl))
	if err := DefaultDeleteSandboxTemplate(ctx, i.Cache.GetClient(), tmpl.Namespace, tmpl.Name); err != nil {
		log.Error(err, "failed to delete sandbox template")
		return managererrors.NewError(managererrors.ErrorInternal, "%s", err.Error())
	}

	// Step 4: Check if checkpoint has OwnerReference to the SandboxTemplate
	// If yes, Kubernetes garbage collection will handle deletion automatically
	// If no, explicitly delete the checkpoint
	if !metav1.IsControlledBy(cp, tmpl) {
		log.Info("checkpoint has no controller reference to template, deleting explicitly", "checkpoint", klog.KObj(cp))
		if err := DefaultDeleteCheckpointCR(ctx, i.Cache.GetClient(), cp.Namespace, cp.Name); err != nil {
			log.Error(err, "failed to delete checkpoint")
			return managererrors.NewError(managererrors.ErrorInternal, "%s", err.Error())
		}
	}

	log.Info("checkpoint deleted successfully")
	return nil
}

func (i *Infra) GetCache() cache.Provider {
	return i.Cache
}

func (i *Infra) HasTemplate(ctx context.Context, opts infra.HasTemplateOptions) bool {
	_, err := i.Cache.PickSandboxSet(ctx, cache.PickSandboxSetOptions{Namespace: opts.Namespace, Name: opts.Name})
	return err == nil
}

func (i *Infra) HasCheckpoint(ctx context.Context, opts infra.HasCheckpointOptions) bool {
	_, err := i.Cache.GetCheckpoint(ctx, cache.GetCheckpointOptions{Namespace: opts.Namespace, CheckpointID: opts.CheckpointID})
	return err == nil
}

func (i *Infra) SelectSandboxes(ctx context.Context, opts infra.SelectSandboxesOptions) ([]infra.Sandbox, error) {
	objects, err := i.Cache.ListSandboxes(ctx, cache.ListSandboxesOptions{
		Namespace: opts.Namespace,
		User:      opts.User,
	})
	if err != nil {
		return nil, err
	}
	return i.asSandboxes(objects), nil
}

func (i *Infra) asSandboxes(objects []*v1alpha1.Sandbox) []infra.Sandbox {
	var sandboxes = make([]infra.Sandbox, 0, len(objects))
	for _, obj := range objects {
		if !managerutils.ResourceVersionExpectationSatisfied(obj) {
			continue
		}
		sandboxes = append(sandboxes, AsSandbox(obj, i.Cache))
	}
	return sandboxes
}

func (i *Infra) SelectSucceededCheckpoints(ctx context.Context, opts infra.SelectSucceededCheckpointsOptions) ([]infra.CheckpointInfo, error) {
	checkpoints, err := i.Cache.ListCheckpoints(ctx, cache.ListCheckpointsOptions{
		Namespace: opts.Namespace,
		User:      opts.User,
	})
	if err != nil {
		return nil, err
	}
	return selectSucceededCheckpoints(checkpoints), nil
}

func selectSucceededCheckpoints(checkpoints []*v1alpha1.Checkpoint) []infra.CheckpointInfo {
	results := make([]infra.CheckpointInfo, 0, len(checkpoints))
	for _, checkpoint := range checkpoints {
		if checkpoint.Status.Phase != v1alpha1.CheckpointSucceeded {
			continue
		}
		// we assume the CheckpointId of a succeeded checkpoint is not empty
		results = append(results, AsCheckpointInfo(checkpoint))
	}
	return results
}

func (i *Infra) GetClaimedSandbox(ctx context.Context, opts infra.GetClaimedSandboxOptions) (infra.Sandbox, error) {
	var sandbox *v1alpha1.Sandbox
	err := retry.OnError(utils.CacheBackoff, utils.RetryIfContextNotCanceled(ctx), func() error {
		got, err := i.Cache.GetClaimedSandbox(ctx, cache.GetClaimedSandboxOptions{Namespace: opts.Namespace, SandboxID: opts.SandboxID})
		if err != nil {
			return err
		}
		sandbox = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !managerutils.ResourceVersionExpectationSatisfied(sandbox) {
		klog.FromContext(ctx).Info("resource version expectation not satisfied, will request APIServer directly")
		sbx := &v1alpha1.Sandbox{}
		err = i.APIReader.Get(ctx, client.ObjectKey{Namespace: sandbox.Namespace, Name: sandbox.Name}, sbx)
		if err != nil {
			return nil, err
		}
		sandbox = sbx
	}
	return AsSandbox(sandbox, i.Cache), nil
}

func (i *Infra) reconcileSandbox(ctx context.Context, sbx *v1alpha1.Sandbox, notFound bool) (ctrl.Result, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))

	if notFound {
		// Sandbox not found, clean up route
		sandboxID := stateutils.GetSandboxID(sbx)
		i.Proxy.DeleteRoute(sandboxID)
		log.Info("sandbox route deleted during reconciliation", "sandboxID", sandboxID)
		return ctrl.Result{}, nil
	}

	// Sandbox exists, refresh route
	i.refreshRoute(sbx)
	log.V(consts.DebugLogLevel).Info("sandbox route refreshed during reconciliation")
	return ctrl.Result{}, nil
}

func (i *Infra) refreshRoute(sbx *v1alpha1.Sandbox) {
	oldRoute, exists := i.Proxy.LoadRoute(sbx.GetName())
	newRoute := proxyutils.DefaultGetRouteFunc(sbx)
	if !exists || newRoute.State != oldRoute.State || newRoute.IP != oldRoute.IP {
		i.Proxy.SetRoute(logs.NewContext(), newRoute)
	}
}

const (
	// RouteReconcileInterval is the interval for route reconciliation
	RouteReconcileInterval = 5 * time.Minute
)

// startRouteReconciler periodically reconciles routes to clean up orphaned entries
// that might be left due to missed delete events from Kubernetes informer.
// It also runs reconcileRoutes immediately on startup to ensure all routes are synced.
func (i *Infra) startRouteReconciler(interval time.Duration) {
	// Run immediately on startup to ensure routes are synced
	ctx := logs.NewContext("action", "reconcileRoutes")
	i.reconcileRoutes(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			i.reconcileRoutes(ctx)
		case <-i.reconcileRouteStopCh:
			klog.Info("route reconciler stopped")
			return
		}
	}
}

// reconcileRoutes compares routes in Proxy with Sandboxes in Cache
// and deletes orphaned routes that no longer have corresponding Sandboxes.
// It also adds missing routes for existing sandboxes that don't have a route yet.
func (i *Infra) reconcileRoutes(ctx context.Context) {
	log := klog.FromContext(ctx)
	log.Info("starting route reconciliation")
	// Build set of existing sandbox IDs from cache
	existingSandboxIDs := make(map[string]struct{})

	sandboxList := &v1alpha1.SandboxList{}
	if err := i.Cache.GetClient().List(ctx, sandboxList); err != nil {
		log.Error(err, "failed to list sandboxes from cache")
		return
	}
	for idx := range sandboxList.Items {
		sandboxID := stateutils.GetSandboxID(&sandboxList.Items[idx])
		existingSandboxIDs[sandboxID] = struct{}{}
	}

	// Check all routes and delete orphaned ones
	routes := i.Proxy.ListRoutes()
	deletedCount := 0
	for _, route := range routes {
		if _, exists := existingSandboxIDs[route.ID]; !exists {
			i.Proxy.DeleteRoute(route.ID)
			deletedCount++
			managerutils.ResourceVersionExpectationDelete(&metav1.ObjectMeta{
				UID: route.UID,
			})
			log.Info("reconciler deleted orphaned route", "sandboxID", route.ID)
		}
	}

	// Add missing routes for sandboxes that don't have a route yet
	addedCount := 0
	for idx := range sandboxList.Items {
		sbx := &sandboxList.Items[idx]
		sandboxID := stateutils.GetSandboxID(sbx)
		if _, hasRoute := i.Proxy.LoadRoute(sandboxID); !hasRoute {
			route := proxyutils.DefaultGetRouteFunc(sbx)
			i.Proxy.SetRoute(ctx, route)
			addedCount++
			log.Info("reconciler added missing route", "sandboxID", sandboxID, "route", route)
		}
	}

	if deletedCount > 0 || addedCount > 0 {
		log.Info("route reconciliation completed", "orphanedRoutesDeleted", deletedCount, "missingRoutesAdded", addedCount, "totalRoutes", len(routes)+addedCount-deletedCount)
	}
}
