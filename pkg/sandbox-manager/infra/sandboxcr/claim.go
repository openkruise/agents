package sandboxcr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
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
	"golang.org/x/time/rate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	if opts.InplaceUpdate != nil {
		if opts.InplaceUpdate.Image == "" {
			return infra.ClaimSandboxOptions{}, fmt.Errorf("inplace update image is required")
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

	err = performLockSandbox(ctx, sbx, lockType, opts, client)
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
		metrics.CSIMount, err = csiMount(ctx, sbx, *opts.CSIMount)
		if err != nil {
			log.Error(err, "failed to perform csi mount")
			err = retriableError{Message: fmt.Sprintf("failed to perform csi mount: %s", err)}
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

func csiMount(ctx context.Context, sbx *Sandbox, opts config.CSIMountOptions) (time.Duration, error) {
	ctx = logs.Extend(ctx, "action", "csiMount")
	start := time.Now()
	err := sbx.CSIMount(ctx, opts.Driver, opts.RequestRaw)
	return time.Since(start), err
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
			return newSandboxFromTemplate(opts, cache, client, limiter)
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
		return newSandboxFromTemplate(opts, cache, client, limiter)
	}
	return nil, "", pickErr
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

func newSandboxFromTemplate(opts infra.ClaimSandboxOptions, cache *Cache, client clients.SandboxClient, limiter *rate.Limiter) (*Sandbox, infra.LockType, error) {
	if !limiter.Allow() {
		return nil, "", NoAvailableError(opts.Template, "sandbox creation is not allowed by rate limiter")
	}
	sbs, err := cache.GetSandboxSet(opts.Template)
	if err != nil {
		return nil, "", NoAvailableError(opts.Template, "cannot create new sandbox: "+err.Error())
	}
	sbx := sandboxset.NewSandboxFromSandboxSet(sbs)
	for _, anno := range FilteredAnnotationsOnCreation {
		delete(sbx.Annotations, anno)
	}
	return AsSandbox(sbx, cache, client), infra.LockTypeCreate, nil
}

//func preCheckCandidate(sbx *v1alpha1.Sandbox) error {
//	if sbx.Status.PodInfo.PodIP == "" {
//		return errors.New("podIP is empty")
//	}
//	return preCheckCreating(sbx)
//}

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
		// should perform an inplace update
		sbx.SetImage(opts.InplaceUpdate.Image)
	}
	// claim sandbox
	sbx.SetOwnerReferences([]metav1.OwnerReference{}) // make SandboxSet scale up
	labels := sbx.GetLabels()
	if labels == nil {
		labels = make(map[string]string, 1)
	}
	labels[v1alpha1.LabelSandboxIsClaimed] = "true"
	sbx.SetLabels(labels)

	annotations := sbx.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string, 1)
	}
	annotations[v1alpha1.AnnotationClaimTime] = time.Now().Format(time.RFC3339)
	sbx.SetAnnotations(annotations)
	return nil
}

var DefaultCreateSandbox = createSandbox

func createSandbox(ctx context.Context, sbx *v1alpha1.Sandbox, client *clients.ClientSet) (*v1alpha1.Sandbox, error) {
	return client.ApiV1alpha1().Sandboxes(sbx.Namespace).Create(ctx, sbx, metav1.CreateOptions{})
}

func performLockSandbox(ctx context.Context, sbx *Sandbox, lockType infra.LockType, opts infra.ClaimSandboxOptions, client *clients.ClientSet) error {
	ctx = logs.Extend(ctx, "action", "performLockSandbox")
	log := klog.FromContext(ctx)
	utils.LockSandbox(sbx.Sandbox, opts.LockString, opts.User)
	var updated *v1alpha1.Sandbox
	var err error
	if lockType == infra.LockTypeCreate {
		log.Info("locking new sandbox via create", "sandbox", klog.KObj(sbx.Sandbox))
		updated, err = DefaultCreateSandbox(ctx, sbx.Sandbox, client)
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
	log := klog.FromContext(ctx).WithValues("sandboxID", sbx.GetName(), "envVars", opts.EnvVars, "resourceVersion", sbx.GetResourceVersion())
	start := time.Now()
	params := map[string]any{
		"envVars": opts.EnvVars,
	}
	if opts.AccessToken != "" {
		params["accessToken"] = opts.AccessToken
	}
	initBody, err := json.Marshal(params)
	if err != nil {
		log.Error(err, "failed to marshal initBody")
		return 0, err
	}
	runtimeURL := sbx.GetRuntimeURL()
	if runtimeURL == "" {
		log.Error(nil, "runtimeURL is empty")
		return 0, fmt.Errorf("runtimeURL is empty")
	}
	url := runtimeURL + "/init"
	log.Info("sending request to runtime", "url", url, "params", params)
	retries := -1
	err = retry.OnError(wait.Backoff{
		// about retry 20s
		Duration: 200 * time.Millisecond,
		Factor:   2.0,
		Steps:    5,
		Cap:      10 * time.Second,
	}, func(err error) bool {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}, func() error {
		var initErr error
		retries++
		defer func() {
			if initErr != nil {
				log.Error(initErr, "init runtime request failed", "retries", retries)
			}
		}()
		// Create a new request for each retry to avoid Body reuse issue
		r, initErr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(initBody))
		if initErr != nil {
			log.Error(initErr, "failed to create request")
			return initErr
		}
		resp, initErr := proxyutils.ProxyRequest(r)
		if initErr != nil {
			return initErr
		}
		// Discard response body to allow connection reuse
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return nil
	})
	return time.Since(start), err
}

func waitForSandboxReady(ctx context.Context, sbx *Sandbox, opts infra.ClaimSandboxOptions, cache *Cache) (cost time.Duration, err error) {
	ctx = logs.Extend(ctx, "action", "waitForSandboxReady")
	log := klog.FromContext(ctx).V(consts.DebugLogLevel).WithValues("sandbox", klog.KObj(sbx))
	start := time.Now()
	defer func() {
		cost = time.Since(start)
	}()
	log.Info("waiting for sandbox ready", "timeout", opts.WaitReadyTimeout)
	if err = cache.WaitForSandboxSatisfied(ctx, sbx.Sandbox, WaitActionWaitReady, func(sbx *v1alpha1.Sandbox) (bool, error) {
		return checkSandboxReady(ctx, sbx)
	}, opts.WaitReadyTimeout); err != nil {
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
