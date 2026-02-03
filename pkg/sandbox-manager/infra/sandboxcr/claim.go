package sandboxcr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/pkg/utils/sandbox-manager/proxyutils"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	if opts.CandidateCounts <= 0 {
		opts.CandidateCounts = consts.DefaultPoolingCandidateCounts
	}
	if opts.LockString == "" {
		opts.LockString = utils.NewLockString()
	}
	return opts, nil
}

// TryClaimSandbox attempts to claim a sandbox based on the provided Options.
// The returned sandbox is valid only when nil error is returned. Once a non-nil sandbox is returned,
// the sandbox object should not be used anymore and needs appropriate handling.
//
// ValidateAndInitClaimOptions must be called before this function.
func TryClaimSandbox(ctx context.Context, opts infra.ClaimSandboxOptions, pickCache *sync.Map,
	cache *Cache, client clients.SandboxClient) (claimed infra.Sandbox, metrics infra.ClaimMetrics, err error) {
	log := klog.FromContext(ctx)
	defer func() {
		clearFailedSandbox(ctx, claimed, err, opts.ReserveFailedSandbox)
	}()
	// Step 1: Pick an available sandbox
	var sbx *Sandbox
	sbx, err = pickAnAvailableSandbox(ctx, opts.Template, opts.CandidateCounts, pickCache, cache, client)
	if err != nil {
		log.Error(err, "failed to select available sandbox")
		return
	}
	defer pickCache.Delete(getPickKey(sbx.Sandbox))

	log = log.WithValues("sandbox", klog.KObj(sbx.Sandbox))
	log.Info("sandbox picked")

	// Step 2: Modify and lock sandbox. All modifications to be applied to the Sandbox should be performed here.
	if err = modifyPickedSandbox(ctx, sbx, opts); err != nil {
		log.Error(err, "failed to modify picked sandbox")
		err = retriableError{Message: fmt.Sprintf("failed to modify picked sandbox: %s", err)}
		return
	}

	metrics.PickAndLock, err = performLockSandbox(ctx, sbx, opts.LockString, opts.User, client)
	if err != nil {
		log.Error(err, "failed to lock sandbox")
		if apierrors.IsConflict(err) {
			err = retriableError{Message: fmt.Sprintf("failed to lock sandbox: %s", err)}
		}
		return
	}
	metrics.Total += metrics.PickAndLock
	utils.ResourceVersionExpectationExpect(sbx)
	log.Info("sandbox locked", "cost", metrics.PickAndLock)
	claimed = sbx

	// Step 3: Built-in post processes. The locked sandbox must be always returned to be cleared properly.
	if opts.InplaceUpdate != nil {
		log.Info("starting to wait for inplace update to complete")
		metrics.InplaceUpdate, err = waitForInplaceUpdate(ctx, sbx, *opts.InplaceUpdate, cache)
		if err != nil {
			log.Error(err, "failed to wait for inplace update")
			err = retriableError{Message: fmt.Sprintf("failed to wait for inplace update: %s", err)}
			return
		}
		metrics.Total += metrics.InplaceUpdate
		log.Info("inplace update completed", "cost", metrics.InplaceUpdate)
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
		log.Info("the locked sandbox will be deleted")
		if err := sbx.Kill(ctx); err != nil {
			log.Error(err, "failed to delete locked sandbox")
		} else {
			log.Info("sandbox deleted")
		}
	}
}

func csiMount(ctx context.Context, sbx *Sandbox, opts infra.CSIMountOptions) (time.Duration, error) {
	start := time.Now()
	err := sbx.CSIMount(ctx, opts.Driver, opts.RequestRaw)
	return time.Since(start), err
}

func getPickKey(sbx *v1alpha1.Sandbox) string {
	return client.ObjectKeyFromObject(sbx).String()
}

func pickAnAvailableSandbox(ctx context.Context, template string, cnt int, pickCache *sync.Map,
	cache *Cache, client clients.SandboxClient) (*Sandbox, error) {
	log := klog.FromContext(ctx).WithValues("template", template).V(consts.DebugLogLevel)
	objects, err := cache.ListAvailableSandboxes(template)
	if err != nil {
		return nil, err
	}
	if len(objects) == 0 {
		return nil, NoAvailableError(template, "no stock")
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
		return nil, NoAvailableError(template, "no candidate")
	}

	start := rand.IntN(len(candidates))

	i := start
	for {
		obj = candidates[i]
		key := getPickKey(obj)
		if _, loaded := pickCache.LoadOrStore(key, struct{}{}); !loaded {
			return AsSandbox(obj, cache, client), nil
		}
		log.Info("candidate picked by another request", "key", key)
		i = (i + 1) % len(candidates)
		if i == start {
			return nil, NoAvailableError(template, "all candidates are picked")
		}
	}
}

func modifyPickedSandbox(ctx context.Context, sbx *Sandbox, opts infra.ClaimSandboxOptions) error {
	if err := sbx.InplaceRefresh(ctx, true); err != nil {
		return err
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

func performLockSandbox(ctx context.Context, sbx *Sandbox, lock string, owner string, client clients.SandboxClient) (time.Duration, error) {
	start := time.Now()
	utils.LockSandbox(sbx.Sandbox, lock, owner)
	updated, err := client.ApiV1alpha1().Sandboxes(sbx.Namespace).Update(ctx, sbx.Sandbox, metav1.UpdateOptions{})
	if err == nil {
		sbx.Sandbox = updated
		return time.Since(start), nil
	}
	return time.Since(start), err
}

func initRuntime(ctx context.Context, sbx *Sandbox, opts infra.InitRuntimeOptions) (time.Duration, error) {
	log := klog.FromContext(ctx).WithValues("sandboxID", sbx.GetName(), "envVars", opts.EnvVars)
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
		return 0, err
	}
	url := runtimeURL + "/init"
	log.Info("sending request to runtime", "url", url)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(initBody))
	if err != nil {
		log.Error(err, "failed to create request")
		return 0, err
	}

	resp, err := proxyutils.ProxyRequest(r)
	if err != nil {
		log.Error(err, "init runtime request failed")
		return time.Since(start), err
	}
	return time.Since(start), resp.Body.Close()
}

func waitForInplaceUpdate(ctx context.Context, sbx *Sandbox, opts infra.InplaceUpdateOptions, cache *Cache) (cost time.Duration, err error) {
	log := klog.FromContext(ctx).V(consts.DebugLogLevel).WithValues("sandbox", klog.KObj(sbx))
	start := time.Now()
	if opts.Timeout == 0 {
		opts.Timeout = consts.DefaultInplaceUpdateTimeout
	}
	log.Info("waiting for inplace-update to finish", "opts", opts)
	err = cache.WaitForSandboxSatisfied(ctx, sbx.Sandbox, WaitActionInplaceUpdate, func(sbx *v1alpha1.Sandbox) (bool, error) {
		return checkSandboxInplaceUpdate(ctx, sbx)
	}, opts.Timeout)
	return time.Since(start), err
}

func checkSandboxInplaceUpdate(ctx context.Context, sbx *v1alpha1.Sandbox) (bool, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx)).V(consts.DebugLogLevel)
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
	state, reason := stateutils.GetSandboxState(sbx)
	log.Info("sandbox checked", "state", state, "reason", reason)
	return state == v1alpha1.SandboxStateRunning, nil
}
