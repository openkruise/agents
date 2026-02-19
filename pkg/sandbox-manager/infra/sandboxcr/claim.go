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
func TryClaimSandbox(ctx context.Context, opts infra.ClaimSandboxOptions, pickCache *sync.Map,
	cache *Cache, client clients.SandboxClient, claimLockChannel chan struct{}) (claimed infra.Sandbox, metrics infra.ClaimMetrics, err error) {
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
		log.Info("try claim sandbox result", "metrics", metrics)
		clearFailedSandbox(ctx, claimed, err, opts.ReserveFailedSandbox)
	}()
	// Step 1: Pick an available sandbox
	var sbx *Sandbox
	var deferFunc func()
	var created bool
	pickStart := time.Now()
	sbx, created, deferFunc, err = pickAnAvailableSandbox(ctx, opts, pickCache, cache, client)
	if err != nil {
		log.Error(err, "failed to select available sandbox")
		return
	}
	defer func() {
		if deferFunc != nil {
			deferFunc()
		}
	}()
	log.Info("sandbox picked")

	// Step 2: Modify and lock sandbox. All modifications to be applied to the Sandbox should be performed here.
	if err = modifyPickedSandbox(ctx, sbx, created, opts); err != nil {
		log.Error(err, "failed to modify picked sandbox")
		err = retriableError{Message: fmt.Sprintf("failed to modify picked sandbox: %s", err)}
		return
	}

	err = performLockSandbox(ctx, sbx, created, opts.LockString, opts.User, client)
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
	metrics.PickAndLock = time.Since(pickStart)
	metrics.Total += metrics.PickAndLock
	utils.ResourceVersionExpectationExpect(sbx)
	log = log.WithValues("sandbox", klog.KObj(sbx.Sandbox))
	log.Info("sandbox locked", "cost", metrics.PickAndLock)
	claimed = sbx
	freeWorkerOnce() // free worker early

	// Step 3: Built-in post processes. The locked sandbox must be always returned to be cleared properly.
	if created || opts.InplaceUpdate != nil {
		log.Info("should wait for sandbox ready", "created", created, "inplaceUpdate", opts.InplaceUpdate != nil)
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
		// use a new context to avoid possible context cancellation
		if err := sbx.Kill(context.Background()); err != nil {
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
	pickCache *sync.Map, cache *Cache, client clients.SandboxClient) (*Sandbox, bool, func(), error) {
	template, cnt := opts.Template, opts.CandidateCounts
	ctx = logs.Extend(ctx, "action", "pickAnAvailableSandbox")
	log := klog.FromContext(ctx).WithValues("template", template).V(consts.DebugLogLevel)
	objects, err := cache.ListAvailableSandboxes(template)
	if err != nil {
		return nil, false, nil, err
	}
	if len(objects) == 0 {
		if opts.CreateOnNoStock {
			log.Info("will create a new sandbox", "reason", "NoStock")
			return newSandboxFromTemplate(opts, cache, client)
		}
		return nil, false, nil, NoAvailableError(template, "no stock")
	}
	var obj *v1alpha1.Sandbox
	candidates := make([]*v1alpha1.Sandbox, 0, cnt)
	for _, obj = range objects {
		if !utils.ResourceVersionExpectationSatisfied(obj) {
			log.Info("skip out-dated sandbox cache", "sandbox", klog.KObj(obj))
			continue
		}
		if obj.Status.Phase == v1alpha1.SandboxRunning && obj.Annotations[v1alpha1.AnnotationLock] == "" {
			candidates = append(candidates, obj)
			if len(candidates) >= cnt {
				break
			}
		}
	}
	if len(candidates) == 0 {
		if opts.CreateOnNoStock {
			log.Info("will create a new sandbox", "reason", "NoCandidate")
			return newSandboxFromTemplate(opts, cache, client)
		}
		return nil, false, nil, NoAvailableError(template, "no candidate")
	}

	start := rand.IntN(len(candidates))

	i := start
	for {
		obj = candidates[i]
		if checkErr := preCheckSandbox(obj); checkErr != nil {
			log.Error(checkErr, "skip invalid sandbox", "sandbox", klog.KObj(obj), "resourceVersion", obj.GetResourceVersion())
		} else {
			key := getPickKey(obj)
			if _, loaded := pickCache.LoadOrStore(key, struct{}{}); !loaded {
				return AsSandbox(obj, cache, client), false, func() {
					pickCache.Delete(key)
				}, nil
			}
			log.Info("candidate picked by another request", "key", key)
		}
		i = (i + 1) % len(candidates)
		if i == start {
			if opts.CreateOnNoStock {
				log.Info("will create a new sandbox", "reason", "AllCandidatesPicked")
				return newSandboxFromTemplate(opts, cache, client)
			}
			return nil, false, nil, NoAvailableError(template, "all candidates are picked")
		}
	}
}

var FilteredAnnotationsOnCreation []string

func newSandboxFromTemplate(opts infra.ClaimSandboxOptions, cache *Cache, client clients.SandboxClient) (*Sandbox, bool, func(), error) {
	sbs, err := cache.GetSandboxSet(opts.Template)
	if err != nil {
		return nil, false, nil, NoAvailableError(opts.Template, "cannot create new sandbox: "+err.Error())
	}
	sbx := sandboxset.NewSandboxFromSandboxSet(sbs)
	sbx.Labels[v1alpha1.LabelSandboxIsClaimed] = "true"
	for _, anno := range FilteredAnnotationsOnCreation {
		delete(sbx.Annotations, anno)
	}
	return AsSandbox(sbx, cache, client), true, nil, nil
}

func preCheckSandbox(sbx *v1alpha1.Sandbox) error {
	if sbx.Status.PodInfo.PodIP == "" {
		return errors.New("podIP is empty")
	}
	return nil
}

func modifyPickedSandbox(ctx context.Context, sbx *Sandbox, created bool, opts infra.ClaimSandboxOptions) error {
	ctx = logs.Extend(ctx, "action", "modifyPickedSandbox", "sandbox", klog.KObj(sbx))
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
	if !created {
		log.Info("refreshing existing sandbox")
		// refresh existing sandbox
		if err := sbx.InplaceRefresh(ctx, true); err != nil {
			return err
		}
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

	sbx.Annotations[v1alpha1.AnnotationClaimTime] = time.Now().Format(time.RFC3339)
	return nil
}

func performLockSandbox(ctx context.Context, sbx *Sandbox, created bool, lock string, owner string, client clients.SandboxClient) error {
	ctx = logs.Extend(ctx, "action", "performLockSandbox")
	log := klog.FromContext(ctx)
	utils.LockSandbox(sbx.Sandbox, lock, owner)
	var updated *v1alpha1.Sandbox
	var err error
	if !created {
		log.Info("locking existing sandbox via update", "sandbox", klog.KObj(sbx.Sandbox))
		updated, err = client.ApiV1alpha1().Sandboxes(sbx.Namespace).Update(ctx, sbx.Sandbox, metav1.UpdateOptions{})
	} else {
		log.Info("locking new sandbox via create", "sandbox", klog.KObj(sbx.Sandbox))
		updated, err = client.ApiV1alpha1().Sandboxes(sbx.Namespace).Create(ctx, sbx.Sandbox, metav1.CreateOptions{})
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
	log.Info("sending request to runtime", "url", url)
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
	if err = sbx.InplaceRefresh(ctx, false); err != nil {
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
	log.Info("sandbox ready checked", "state", state, "reason", reason, "ip", ip)
	return state == v1alpha1.SandboxStateRunning && ip != "", nil
}
