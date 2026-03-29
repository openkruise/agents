package sandboxcr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonutils "github.com/openkruise/agents/pkg/utils"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/sandboxset"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/pkg/utils/expectations"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/pkg/utils/sandbox-manager/proxyutils"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
)

func ValidateAndInitClaimOptions(opts infra.ClaimSandboxOptions) (infra.ClaimSandboxOptions, error) {
	if opts.User == "" {
		return infra.ClaimSandboxOptions{}, fmt.Errorf("user is required")
	}
	if opts.Template == "" {
		return infra.ClaimSandboxOptions{}, fmt.Errorf("template is required")
	}
	if opts.CSIMount != nil {
		if opts.InitRuntime == nil {
			return infra.ClaimSandboxOptions{}, fmt.Errorf("init runtime is required when csi mount is specified")
		}
	}
	if opts.InplaceUpdate != nil && opts.InplaceUpdate.Image == "" && opts.InplaceUpdate.Resources == nil {
		return infra.ClaimSandboxOptions{}, fmt.Errorf("inplace update requires either image or resources to be set")
	}
	if opts.InplaceUpdate != nil && opts.InplaceUpdate.Resources != nil {
		if opts.InplaceUpdate.Resources.ScaleFactor <= 1 {
			return infra.ClaimSandboxOptions{}, fmt.Errorf("cpu scale factor should be greater than 1")
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
func TryClaimSandbox(ctx context.Context, opts infra.ClaimSandboxOptions, pickCache *sync.Map, cache *Cache, client *clients.ClientSet,
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
	sbx, lockType, err = pickAnAvailableSandbox(ctx, opts, pickCache, cache, client.SandboxClient, createLimiter)
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

	err = performLockSandbox(ctx, sbx, lockType, opts, client, cache)
	if err != nil {
		// TODO: these lines cannot be covered by tests currently, which will be fixed when the cache is converted to controller-runtime
		log.Error(err, "failed to lock sandbox")
		if apierrors.IsConflict(err) {
			utils.ResourceVersionExpectationExpect(&metav1.ObjectMeta{
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
	utils.ResourceVersionExpectationExpect(sbx)
	log = log.WithValues("sandbox", klog.KObj(sbx.Sandbox))
	log.Info("sandbox locked", "cost", metrics.PickAndLock, "type", metrics.LockType)
	claimed = sbx
	freeWorkerOnce() // free worker early

	// Step 3: Built-in post processes. The locked sandbox must be always returned to be cleared properly.
	// Wait for sandbox ready when:
	// 1. Creating/speculating a new sandbox, OR
	// 2. Any inplace update is requested (image and/or resources)
	shouldWaitForSandboxReady := lockType == infra.LockTypeCreate ||
		lockType == infra.LockTypeSpeculate ||
		(opts.InplaceUpdate != nil && (opts.InplaceUpdate.Image != "" || opts.InplaceUpdate.Resources != nil))
	if shouldWaitForSandboxReady {
		log.Info("waiting for sandbox ready", "inplaceUpdate", opts.InplaceUpdate != nil, "hasImageUpdate", opts.InplaceUpdate != nil && opts.InplaceUpdate.Image != "", "hasResourcesUpdate", opts.InplaceUpdate != nil && opts.InplaceUpdate.Resources != nil)
		metrics.WaitReady, err = waitForSandboxReady(ctx, sbx, opts, cache)
		metrics.Total += metrics.WaitReady
		if err != nil {
			log.Error(err, "failed to wait for sandbox ready", "cost", metrics.WaitReady)
			err = retriableError{Message: fmt.Sprintf("failed to wait for sandbox ready: %s", err)}
			return
		}
		log.Info("sandbox is ready", "cost", metrics.WaitReady)
	}

	// Step 4: Wait for inplace update (resources) to complete - only when:
	// 1. It's a LockTypeUpdate (not create/speculate)
	// 2. Resources update is requested
	// Note: Image update is handled in Step 3 by waiting for sandbox ready
	if lockType == infra.LockTypeUpdate && opts.InplaceUpdate != nil && opts.InplaceUpdate.Resources != nil {
		log.Info("waiting for sandbox inplace update (resources)", "scaleFactor", opts.InplaceUpdate.Resources.ScaleFactor, "returnOnFeasible", opts.InplaceUpdate.Resources.ReturnOnFeasible)
		if opts.InplaceUpdate.Resources.ReturnOnFeasible {
			metrics.InplaceUpdate, err = waitForSandboxInplaceFeasible(ctx, sbx, opts, cache)
		} else {
			metrics.InplaceUpdate, err = waitForSandboxReady(ctx, sbx, opts, cache)
		}
		metrics.Total += metrics.InplaceUpdate
		if err != nil {
			log.Error(err, "failed to wait for sandbox inplace update (resources)")
			err = retriableError{Message: fmt.Sprintf("failed to wait for sandbox inplace update (resources): %s", err)}
			return
		}
		log.Info("sandbox inplace update (resources) is ready", "cost", metrics.InplaceUpdate)
	}

	if opts.InitRuntime != nil {
		log.Info("starting to init runtime", "opts", opts.InitRuntime)
		metrics.InitRuntime, err = initRuntime(ctx, sbx, *opts.InitRuntime)
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
		metrics.CSIMount, err = processCSIMounts(ctx, sbx, *opts.CSIMount)
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
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := sbx.Kill(cleanupCtx); err != nil {
			log.Error(err, "failed to delete locked sandbox")
		} else {
			log.Info("sandbox deleted")
		}
	}
}

func csiMount(ctx context.Context, sbx *Sandbox, opts config.MountConfig) (time.Duration, error) {
	ctx = logs.Extend(ctx, "action", "csiMount")
	start := time.Now()
	err := sbx.CSIMount(ctx, opts.Driver, opts.RequestRaw)
	return time.Since(start), err
}

// processCSIMounts performs CSI volume mounting operations for all mount configurations.
// It iterates through each mount option in the list and attempts to mount them sequentially.
// If a mount operation fails, it logs the error and continues with the next mount option
// to ensure that a single failure doesn't block other mounts.
// Returns the total duration spent on all mount operations and any accumulated errors.
func processCSIMounts(ctx context.Context, sbx *Sandbox, opts config.CSIMountOptions) (time.Duration, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
	start := time.Now()

	for _, opt := range opts.MountOptionList {
		mountDuration, err := csiMount(ctx, sbx, opt)
		if err != nil {
			log.Error(err, "failed to perform CSI mount", "mountOptionConfig", opt)
			return time.Since(start), err
		}
		log.Info("CSI mount completed successfully",
			"mountOptionConfig", opt,
			"duration", mountDuration)
	}
	totalDuration := time.Since(start)
	return totalDuration, nil
}

func getPickKey(sbx *v1alpha1.Sandbox) string {
	return client.ObjectKeyFromObject(sbx).String()
}

func pickAnAvailableSandbox(ctx context.Context, opts infra.ClaimSandboxOptions,
	pickCache *sync.Map, cache *Cache, client clients.SandboxClient, limiter *rate.Limiter) (*Sandbox, infra.LockType, error) {
	template, cnt := opts.Template, opts.CandidateCounts
	ctx = logs.Extend(ctx, "action", "pickAnAvailableSandbox")
	log := klog.FromContext(ctx).WithValues("template", template).V(consts.DebugLogLevel)
	objects, err := cache.ListSandboxesInPool(template)
	if err != nil {
		return nil, "", err
	}
	if len(objects) == 0 {
		if opts.CreateOnNoStock {
			log.Info("will create a new sandbox", "reason", "NoStock")
			return newSandboxFromSandboxSet(opts, cache, client, limiter)
		}
		return nil, "", NoAvailableError(template, "no stock")
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
		if !utils.ResourceVersionExpectationSatisfied(obj) {
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

	// Step 1: select from available candidate
	log.Info("picking from available candidates")
	sbx, pickErr := pickFromCandidates(ctx, availableCandidates, pickCache)
	if pickErr == nil {
		return AsSandbox(sbx, cache, client), infra.LockTypeUpdate, nil
	}
	log.Error(pickErr, "failed to pick from available candidates")

	// Step 2: select from speculated candidates
	if opts.SpeculateCreatingDuration > 0 {
		log.Info("picking from speculated candidates")
		sbx, pickErr = pickFromCandidates(ctx, speculatingCandidates, pickCache)
		if pickErr == nil {
			log.Info("will speculate creating sandbox", "sandbox", klog.KObj(sbx))
			return AsSandbox(sbx, cache, client), infra.LockTypeSpeculate, nil
		}
	}

	// Step 3: create new sandbox
	if opts.CreateOnNoStock {
		log.Info("will create a new sandbox")
		return newSandboxFromSandboxSet(opts, cache, client, limiter)
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
			if !utils.ResourceVersionExpectationSatisfied(obj) {
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

func newSandboxFromSandboxSet(opts infra.ClaimSandboxOptions, cache *Cache, client clients.SandboxClient, limiter *rate.Limiter) (*Sandbox, infra.LockType, error) {
	if limiter != nil {
		if !limiter.Allow() {
			return nil, "", NoAvailableError(opts.Template, "sandbox creation is not allowed by rate limiter")
		}
	}
	sbs, err := cache.GetSandboxSet(opts.Template)
	if err != nil {
		return nil, "", NoAvailableError(opts.Template, "cannot create new sandbox: "+err.Error())
	}
	sbx := sandboxset.NewSandboxFromSandboxSet(sbs)
	// sandbox manager creates high-priority sandbox
	sbx.Annotations[v1alpha1.SandboxAnnotationPriority] = "100"
	for _, anno := range FilteredAnnotationsOnCreation {
		delete(sbx.Annotations, anno)
	}
	return AsSandbox(sbx, cache, client), infra.LockTypeCreate, nil
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
			if err := applySandboxCPUResize(sbx, opts.InplaceUpdate.Resources.ScaleFactor); err != nil {
				return err
			}
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
	sbx.SetAnnotations(annotations)
	return nil
}

func applySandboxCPUResize(sbx *Sandbox, factor float64) error {
	if sbx.Spec.Template == nil {
		return nil
	}
	pod := &corev1.Pod{
		Spec: sbx.Spec.Template.Spec,
	}
	resizedPod, _, err := buildCPUResizedPod(pod, factor)
	if err != nil {
		return err
	}
	sbx.Spec.Template.Spec = resizedPod.Spec
	return nil
}

var DefaultCreateSandbox = createSandbox

func createSandbox(ctx context.Context, sbx *v1alpha1.Sandbox, client *clients.ClientSet, cache infra.CacheProvider) (*v1alpha1.Sandbox, error) {
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("context canceled before creating sandbox: %w", ctx.Err())
	default:
	}
	return client.ApiV1alpha1().Sandboxes(sbx.Namespace).Create(ctx, sbx, metav1.CreateOptions{})
}

func performLockSandbox(ctx context.Context, sbx *Sandbox, lockType infra.LockType, opts infra.ClaimSandboxOptions, client *clients.ClientSet, cache infra.CacheProvider) error {
	ctx = logs.Extend(ctx, "action", "performLockSandbox")
	log := klog.FromContext(ctx)
	utils.LockSandbox(sbx.Sandbox, opts.LockString, opts.User)
	var updated *v1alpha1.Sandbox
	var err error
	if lockType == infra.LockTypeCreate {
		log.Info("locking new sandbox via create", "sandbox", klog.KObj(sbx.Sandbox))
		updated, err = DefaultCreateSandbox(ctx, sbx.Sandbox, client, cache)
	} else {
		log.Info("locking existing sandbox via update", "sandbox", klog.KObj(sbx.Sandbox))
		updated, err = client.ApiV1alpha1().Sandboxes(sbx.Namespace).Update(ctx, sbx.Sandbox, metav1.UpdateOptions{})
	}
	if err == nil {
		sbx.Sandbox = updated
		return nil
	}
	return err
}

func initRuntime(ctx context.Context, sbx *Sandbox, opts config.InitRuntimeOptions) (time.Duration, error) {
	ctx = logs.Extend(ctx, "action", "initRuntime")
	log := klog.FromContext(ctx).WithValues("sandboxID", sbx.GetName(), "resourceVersion", sbx.GetResourceVersion())
	start := time.Now()
	initBody, err := json.Marshal(opts)
	if err != nil {
		log.Error(err, "failed to marshal initBody")
		return 0, err
	}
	retries := -1
	err = retry.OnError(wait.Backoff{
		// about retry 20s
		Duration: 200 * time.Millisecond,
		Factor:   2.0,
		Steps:    5,
		Cap:      10 * time.Second,
	}, commonutils.RetryIfContextNotCanceled(ctx), func() error {
		var initErr error
		retries++
		requestCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer func() {
			cancel()
			if initErr != nil {
				log.Error(initErr, "init runtime request failed", "retries", retries)
			}
		}()

		initErr = sbx.InplaceRefresh(ctx, false)
		if initErr != nil {
			log.Error(initErr, "failed to refresh sandbox")
			return initErr
		}
		runtimeURL := sbx.GetRuntimeURL()
		if runtimeURL == "" {
			log.Error(nil, "runtimeURL is empty")
			return fmt.Errorf("runtimeURL is empty")
		}
		url := runtimeURL + "/init"
		log.Info("sending request to runtime", "resourceVersion", sbx.GetResourceVersion(),
			"url", url, "params", opts, "retries", retries)

		// Create a new request for each retry to avoid Body reuse issue
		r, initErr := http.NewRequestWithContext(requestCtx, http.MethodPost, url, bytes.NewBuffer(initBody))
		if initErr != nil {
			log.Error(initErr, "failed to create request")
			return initErr
		}
		resp, initErr := proxyutils.ProxyRequest(r)
		defer func() {
			// Discard response body to allow connection reuse
			if resp != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}()
		// When ReInit is true, treat 401 as success (sandbox already initialized)
		if resp != nil && resp.StatusCode == http.StatusUnauthorized && opts.ReInit {
			log.Info("init runtime returned 401, treated as success because ReInit is true")
			return nil
		}
		if initErr != nil {
			return initErr
		}
		return nil
	})
	return time.Since(start), err
}

func buildCPUResizedPod(pod *corev1.Pod, factor float64) (*corev1.Pod, bool, error) {
	clone := pod.DeepCopy()
	changed := false

	for i := range clone.Spec.Containers {
		containerChanged := scaleContainerCPU(&clone.Spec.Containers[i], factor)
		changed = changed || containerChanged
	}
	if !changed {
		return clone, false, nil
	}

	return clone, true, nil
}

func scaleContainerCPU(container *corev1.Container, factor float64) bool {
	req := container.Resources.Requests
	lim := container.Resources.Limits
	if req == nil && lim == nil {
		return false
	}

	var (
		cpuReq    resource.Quantity
		cpuLim    resource.Quantity
		hasCPUReq bool
		hasCPULim bool
	)
	if req != nil {
		cpuReq, hasCPUReq = req[corev1.ResourceCPU]
	}
	if lim != nil {
		cpuLim, hasCPULim = lim[corev1.ResourceCPU]
	}
	if !hasCPUReq && !hasCPULim {
		return false
	}

	if container.Resources.Requests == nil {
		container.Resources.Requests = corev1.ResourceList{}
	}
	if container.Resources.Limits == nil {
		container.Resources.Limits = corev1.ResourceList{}
	}

	if hasCPUReq {
		container.Resources.Requests[corev1.ResourceCPU] = scaleCPUQuantity(cpuReq, factor)
	}
	if hasCPULim {
		container.Resources.Limits[corev1.ResourceCPU] = scaleCPUQuantity(cpuLim, factor)
	}
	return true
}

func scaleCPUQuantity(q resource.Quantity, factor float64) resource.Quantity {
	scaledMilli := int64(math.Ceil(float64(q.MilliValue()) * factor))
	// Defensive fallback, should not happen when factor > 1 and q > 0.
	if scaledMilli <= 0 {
		scaledMilli = 1
	}
	return *resource.NewMilliQuantity(scaledMilli, resource.DecimalSI)
}

type containerCPUTarget struct {
	request    resource.Quantity
	hasRequest bool
	limit      resource.Quantity
	hasLimit   bool
}

// buildContainerCPUTargets extracts expected CPU requests/limits from resized Pod spec.
// These targets are later compared with pod.status.containerStatuses[].resources.
func buildContainerCPUTargets(pod *corev1.Pod) map[string]containerCPUTarget {
	targets := make(map[string]containerCPUTarget, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
	for _, c := range pod.Spec.Containers {
		target := containerCPUTarget{}
		if c.Resources.Requests != nil {
			cpuReq, ok := c.Resources.Requests[corev1.ResourceCPU]
			if ok {
				target.request = cpuReq
				target.hasRequest = true
			}
		}
		if c.Resources.Limits != nil {
			cpuLim, ok := c.Resources.Limits[corev1.ResourceCPU]
			if ok {
				target.limit = cpuLim
				target.hasLimit = true
			}
		}
		if target.hasRequest || target.hasLimit {
			targets[c.Name] = target
		}
	}
	// Also handle InitContainers
	for _, c := range pod.Spec.InitContainers {
		target := containerCPUTarget{}
		if c.Resources.Requests != nil {
			cpuReq, ok := c.Resources.Requests[corev1.ResourceCPU]
			if ok {
				target.request = cpuReq
				target.hasRequest = true
			}
		}
		if c.Resources.Limits != nil {
			cpuLim, ok := c.Resources.Limits[corev1.ResourceCPU]
			if ok {
				target.limit = cpuLim
				target.hasLimit = true
			}
		}
		if target.hasRequest || target.hasLimit {
			targets[c.Name] = target
		}
	}
	return targets
}

// isPodCPUResizeApplied checks whether target CPU resources have been enacted
// on running containers according to pod.status.containerStatuses[].resources.
func isPodCPUResizeApplied(pod *corev1.Pod, targets map[string]containerCPUTarget) bool {
	if len(targets) == 0 {
		return false
	}
	// Build status lookup from both regular and init containers
	statuses := make(map[string]*corev1.ContainerStatus, len(pod.Status.ContainerStatuses)+len(pod.Status.InitContainerStatuses))
	for i := range pod.Status.ContainerStatuses {
		statuses[pod.Status.ContainerStatuses[i].Name] = &pod.Status.ContainerStatuses[i]
	}
	for i := range pod.Status.InitContainerStatuses {
		statuses[pod.Status.InitContainerStatuses[i].Name] = &pod.Status.InitContainerStatuses[i]
	}
	for name, target := range targets {
		status, ok := statuses[name]
		if !ok || status.Resources == nil {
			return false
		}
		if target.hasRequest {
			actualReq, ok := status.Resources.Requests[corev1.ResourceCPU]
			if !ok || actualReq.Cmp(target.request) != 0 {
				return false
			}
		}
		if target.hasLimit {
			actualLim, ok := status.Resources.Limits[corev1.ResourceCPU]
			if !ok || actualLim.Cmp(target.limit) != 0 {
				return false
			}
		}
	}
	return true
}

type podResizeStateSnapshot struct {
	pendingTrue       bool
	pendingReason     string
	pendingMessage    string
	inProgressTrue    bool
	inProgressReason  string
	inProgressMessage string
	resizeStatus      corev1.PodResizeStatus
	resizeApplied     bool
}

func inspectPodResizeState(pod *corev1.Pod, targets map[string]containerCPUTarget) podResizeStateSnapshot {
	pending := getPodCondition(pod, corev1.PodResizePending)
	inProgress := getPodCondition(pod, corev1.PodResizeInProgress)

	state := podResizeStateSnapshot{
		resizeStatus:  pod.Status.Resize,
		resizeApplied: isPodCPUResizeApplied(pod, targets),
	}
	if pending != nil && pending.Status == corev1.ConditionTrue {
		state.pendingTrue = true
		state.pendingReason = pending.Reason
		state.pendingMessage = pending.Message
	}
	if inProgress != nil && inProgress.Status == corev1.ConditionTrue {
		state.inProgressTrue = true
		state.inProgressReason = inProgress.Reason
		state.inProgressMessage = inProgress.Message
	}
	return state
}

func (s podResizeStateSnapshot) hasResizeSignal() bool {
	return s.pendingTrue || s.inProgressTrue || s.resizeStatus != ""
}

func (s podResizeStateSnapshot) terminalError() error {
	if s.pendingTrue && s.pendingReason == corev1.PodReasonInfeasible {
		return fmt.Errorf("pod resize is infeasible: %s", s.pendingMessage)
	}
	if s.inProgressTrue && s.inProgressReason == corev1.PodReasonError {
		return fmt.Errorf("pod resize has error: %s", s.inProgressMessage)
	}
	if s.resizeStatus == corev1.PodResizeStatusInfeasible {
		return fmt.Errorf("pod resize is infeasible")
	}
	return nil
}

func (s podResizeStateSnapshot) isSettledWithoutDeferral() bool {
	return !s.pendingTrue && !s.inProgressTrue && s.resizeStatus != corev1.PodResizeStatusDeferred
}

func shouldReturnOnFeasible(state podResizeStateSnapshot, sawResizeSignal bool) bool {
	// returnOnFeasible means caller accepts "resize in progress" as usable.
	if state.inProgressTrue || state.resizeStatus == corev1.PodResizeStatusInProgress || state.resizeApplied {
		return true
	}
	// If lifecycle signals have appeared and then cleared quickly, treat as done.
	return sawResizeSignal && state.isSettledWithoutDeferral()
}

func shouldReturnOnCompleted(state podResizeStateSnapshot, sawResizeSignal bool) bool {
	// Strict path: only return after target resources are actually enacted on container statuses.
	if state.resizeApplied {
		return true
	}
	// Compatibility fallback for clusters that do not expose status.containerStatuses[].resources.
	// We only use this fallback after observing at least one resize lifecycle signal.
	return sawResizeSignal && state.isSettledWithoutDeferral()
}

func waitForPodResizeState(ctx context.Context, client *clients.ClientSet, namespace, name string,
	targetPod *corev1.Pod, timeout time.Duration, returnOnFeasible bool) error {
	log := klog.FromContext(ctx).WithValues("pod", klog.KRef(namespace, name))
	if timeout <= 0 {
		return nil
	}
	// Build expected CPU targets from the request payload (targetPod) instead of re-reading Pod spec.
	// Reason:
	// 1) targetPod is the exact desired state we just sent via UpdateResize;
	// 2) polling should validate runtime status against this stable target;
	// 3) re-reading spec from API can drift (e.g. subsequent updates) and adds an extra API dependency.
	targets := buildContainerCPUTargets(targetPod)

	sawResizeSignal := false
	lastPendingReason := ""
	lastPendingMessage := ""
	err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		pod, err := client.K8sClient.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		state := inspectPodResizeState(pod, targets)
		if state.hasResizeSignal() {
			sawResizeSignal = true
		}
		if state.pendingTrue {
			lastPendingReason = state.pendingReason
			lastPendingMessage = state.pendingMessage
		}
		if err := state.terminalError(); err != nil {
			return false, err
		}

		decideReady := shouldReturnOnCompleted
		if returnOnFeasible {
			decideReady = shouldReturnOnFeasible
		}
		return decideReady(state, sawResizeSignal), nil
	})
	if err != nil {
		log.Error(err, "wait for pod resize state timeout")
		if lastPendingReason != "" {
			return fmt.Errorf("wait for pod resize state: %w (last pending reason=%s, message=%s)", err, lastPendingReason, lastPendingMessage)
		}
		return fmt.Errorf("wait for pod resize state: %w", err)
	}
	return nil
}

func getPodCondition(pod *corev1.Pod, condType corev1.PodConditionType) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		cond := &pod.Status.Conditions[i]
		if cond.Type == condType {
			return cond
		}
	}
	return nil
}

// waitForSandboxReady waits for sandbox to be ready.
// Returns the time taken on success, or an error on timeout/failure.
func waitForSandboxReady(ctx context.Context, sbx *Sandbox, opts infra.ClaimSandboxOptions, cache *Cache) (cost time.Duration, err error) {
	return waitForSandboxCondition(ctx, sbx, cache, "waitForSandboxReady", WaitActionWaitReady, opts.WaitReadyTimeout, checkSandboxReady)
}

// waitForSandboxInplaceFeasible waits for sandbox inplace update to become feasible.
// Returns the time taken on success, or an error on timeout/failure.
func waitForSandboxInplaceFeasible(ctx context.Context, sbx *Sandbox, opts infra.ClaimSandboxOptions, cache *Cache) (cost time.Duration, err error) {
	return waitForSandboxCondition(ctx, sbx, cache, "waitForSandboxInplaceFeasible", WaitActionWaitInplaceFeasible, opts.WaitReadyTimeout, checkSandboxInplaceFeasible)
}

// sandboxCheckFunc is a function that checks if a sandbox satisfies a condition.
type sandboxCheckFunc func(ctx context.Context, sbx *v1alpha1.Sandbox) (bool, error)

// waitForSandboxCondition is a generic function that waits for a sandbox to satisfy a condition.
func waitForSandboxCondition(ctx context.Context, sbx *Sandbox, cache *Cache, actionName string, waitAction WaitAction, timeout time.Duration, checkFunc sandboxCheckFunc) (cost time.Duration, err error) {
	ctx = logs.Extend(ctx, "action", actionName)
	log := klog.FromContext(ctx).V(consts.DebugLogLevel).WithValues("sandbox", klog.KObj(sbx))
	start := time.Now()
	defer func() {
		cost = time.Since(start)
	}()
	log.Info("waiting for sandbox condition", "action", actionName, "timeout", timeout)
	if err = cache.WaitForSandboxSatisfied(ctx, sbx.Sandbox, waitAction, func(sbx *v1alpha1.Sandbox) (bool, error) {
		return checkFunc(ctx, sbx)
	}, timeout); err != nil {
		log.Error(err, "failed to wait for sandbox condition", "action", actionName)
		return
	}
	// Use deepcopy to avoid data race
	if err = sbx.InplaceRefresh(ctx, true); err != nil {
		log.Error(err, "failed to refresh sandbox")
		return
	}
	return
}

func checkSandboxInplaceFeasible(ctx context.Context, sbx *v1alpha1.Sandbox) (bool, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx), "resourceVersion", sbx.GetResourceVersion()).V(consts.DebugLogLevel)
	if sbx.Status.ObservedGeneration != sbx.Generation {
		log.Info("watched sandbox not updated", "generation", sbx.Generation, "observedGeneration", sbx.Status.ObservedGeneration)
		return false, nil
	}
	readyCond := GetSandboxCondition(sbx, v1alpha1.SandboxConditionReady)
	if readyCond.Reason == v1alpha1.SandboxReadyReasonStartContainerFailed {
		err := retriableError{Message: fmt.Sprintf("sandbox inplace update failed: %s", readyCond.Message)}
		log.Error(err, "sandbox inplace update failed")
		return false, err // stop early
	}
	inplaceCond := GetSandboxCondition(sbx, v1alpha1.SandboxConditionInplaceUpdate)
	if inplaceCond.Reason == v1alpha1.SandboxInplaceUpdateReasonFailed {
		err := retriableError{Message: fmt.Sprintf("sandbox inplace update failed: %s", inplaceCond.Message)}
		log.Error(err, "sandbox inplace update failed")
		return false, err // stop early
	}
	if inplaceCond.Status == metav1.ConditionFalse && inplaceCond.Reason == v1alpha1.SandboxInplaceUpdateReasonInplaceUpdating {
		utils.ResourceVersionExpectationExpect(sbx)
		return true, nil
	}
	if inplaceCond.Status == metav1.ConditionTrue && inplaceCond.Reason == v1alpha1.SandboxInplaceUpdateReasonSucceeded {
		utils.ResourceVersionExpectationExpect(sbx)
		return true, nil
	}
	log.Info("sandbox inplace feasible signal not observed yet",
		"inplaceCondStatus", inplaceCond.Status, "inplaceCondReason", inplaceCond.Reason)
	return false, nil
}

func checkSandboxReady(ctx context.Context, sbx *v1alpha1.Sandbox) (bool, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx), "resourceVersion", sbx.GetResourceVersion()).V(consts.DebugLogLevel)
	if sbx.Status.ObservedGeneration != sbx.Generation {
		log.Info("watched sandbox not updated", "generation", sbx.Generation, "observedGeneration", sbx.Status.ObservedGeneration)
		return false, nil
	}
	cond := GetSandboxCondition(sbx, v1alpha1.SandboxConditionReady)
	if cond.Reason == v1alpha1.SandboxReadyReasonStartContainerFailed {
		err := retriableError{Message: fmt.Sprintf("sandbox inplace update failed: %s", cond.Message)}
		log.Error(err, "sandbox inplace update failed")
		return false, err // stop early
	}
	ip := sbx.Status.PodInfo.PodIP
	state, reason := stateutils.GetSandboxState(sbx)
	isReady := state == v1alpha1.SandboxStateRunning && ip != ""
	log.Info("sandbox ready checked", "state", state, "reason", reason, "ip", ip, "isReady", isReady, "resourceVersion", sbx.GetResourceVersion())
	if isReady {
		// Expect the resourceVersion to ensure InplaceRefresh fetches the latest from API server
		utils.ResourceVersionExpectationExpect(sbx)
	}
	return isReady, nil
}
