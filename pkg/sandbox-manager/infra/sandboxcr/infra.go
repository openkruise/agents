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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"github.com/openkruise/agents/pkg/utils/proxyutils"
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

// createRetryBackoff returns the bounded, context-aware exponential backoff
// shared by the ClaimSandbox and CloneSandbox create-retry loops. The step
// count caps how many sandbox creations a single operation may trigger.
func createRetryBackoff() wait.Backoff {
	return wait.Backoff{
		Steps:    CreateMaxRetrySteps,
		Duration: CreateRetryInterval,
		Factor:   CreateRetryBackoffFactor,
		Jitter:   CreateRetryJitter,
		Cap:      CreateRetryIntervalCap,
	}
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
	log.V(utils.DebugLogLevel).Info("claim sandbox options", "options", opts)
	metrics.Retries = -1 // starts from 0
	var claimedSandbox infra.Sandbox
	// The create retry budget is intentionally bounded (CreateMaxRetrySteps) to
	// cap how many sandbox creations a single claim may trigger. The backoff is
	// context-aware: a sleep between attempts is interrupted as soon as claimCtx
	// is cancelled or reaches ClaimTimeout.
	var lastErr error
	waitErr := wait.ExponentialBackoffWithContext(claimCtx, createRetryBackoff(), func(context.Context) (bool, error) {
		metrics.Retries++
		log.Info("try to claim sandbox", "retries", metrics.Retries)
		claimed, tryMetrics, claimErr := TryClaimSandbox(claimCtx, opts, &i.pickCache, i.Cache, i.claimLockChannel, i.createLimiter)
		metrics.Total += tryMetrics.Total
		metrics.Wait += tryMetrics.Wait
		metrics.PickAndLock += tryMetrics.PickAndLock
		metrics.WaitReady += tryMetrics.WaitReady
		metrics.InitRuntime += tryMetrics.InitRuntime
		metrics.SecurityToken += tryMetrics.SecurityToken
		metrics.TrafficToken += tryMetrics.TrafficToken
		metrics.CSIMount += tryMetrics.CSIMount
		metrics.LockType = tryMetrics.LockType
		metrics.MergePickSandboxFailures(tryMetrics.PickSandboxFailures)
		if tryMetrics.LastError != nil {
			metrics.LastError = tryMetrics.LastError
		}
		if claimErr == nil {
			claimedSandbox = claimed
			return true, nil
		}
		metrics.RetryCost += tryMetrics.Total
		lastErr = claimErr
		if errors.As(claimErr, &retriableError{}) {
			return false, nil
		}
		// Terminal error (e.g. classified BadRequest/Internal): stop retrying so
		// buildClaimError can preserve its ErrorCode for HTTP status mapping.
		return false, claimErr
	})
	// When the retry budget is exhausted (steps used up while claimCtx is still
	// live), surface the last retriable attempt error instead of the generic
	// wait sentinel. On context cancellation/timeout keep the context error.
	finalErr := waitErr
	if waitErr != nil && claimCtx.Err() == nil && lastErr != nil {
		finalErr = lastErr
	}
	return claimedSandbox, metrics, buildClaimError(finalErr, metrics.LastError, metrics.PickSandboxFailures)
}

func buildClaimError(err error, lastError error, failures []infra.PickSandboxFailure) error {
	if err == nil {
		return nil
	}
	// Preserve terminal error type (ErrorBadRequest / ErrorInternal) from the classifier.
	// These errors were not retried and carry the correct ErrorCode for HTTP status mapping.
	var mErr *managererrors.Error
	if errors.As(err, &mErr) {
		return mErr
	}
	// Retry exhausted or interrupted: wrap as Internal
	base := fmt.Sprintf("%v, last error: %v", err, lastError)
	if len(failures) > 0 {
		raw, marshalErr := json.Marshal(failures)
		if marshalErr == nil {
			base = fmt.Sprintf("%s, pick sandbox failures: %s", base, string(raw))
		}
	}
	return managererrors.NewError(managererrors.ErrorInternal, "%s", base)
}

func (i *Infra) CloneSandbox(ctx context.Context, opts infra.CloneSandboxOptions) (infra.Sandbox, infra.CloneMetrics, error) {
	log := klog.FromContext(ctx)
	metrics := infra.CloneMetrics{}
	opts, err := ValidateAndInitCloneOptions(opts)
	if err != nil {
		log.Error(err, "invalid clone options")
		return nil, metrics, err
	}
	log.V(utils.DebugLogLevel).Info("clone sandbox options", "options", opts)

	cloneCtx, cancel := context.WithTimeout(ctx, opts.CloneTimeout)
	defer cancel()

	// Inject Infra-level limiter so the inner CloneSandbox enforces rate
	// limiting at the same gate as ClaimSandbox (see newSandboxFromSandboxSet).
	attemptOpts := opts
	attemptOpts.CreateLimiter = i.createLimiter

	metrics.Retries = -1 // starts from 0
	var clonedSandbox infra.Sandbox
	// Bounded, context-aware create retry (see ClaimSandbox for the rationale).
	var lastErr error
	waitErr := wait.ExponentialBackoffWithContext(cloneCtx, createRetryBackoff(), func(context.Context) (bool, error) {
		metrics.Retries++
		cloned, tryMetrics, cloneErr := CloneSandbox(cloneCtx, attemptOpts, i.Cache)
		metrics.Merge(tryMetrics)
		if cloneErr == nil {
			clonedSandbox = cloned
			return true, nil
		}
		metrics.LastError = cloneErr
		lastErr = cloneErr
		if errors.As(cloneErr, &retriableError{}) {
			return false, nil
		}
		// Terminal error: stop retrying and preserve its ErrorCode.
		return false, cloneErr
	})
	if waitErr != nil {
		// On retry-budget exhaustion (cloneCtx still live) surface the last
		// attempt error; on context cancellation/timeout keep the context error.
		finalErr := waitErr
		if cloneCtx.Err() == nil && lastErr != nil {
			finalErr = lastErr
		}
		log.Error(finalErr, "failed to clone sandbox", "metrics", metrics.String())
		return nil, metrics, buildCloneError(finalErr)
	}
	log.Info("sandbox cloned", "sandbox", klog.KObj(clonedSandbox), "metrics", metrics.String())
	return clonedSandbox, metrics, nil
}

// buildCloneError keeps an already-classified manager error (so its
// ErrorCode reaches the HTTP layer) and wraps any other error as ErrorInternal.
func buildCloneError(err error) error {
	if err == nil {
		return nil
	}
	var mErr *managererrors.Error
	if errors.As(err, &mErr) {
		return mErr
	}
	if apierrors.IsAlreadyExists(err) {
		return managererrors.NewError(managererrors.ErrorConflict, "%v", err)
	}
	return managererrors.NewError(managererrors.ErrorInternal, "%v", err)
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

	// Step 3: Delete the Checkpoint. For new-shape data (SandboxTemplate owned
	// by Checkpoint), Kubernetes garbage collection cascades the
	// SandboxTemplate after the agents.kruise.io/checkpoint finalizer is
	// processed.
	log.Info("deleting checkpoint", "checkpoint", klog.KObj(cp))
	if err := client.IgnoreNotFound(DefaultDeleteCheckpointCR(ctx, i.Cache.GetClient(), cp.Namespace, cp.Name)); err != nil {
		log.Error(err, "failed to delete checkpoint")
		return managererrors.NewError(managererrors.ErrorInternal, "%s", err.Error())
	}

	// Step 4: For legacy-shape data (Checkpoint owned by SandboxTemplate, with
	// no owner reference on the SandboxTemplate itself), GC will not reach the
	// SandboxTemplate. Delete it explicitly.
	if !metav1.IsControlledBy(tmpl, cp) {
		log.Info("template not controlled by checkpoint, deleting explicitly", "template", klog.KObj(tmpl))
		if err := client.IgnoreNotFound(DefaultDeleteSandboxTemplate(ctx, i.Cache.GetClient(), tmpl.Namespace, tmpl.Name)); err != nil {
			log.Error(err, "failed to delete sandbox template")
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
		if !expectations.ResourceVersionExpectationSatisfied(obj) {
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

type claimedSandboxLookup struct {
	sandbox  *v1alpha1.Sandbox
	route    proxy.Route
	hasRoute bool
}

// lookupSandbox waits until the informer cache returns the claimed Sandbox.
// Route state is loaded only after a cache hit and is used later as a staleness
// signal, not as a cache-miss fallback trigger.
func (i *Infra) lookupSandbox(ctx context.Context, opts infra.GetSandboxOptions) (claimedSandboxLookup, error) {
	var lookup claimedSandboxLookup
	err := wait.PollUntilContextCancel(ctx, RetryInterval, true, func(ctx context.Context) (bool, error) {
		got, err := i.Cache.GetClaimedSandbox(ctx, cache.GetClaimedSandboxOptions{Namespace: opts.Namespace, SandboxID: opts.SandboxID})
		if err == nil {
			lookup.sandbox = got
			if route, ok := i.Proxy.LoadRoute(opts.SandboxID); ok {
				lookup.route = route
				lookup.hasRoute = true
			}
			return true, nil
		}
		if errors.Is(err, cache.ErrSandboxNotFound) {
			return false, nil
		}
		if isContextError(err) {
			return false, ctx.Err()
		}
		return false, err
	})
	if err != nil {
		return lookup, err
	}
	return lookup, nil
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// isSandboxStale reports whether the cache-hit Sandbox should be refreshed via
// the APIReader fallback. It is only called after lookupClaimedSandbox returns
// a cache-hit Sandbox, and emits fallback metrics and debug logging internally
// when it returns true.
func isSandboxStale(ctx context.Context, lookup claimedSandboxLookup) bool {
	cacheRV := lookup.sandbox.GetResourceVersion()
	var reason string
	switch {
	case !expectations.ResourceVersionExpectationSatisfied(lookup.sandbox):
		reason = fallbackReasonRVExpectation
	case lookup.hasRoute &&
		lookup.route.ResourceVersion != "" &&
		expectations.IsResourceVersionReallyNewer(cacheRV, lookup.route.ResourceVersion):
		reason = fallbackReasonCacheLagging
	default:
	}
	if reason != "" {
		klog.FromContext(ctx).V(utils.DebugLogLevel).Info("informer cache result requires APIReader fallback",
			"sandbox", klog.KObj(lookup.sandbox), "reason", reason,
			"routeRV", lookup.route.ResourceVersion, "cacheRV", cacheRV)
		sandboxFallbackTotal.WithLabelValues(lookup.sandbox.Namespace, reason).Inc()
		return true
	}
	return false
}

func (i *Infra) getSandboxFromAPIReader(ctx context.Context, key client.ObjectKey, sandboxID string) (*v1alpha1.Sandbox, error) {
	fresh := &v1alpha1.Sandbox{}
	if err := i.APIReader.Get(ctx, key, fresh); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s", cache.ErrSandboxNotFound, sandboxID)
		}
		return nil, err
	}
	if fresh.GetLabels()[v1alpha1.LabelSandboxIsClaimed] != v1alpha1.True {
		return nil, fmt.Errorf("%w: %s", cache.ErrSandboxNotFound, sandboxID)
	}
	return fresh, nil
}

func (i *Infra) GetSandbox(ctx context.Context, opts infra.GetSandboxOptions) (infra.Sandbox, error) {
	lookup, err := i.lookupSandbox(ctx, opts)
	if err != nil {
		return nil, err
	}

	if !isSandboxStale(ctx, lookup) {
		return AsSandbox(lookup.sandbox, i.Cache), nil
	}

	key := client.ObjectKey{Namespace: lookup.sandbox.Namespace, Name: lookup.sandbox.Name}
	fresh, err := i.getSandboxFromAPIReader(ctx, key, opts.SandboxID)
	if err != nil {
		return nil, err
	}
	return AsSandbox(fresh, i.Cache), nil
}

func (i *Infra) reconcileSandbox(ctx context.Context, sbx *v1alpha1.Sandbox, notFound bool) (ctrl.Result, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))

	if notFound {
		// Sandbox not found, clean up route
		sandboxID := utils.GetSandboxID(sbx)
		i.Proxy.DeleteRoute(sandboxID)
		log.Info("sandbox route deleted during reconciliation", "sandboxID", sandboxID)
		return ctrl.Result{}, nil
	}

	// Sandbox exists, refresh route
	if i.refreshRoute(sbx) {
		log.V(utils.DebugLogLevel).Info("sandbox route refreshed during reconciliation")
	}
	return ctrl.Result{}, nil
}

func (i *Infra) refreshRoute(sbx *v1alpha1.Sandbox) bool {
	oldRoute, exists := i.Proxy.LoadRoute(utils.GetSandboxID(sbx))
	newRoute := proxyutils.DefaultGetRouteFunc(sbx)
	if !exists || newRoute.State != oldRoute.State || newRoute.IP != oldRoute.IP {
		i.Proxy.SetRoute(logs.NewContext(), newRoute)
		return true
	}
	return false
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
		sandboxID := utils.GetSandboxID(&sandboxList.Items[idx])
		existingSandboxIDs[sandboxID] = struct{}{}
	}

	// Check all routes and delete orphaned ones
	routes := i.Proxy.ListRoutes()
	deletedCount := 0
	for _, route := range routes {
		if _, exists := existingSandboxIDs[route.ID]; !exists {
			i.Proxy.DeleteRoute(route.ID)
			deletedCount++
			expectations.ResourceVersionExpectationDelete(&metav1.ObjectMeta{
				UID: route.UID,
			})
			log.Info("reconciler deleted orphaned route", "sandboxID", route.ID)
		}
	}

	// Add missing routes for sandboxes that don't have a route yet
	addedCount := 0
	for idx := range sandboxList.Items {
		sbx := &sandboxList.Items[idx]
		sandboxID := utils.GetSandboxID(sbx)
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
