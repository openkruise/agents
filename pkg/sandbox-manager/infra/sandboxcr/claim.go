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
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/controller/sandboxset"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"github.com/openkruise/agents/pkg/utils/runtime"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/pkg/utils/sandbox-manager/expectationutils"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
)

var DefaultCleanupTimeout = 30 * time.Second

func ValidateAndInitClaimOptions(opts infra.ClaimSandboxOptions) (infra.ClaimSandboxOptions, error) {
	if opts.User == "" {
		return infra.ClaimSandboxOptions{}, fmt.Errorf("user is required")
	}
	if opts.Template == "" {
		return infra.ClaimSandboxOptions{}, fmt.Errorf("template is required")
	}
	if opts.CSIMount != nil {
		// for csi mount, init runtime is required
		if opts.InitRuntime == nil {
			return infra.ClaimSandboxOptions{}, fmt.Errorf("init runtime is required when csi mount is specified")
		}
	}
	if opts.InplaceUpdate != nil && opts.InplaceUpdate.Image == "" && opts.InplaceUpdate.Resources == nil {
		return infra.ClaimSandboxOptions{}, fmt.Errorf("inplace update requires either image or resources to be set")
	}
	if opts.InplaceUpdate != nil && opts.InplaceUpdate.Resources != nil {
		res := opts.InplaceUpdate.Resources
		if len(res.Requests) == 0 && len(res.Limits) == 0 {
			return infra.ClaimSandboxOptions{}, fmt.Errorf("resources must specify at least one of requests or limits")
		}
		for _, rl := range []corev1.ResourceList{res.Requests, res.Limits} {
			if cpu, ok := rl[corev1.ResourceCPU]; ok {
				if cpu.IsZero() || cpu.Cmp(resource.Quantity{}) < 0 {
					return infra.ClaimSandboxOptions{}, fmt.Errorf("target cpu must be a positive value")
				}
			}
		}
	}
	if opts.CandidateCounts <= 0 {
		opts.CandidateCounts = consts.DefaultPoolingCandidateCounts
	}
	if opts.LockString == "" {
		opts.LockString = utils.NewLockString()
	}
	if opts.ClaimTimeout <= 0 {
		opts.ClaimTimeout = DefaultClaimTimeout
	}
	if opts.WaitReadyTimeout <= 0 {
		opts.WaitReadyTimeout = consts.DefaultWaitReadyTimeout
	}
	return opts, nil
}

// TryClaimSandbox attempts to claim a sandbox based on the provided Options.
// The returned sandbox is valid only when nil error is returned. Once a non-nil sandbox is returned,
// the sandbox object should not be used anymore and needs appropriate handling.
//
// ValidateAndInitClaimOptions must be called before this function.
func TryClaimSandbox(ctx context.Context, opts infra.ClaimSandboxOptions, pickCache *sync.Map, cache cache.Provider,
	claimLockChannel chan struct{}, createLimiter *rate.Limiter) (claimed infra.Sandbox, metrics infra.ClaimMetrics, err error) {
	ctx = logs.Extend(ctx, "tryClaimId", uuid.NewString()[:8])
	log := klog.FromContext(ctx)

	select {
	case <-ctx.Done():
		err = fmt.Errorf("context canceled while retrying: %v", ctx.Err())
		log.Error(ctx.Err(), "context canceled while retrying")
		return
	default:
	}

	log.Info("waiting for a free claim worker")
	startWaiting := time.Now()
	freeWorkerOnce := sync.OnceFunc(func() {
		<-claimLockChannel // free the worker
	})
	select {
	case <-ctx.Done():
		err = fmt.Errorf("context canceled before getting a free claim worker: %v", ctx.Err())
		log.Error(ctx.Err(), "failed to get a free claim worker")
		return
	case claimLockChannel <- struct{}{}:
		metrics.Wait = time.Since(startWaiting)
		log.Info("got a free claim worker", "cost", metrics.Wait)
	}
	defer func() {
		freeWorkerOnce()
		metrics.LastError = err
		log.Info("try claim sandbox result", "metrics", metrics.String())
		clearFailedSandbox(ctx, claimed, err, opts.ReserveFailedSandbox)
	}()
	// Step 1: Pick an available sandbox
	var sbx *Sandbox
	var lockType infra.LockType
	pickStart := time.Now()
	sbx, lockType, err = pickAnAvailableSandbox(ctx, opts, pickCache, cache, createLimiter)
	if err != nil {
		log.Error(err, "failed to select available sandbox")
		return
	}
	// Clean up pickCache based on lockType:
	// - LockTypeUpdate/LockTypeSpeculate: delete from pickCache (picked from pool)
	// - LockTypeCreate: no deletion needed (newly created, not in pickCache)
	defer func() {
		if sbx != nil && sbx.Sandbox != nil && (lockType == infra.LockTypeUpdate || lockType == infra.LockTypeSpeculate) {
			pickCache.Delete(getPickKey(sbx.Sandbox))
		}
	}()
	log.Info("sandbox picked", "sandbox", klog.KObj(sbx.Sandbox), "lockType", lockType)

	// Step 2: Modify and lock sandbox. All modifications to be applied to the Sandbox should be performed here.
	if err = modifyPickedSandbox(sbx, lockType, opts); err != nil {
		log.Error(err, "failed to modify picked sandbox")
		err = retriableError{Message: fmt.Sprintf("failed to modify picked sandbox: %s", err)}
		return
	}

	err = performLockSandbox(ctx, sbx, lockType, opts, cache)
	if err != nil {
		log.Error(err, "failed to lock sandbox")
		if apierrors.IsConflict(err) {
			expectationutils.ResourceVersionExpectationExpect(&metav1.ObjectMeta{
				UID:             sbx.GetUID(),
				ResourceVersion: expectations.GetNewerResourceVersion(sbx),
			})
			err = retriableError{Message: fmt.Sprintf("failed to lock sandbox: %s", err)}
		}
		return
	}
	metrics.LockType = lockType
	metrics.PickAndLock = time.Since(pickStart)
	metrics.Total += metrics.PickAndLock
	expectationutils.ResourceVersionExpectationExpect(sbx)
	log = log.WithValues("sandbox", klog.KObj(sbx.Sandbox))
	log.Info("sandbox locked", "cost", metrics.PickAndLock, "type", metrics.LockType)
	claimed = sbx
	freeWorkerOnce() // free worker early

	// Step 3: Built-in post processes. The locked sandbox must be always returned to be cleared properly.
	if lockType == infra.LockTypeCreate || lockType == infra.LockTypeSpeculate || opts.InplaceUpdate != nil {
		log.Info("should wait for sandbox ready", "inplaceUpdate", opts.InplaceUpdate != nil)
		metrics.WaitReady, err = waitForSandboxReady(ctx, sbx, opts, cache)
		metrics.Total += metrics.WaitReady
		if err != nil {
			log.Error(err, "failed to wait for sandbox ready", "cost", metrics.WaitReady)
			err = retriableError{Message: fmt.Sprintf("failed to wait for sandbox ready: %s", err)}
			return
		}
		log.Info("sandbox is ready", "cost", metrics.WaitReady)
	}

	if opts.InitRuntime != nil {
		log.Info("starting to init runtime", "opts", opts.InitRuntime)
		metrics.InitRuntime, err = runtime.InitRuntime(ctx, sbx.Sandbox, *opts.InitRuntime, sbx.refreshFunc())
		if err != nil {
			log.Error(err, "failed to init runtime")
			err = retriableError{Message: fmt.Sprintf("failed to init runtime: %s", err)}
			return
		}
		metrics.Total += metrics.InitRuntime
		log.Info("runtime inited", "cost", metrics.InitRuntime)
	}

	if opts.CSIMount != nil {
		log.Info("starting to perform csi mount")
		metrics.CSIMount, err = runtime.ProcessCSIMounts(ctx, sbx.Sandbox, *opts.CSIMount)
		if err != nil {
			log.Error(err, "failed to perform csi mount")
			err = fmt.Errorf("failed to perform csi mount: %s", err)
			return
		}
		metrics.Total += metrics.CSIMount
		log.Info("csi mount completed", "cost", metrics.CSIMount)
	}

	return
}

func clearFailedSandbox(ctx context.Context, sbx infra.Sandbox, err error, reserve bool) {
	if err == nil || sbx == nil {
		return // success or no need to clear
	}
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
	if reserve {
		log.Info("the locked sandbox is reserved for debugging")
	} else {
		log.Info("the locked sandbox will be deleted", "reason", err)
		// Use a new context with timeout to avoid indefinite blocking
		cleanupCtx, cancel := context.WithTimeout(context.Background(), DefaultCleanupTimeout)
		defer cancel()
		if err := sbx.Kill(cleanupCtx); err != nil {
			log.Error(err, "failed to delete locked sandbox")
		} else {
			log.Info("sandbox deleted")
		}
	}
}

func getPickKey(sbx *v1alpha1.Sandbox) string {
	return client.ObjectKeyFromObject(sbx).String()
}

func pickAnAvailableSandbox(ctx context.Context, opts infra.ClaimSandboxOptions, pickCache *sync.Map, cache cache.Provider, limiter *rate.Limiter) (*Sandbox, infra.LockType, error) {
	template, cnt := opts.Template, opts.CandidateCounts
	ctx = logs.Extend(ctx, "action", "pickAnAvailableSandbox")
	log := klog.FromContext(ctx).WithValues("template", template).V(consts.DebugLogLevel)
	objects, err := cache.ListSandboxesInPool(ctx, template)
	if err != nil {
		return nil, "", err
	}
	if len(objects) == 0 {
		if opts.CreateOnNoStock {
			log.Info("will create a new sandbox", "reason", "NoStock")
			return newSandboxFromSandboxSet(ctx, opts, cache, limiter)
		}
		return nil, "", NoAvailableError(template, "no stock")
	}

	// Get the SandboxSet's current update revision to prefer matching sandboxes.
	var updateRevision string
	if sbs, sErr := cache.PickSandboxSet(ctx, template); sErr == nil && sbs != nil {
		updateRevision = sbs.Status.UpdateRevision
	}

	// Select available candidates and speculated creating sandboxes
	availableCandidates := make([]*v1alpha1.Sandbox, 0, cnt)
	speculatingCandidates := make([]*v1alpha1.Sandbox, 0, cnt)
	for _, obj := range objects {
		if len(availableCandidates) >= cnt {
			if opts.SpeculateCreatingDuration == 0 || len(speculatingCandidates) >= cnt {
				break
			}
		}
		if !expectationutils.ResourceVersionExpectationSatisfied(obj) {
			log.Info("skip out-dated sandbox cache", "sandbox", klog.KObj(obj))
			continue
		}
		if checkErr := preCheckCandidate(obj); checkErr != nil {
			log.Error(checkErr, "skip invalid sandbox", "sandbox", klog.KObj(obj), "resourceVersion", obj.GetResourceVersion())
			continue
		}
		state, _ := stateutils.GetSandboxState(obj)
		switch state {
		case v1alpha1.SandboxStateAvailable:
			if len(availableCandidates) >= cnt {
				continue
			}
			if obj.Status.PodInfo.PodIP == "" {
				log.Info("skip available sandbox without podIP", "sandbox", klog.KObj(obj))
				continue
			}
			availableCandidates = append(availableCandidates, obj)
		case v1alpha1.SandboxStateCreating:
			if opts.SpeculateCreatingDuration == 0 || len(speculatingCandidates) >= cnt {
				continue
			}
			creationDuration := time.Since(obj.CreationTimestamp.Time)
			if creationDuration >= opts.SpeculateCreatingDuration {
				speculatingCandidates = append(speculatingCandidates, obj)
			}
		}
	}
	log.Info("candidates collected", "available", len(availableCandidates), "speculating", len(speculatingCandidates))

	// Split available candidates into updated (current revision) and old groups.
	// Try updated candidates first to reduce conflicts with SandboxSet rolling update
	// which targets old-version sandboxes.
	var updatedCandidates, oldCandidates []*v1alpha1.Sandbox
	if updateRevision != "" {
		updatedCandidates = make([]*v1alpha1.Sandbox, 0, len(availableCandidates))
		oldCandidates = make([]*v1alpha1.Sandbox, 0, len(availableCandidates))
		for _, c := range availableCandidates {
			if c.Labels[v1alpha1.LabelTemplateHash] == updateRevision {
				updatedCandidates = append(updatedCandidates, c)
			} else {
				oldCandidates = append(oldCandidates, c)
			}
		}
	} else {
		updatedCandidates = availableCandidates
	}

	// Step 1: try to pick from updated (newest version) candidates first
	log.Info("picking from available candidates", "updated", len(updatedCandidates), "old", len(oldCandidates))
	sbx, pickErr := pickFromCandidates(ctx, updatedCandidates, pickCache)
	if pickErr != nil && len(oldCandidates) > 0 {
		// fall back to old candidates
		log.Info("falling back to old available candidates")
		sbx, pickErr = pickFromCandidates(ctx, oldCandidates, pickCache)
	}
	if pickErr == nil {
		return AsSandbox(sbx, cache), infra.LockTypeUpdate, nil
	}
	log.Error(pickErr, "failed to pick from available candidates")

	// Step 2: select from speculated candidates
	if opts.SpeculateCreatingDuration > 0 {
		log.Info("picking from speculated candidates")
		sbx, pickErr = pickFromCandidates(ctx, speculatingCandidates, pickCache)
		if pickErr == nil {
			log.Info("will speculate creating sandbox", "sandbox", klog.KObj(sbx))
			return AsSandbox(sbx, cache), infra.LockTypeSpeculate, nil
		}
	}

	// Step 3: create new sandbox
	if opts.CreateOnNoStock {
		log.Info("will create a new sandbox")
		return newSandboxFromSandboxSet(ctx, opts, cache, limiter)
	}
	return nil, "", NoAvailableError(template, pickErr.Error())
}

func pickFromCandidates(ctx context.Context, candidates []*v1alpha1.Sandbox, pickCache *sync.Map) (*v1alpha1.Sandbox, error) {
	log := klog.FromContext(ctx).V(consts.DebugLogLevel)
	// Step 1: select from candidate
	if len(candidates) == 0 {
		return nil, errors.New("no candidate")
	}
	start := rand.IntN(len(candidates))
	i := start
	for {
		// Check if context is canceled
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context canceled while picking sandbox: %w", ctx.Err())
		default:
		}

		obj := candidates[i]
		key := getPickKey(obj)
		if _, loaded := pickCache.LoadOrStore(key, struct{}{}); !loaded {
			// The flow of the first-level lock introduced by pickCache is:
			// Acquire pickCache -> Attempt second-level optimistic lock via k8s update api -> Release pickCache
			// This ensures that for the same object, acquiring pickCache must happen after another request completes
			// the expectation, and this check guarantees that the same object will not be selected
			if !expectationutils.ResourceVersionExpectationSatisfied(obj) {
				log.Info("expectation of picked candidate is out-of-date", "key", key)
				pickCache.Delete(key)
			} else {
				log.Info("candidate picked", "sandbox", klog.KObj(obj))
				return obj, nil
			}
		} else {
			log.Info("candidate picked by another request", "key", key)
		}
		i = (i + 1) % len(candidates)
		if i == start {
			break
		}
	}
	return nil, errors.New("all candidates are picked")
}

var FilteredAnnotationsOnCreation []string

func newSandboxFromSandboxSet(ctx context.Context, opts infra.ClaimSandboxOptions, cache cache.Provider, limiter *rate.Limiter) (*Sandbox, infra.LockType, error) {
	if limiter != nil {
		if !limiter.Allow() {
			return nil, "", NoAvailableError(opts.Template, "sandbox creation is not allowed by rate limiter")
		}
	}
	sbs, err := cache.PickSandboxSet(ctx, opts.Template)
	if err != nil {
		return nil, "", NoAvailableError(opts.Template, "cannot create new sandbox: "+err.Error())
	}
	sbx := sandboxset.NewSandboxFromSandboxSet(sbs)
	// sandbox manager creates high-priority sandbox
	sbx.Annotations[v1alpha1.SandboxAnnotationPriority] = "100"
	for _, anno := range FilteredAnnotationsOnCreation {
		delete(sbx.Annotations, anno)
	}
	return AsSandbox(sbx, cache), infra.LockTypeCreate, nil
}

func preCheckCandidate(sbx *v1alpha1.Sandbox) error {
	lock := sbx.Annotations[v1alpha1.AnnotationLock]
	if lock != "" {
		return fmt.Errorf("sandbox is locked by %s", lock)
	}
	if sbx.CreationTimestamp.IsZero() {
		return errors.New("creation timestamp is zero")
	}
	return nil
}

func modifyPickedSandbox(sbx *Sandbox, lockType infra.LockType, opts infra.ClaimSandboxOptions) error {
	if lockType != infra.LockTypeCreate {
		sbx.Sandbox = sbx.Sandbox.DeepCopy()
	}
	if opts.Modifier != nil {
		opts.Modifier(sbx)
	}
	if opts.InplaceUpdate != nil {
		if opts.InplaceUpdate.Image != "" {
			sbx.SetImage(opts.InplaceUpdate.Image)
		}
		if opts.InplaceUpdate.Resources != nil {
			sbx.SetResources(opts.InplaceUpdate.Resources.Requests, opts.InplaceUpdate.Resources.Limits)
		}
	}
	// claim sandbox
	sbx.SetOwnerReferences([]metav1.OwnerReference{}) // make SandboxSet scale up
	labels := sbx.GetLabels()
	if labels == nil {
		labels = make(map[string]string, 1)
	}
	labels[v1alpha1.LabelSandboxIsClaimed] = v1alpha1.True
	sbx.SetLabels(labels)

	annotations := sbx.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string, 1)
	}
	annotations[v1alpha1.AnnotationClaimTime] = time.Now().Format(time.RFC3339)

	// record init config into annotation
	if opts.InitRuntime != nil {
		initRuntimeJSON, err := json.Marshal(opts.InitRuntime)
		if err != nil {
			return fmt.Errorf("failed to marshal init runtime options: %w", err)
		}
		annotations[v1alpha1.AnnotationInitRuntimeRequest] = string(initRuntimeJSON)
		if opts.InitRuntime.AccessToken != "" {
			annotations[v1alpha1.AnnotationRuntimeAccessToken] = opts.InitRuntime.AccessToken
		}
	}

	// record csi mount config into annotation
	if opts.CSIMount != nil {
		if opts.CSIMount.MountOptionListRaw != "" {
			// record the csi mount config to annotation
			annotations[models.ExtensionKeyClaimWithCSIMount_MountConfig] = opts.CSIMount.MountOptionListRaw
		}
	}

	sbx.SetAnnotations(annotations)
	return nil
}

// SetResources applies in-place resource resize to the first container.
func (s *Sandbox) SetResources(requests, limits corev1.ResourceList) {
	if s.Spec.Template == nil {
		return
	}
	pod := &corev1.Pod{
		Spec: s.Spec.Template.Spec,
	}
	resizedPod, _ := buildResourceResizedPod(pod, requests, limits)
	s.Spec.Template.Spec = resizedPod.Spec
}

var DefaultCreateSandbox = createSandbox

func createSandbox(ctx context.Context, sbx *v1alpha1.Sandbox, c client.Client) (*v1alpha1.Sandbox, error) {
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("context canceled before creating sandbox: %w", ctx.Err())
	default:
	}
	err := c.Create(ctx, sbx)
	if err != nil {
		return nil, err
	}
	return sbx, nil
}

func performLockSandbox(ctx context.Context, sbx *Sandbox, lockType infra.LockType, opts infra.ClaimSandboxOptions, cache cache.Provider) error {
	ctx = logs.Extend(ctx, "action", "performLockSandbox")
	log := klog.FromContext(ctx)
	c := cache.GetClient()
	utils.LockSandbox(sbx.Sandbox, opts.LockString, opts.User)
	var updated *v1alpha1.Sandbox
	var err error
	if lockType == infra.LockTypeCreate {
		log.Info("locking new sandbox via create", "sandbox", klog.KObj(sbx.Sandbox))
		updated, err = DefaultCreateSandbox(ctx, sbx.Sandbox, c)
	} else {
		log.Info("locking existing sandbox via update", "sandbox", klog.KObj(sbx.Sandbox))
		err = c.Update(ctx, sbx.Sandbox)
		if err == nil {
			updated = sbx.Sandbox
		}
	}
	if err == nil {
		sbx.Sandbox = updated
		return nil
	}
	return err
}

func buildResourceResizedPod(pod *corev1.Pod, requests, limits corev1.ResourceList) (*corev1.Pod, bool) {
	if len(pod.Spec.Containers) == 0 {
		return pod.DeepCopy(), false
	}
	clone := pod.DeepCopy()
	changed := setContainerResources(&clone.Spec.Containers[0], requests, limits)
	return clone, changed
}

// supportedResizeResources defines which resources are allowed for in-place resize.
var supportedResizeResources = map[corev1.ResourceName]bool{
	corev1.ResourceCPU: true,
}

// setContainerResources updates the container's requests and limits for resources
// listed in supportedResizeResources. Unsupported resource types are silently ignored.
// A resource is also skipped if it was not originally set on the container.
func setContainerResources(container *corev1.Container, requests, limits corev1.ResourceList) bool {
	changed := false
	for resName, target := range requests {
		if !supportedResizeResources[resName] {
			continue
		}
		if cur, ok := container.Resources.Requests[resName]; !ok || cur.IsZero() {
			continue
		}
		if container.Resources.Requests == nil {
			container.Resources.Requests = corev1.ResourceList{}
		}
		container.Resources.Requests[resName] = target.DeepCopy()
		changed = true
	}
	for resName, target := range limits {
		if !supportedResizeResources[resName] {
			continue
		}
		if cur, ok := container.Resources.Limits[resName]; !ok || cur.IsZero() {
			continue
		}
		if container.Resources.Limits == nil {
			container.Resources.Limits = corev1.ResourceList{}
		}
		container.Resources.Limits[resName] = target.DeepCopy()
		changed = true
	}
	return changed
}

func waitForSandboxReady(ctx context.Context, sbx *Sandbox, opts infra.ClaimSandboxOptions, cache cache.Provider) (cost time.Duration, err error) {
	ctx = logs.Extend(ctx, "action", "waitForSandboxReady")
	log := klog.FromContext(ctx).V(consts.DebugLogLevel).WithValues("sandbox", klog.KObj(sbx))
	start := time.Now()
	defer func() {
		cost = time.Since(start)
	}()
	log.Info("waiting for sandbox ready", "timeout", opts.WaitReadyTimeout)
	if err = cache.NewSandboxWaitReadyTask(ctx, sbx.Sandbox).Wait(opts.WaitReadyTimeout); err != nil {
		log.Error(err, "failed to wait for sandbox ready")
		return
	}
	// Use deepcopy to avoid data race
	if err = sbx.InplaceRefresh(ctx, true); err != nil {
		log.Error(err, "failed to refresh sandbox")
		return
	}
	return
}

func checkSandboxReady(ctx context.Context, sbx *v1alpha1.Sandbox) (bool, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx), "resourceVersion", sbx.GetResourceVersion()).V(consts.DebugLogLevel)
	if sbx.Status.ObservedGeneration != sbx.Generation {
		log.Info("watched sandbox not updated", "generation", sbx.Generation, "observedGeneration", sbx.Status.ObservedGeneration)
		return false, nil
	}
	readyCond := GetSandboxCondition(sbx, v1alpha1.SandboxConditionReady)
	if readyCond.Reason == v1alpha1.SandboxReadyReasonStartContainerFailed {
		err := retriableError{Message: fmt.Sprintf("sandbox start container failed: %s", readyCond.Message)}
		log.Error(err, "sandbox start container failed")
		return false, err
	}

	// If an inplace update is still in progress, wait for it to reach a terminal
	// state (Succeeded or Failed) before reporting ready
	inplaceCond := GetSandboxCondition(sbx, v1alpha1.SandboxConditionInplaceUpdate)
	if inplaceCond.Reason == v1alpha1.SandboxInplaceUpdateReasonInplaceUpdating {
		log.Info("sandbox inplace update still in progress, waiting")
		return false, nil
	}

	ip := sbx.Status.PodInfo.PodIP
	state, reason := stateutils.GetSandboxState(sbx)
	isReady := state == v1alpha1.SandboxStateRunning && ip != ""
	log.Info("sandbox ready checked", "state", state, "reason", reason, "ip", ip, "isReady", isReady, "resourceVersion", sbx.GetResourceVersion())
	if isReady {
		// Expect the resourceVersion to ensure InplaceRefresh fetches the latest from API server
		expectationutils.ResourceVersionExpectationExpect(sbx)
	}
	return isReady, nil
}
