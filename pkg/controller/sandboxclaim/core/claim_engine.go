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

package core

// This file implements a controller-owned engine for claiming a Sandbox from
// a SandboxSet pool: pick an available candidate (or create one on no
// stock), lock it via update/create, wait for readiness, and run the claim
// post-processes (runtime init, identity token, CSI mount).
//
// It intentionally does not reuse pkg/sandbox-manager/infra/sandboxcr's
// TryClaimSandbox, even though the two implement closely related operations.
// AGENTS.md requires the Agent Sandbox Controller's complete dependency
// closure to exclude pkg/sandbox-manager/** ("Controllers may reconcile CRDs
// directly, but must not reuse sandbox-manager API behavior, business
// orchestration, or Infra implementations"), so this engine is built only on
// packages that are genuinely neutral: api/v1alpha1, pkg/cache,
// pkg/identity, pkg/utils/runtime (and its config subpackage), pkg/tracing,
// pkg/utils/expectations, and pkg/utils/timeout.
//
// This engine only implements what the SandboxClaim controller actually
// uses: it has no notion of quota admission (SandboxAdmission/LockString),
// speculative pick of in-flight creates, or a per-call rate limiter /
// bounded worker pool. Those exist in sandboxcr.TryClaimSandbox for the
// E2B-API-driven claim path (through infra.Infrastructure.ClaimSandbox);
// buildClaimOptions never populated the controller equivalents, so the
// controller never exercised that machinery and it is not ported here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/api/validate/content"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
	"github.com/openkruise/agents/pkg/controller/sandboxset"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/tracing"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"github.com/openkruise/agents/pkg/utils/runtime"
	runtimeconfig "github.com/openkruise/agents/pkg/utils/runtime/config"
	timeoututils "github.com/openkruise/agents/pkg/utils/timeout"
)

// Sentinel values for ReserveFailedSandboxFor, mirroring the semantics of
// pkg/sandbox-manager/consts (which this package must not import). Owned
// locally because they encode this engine's own failure-cleanup policy, not
// a contract shared with sandbox-manager.
const (
	// reserveFailedSandboxNever instructs the engine to delete a failed
	// sandbox immediately instead of keeping it around.
	reserveFailedSandboxNever = time.Duration(0)
	// reserveFailedSandboxForever instructs the engine to keep a failed
	// sandbox indefinitely for debugging.
	reserveFailedSandboxForever = time.Duration(-1)
	// defaultReserveFailedSandboxFor is used when ReserveFailedSandboxFor is nil.
	defaultReserveFailedSandboxFor = reserveFailedSandboxNever

	defaultCandidateCounts  = 100
	defaultWaitReadyTimeout = 60 * time.Second
	defaultCleanupTimeout   = 30 * time.Second
)

// inplaceUpdateOptions requests an in-place image and/or resource update be
// applied to the picked sandbox before it is locked.
type inplaceUpdateOptions struct {
	Image     string
	Resources *inplaceUpdateResourcesOptions
}

type inplaceUpdateResourcesOptions struct {
	Requests corev1.ResourceList
	Limits   corev1.ResourceList
}

// lockType records how a sandbox was locked for claiming.
type lockType string

const (
	lockTypeCreate lockType = "create"
	lockTypeUpdate lockType = "update"
)

// claimOptions configures a single claim attempt against a SandboxSet pool.
// It intentionally mirrors the shape of infra.ClaimSandboxOptions for the
// fields the controller actually uses, without importing that
// sandbox-manager-owned type.
type claimOptions struct {
	Namespace string
	// User specifies the owner of the sandbox. Required.
	User string
	// Template specifies the pool to claim the sandbox from. Required.
	Template string
	// CandidateCounts is the maximum number of available sandboxes to
	// consider from the cache.
	CandidateCounts int
	// Modifier mutates the sandbox before it is locked.
	Modifier func(sbx *agentsv1alpha1.Sandbox)
	// ReserveFailedSandboxFor controls how long failed sandboxes are kept
	// for debugging. nil means defaultReserveFailedSandboxFor.
	ReserveFailedSandboxFor *time.Duration
	// InplaceUpdate triggers an inplace-update (image and/or resources).
	InplaceUpdate *inplaceUpdateOptions
	// RuntimeConfig records the runtime configuration requested by the
	// claim. Carried through for parity with the claim request; not
	// consumed directly by this engine.
	RuntimeConfig []agentsv1alpha1.RuntimeConfig
	// InitRuntime, when non-nil, initializes the agent-runtime.
	InitRuntime *runtimeconfig.InitRuntimeOptions
	// CSIMount, when non-nil, mounts a CSI volume.
	CSIMount *runtimeconfig.CSIMountOptions
	// WaitReadyTimeout bounds how long to wait for the picked/created
	// sandbox to become ready.
	WaitReadyTimeout time.Duration
	// CreateOnNoStock creates a Sandbox from the SandboxSet template when
	// the pool has no available candidates.
	CreateOnNoStock bool
	// UserMetadataKeys records the keys of user-specified labels/annotations
	// added during claim, for the cleanup flow to remove on release.
	UserMetadataKeys *agentsv1alpha1.UpdatedMetadataInClaim
	// Claim is the SandboxClaim that triggers this claim.
	Claim *agentsv1alpha1.SandboxClaim
	// RuntimeTLSBundle is the client TLS bundle used to reach a
	// TLS-capable agent-runtime during claim post-processing.
	RuntimeTLSBundle *runtime.TLSBundle
}

// claimMetrics reports per-attempt timing, for logging only.
type claimMetrics struct {
	Total         time.Duration
	PickAndLock   time.Duration
	WaitReady     time.Duration
	InitRuntime   time.Duration
	CSIMount      time.Duration
	SecurityToken time.Duration
	TrafficToken  time.Duration
	LockType      lockType
}

func (m claimMetrics) String() string {
	return fmt.Sprintf("claimMetrics{Total: %v, PickAndLock: %v, LockType: %v, WaitReady: %v, InitRuntime: %v, CSIMount: %v, SecurityToken: %v, TrafficToken: %v}",
		m.Total, m.PickAndLock, m.LockType, m.WaitReady, m.InitRuntime, m.CSIMount, m.SecurityToken, m.TrafficToken)
}

// buildUserMetadataKeys records the keys of user-specified labels/annotations
// so the release flow knows which pool-owned metadata to clean up.
func buildUserMetadataKeys(labels, annotations map[string]string) *agentsv1alpha1.UpdatedMetadataInClaim {
	if len(labels) == 0 && len(annotations) == 0 {
		return nil
	}
	keys := &agentsv1alpha1.UpdatedMetadataInClaim{}
	for k := range labels {
		keys.Labels = append(keys.Labels, k)
	}
	for k := range annotations {
		keys.Annotations = append(keys.Annotations, k)
	}
	return keys
}

// validateAndInitClaimOptions validates opts and fills in defaults. It must
// be called before tryClaimSandbox.
func validateAndInitClaimOptions(opts claimOptions) (claimOptions, error) {
	if opts.User == "" {
		return claimOptions{}, fmt.Errorf("user is required")
	}
	// Owner is also synced to a pod label (AnnotationOwner key). Annotation
	// values are unrestricted but label values are not, so reject early.
	if errs := content.IsLabelValue(opts.User); len(errs) > 0 {
		return claimOptions{}, fmt.Errorf("invalid owner %q for pod label %s: %s", opts.User, agentsv1alpha1.AnnotationOwner, strings.Join(errs, "; "))
	}
	if opts.Template == "" {
		return claimOptions{}, fmt.Errorf("template is required")
	}
	if opts.CSIMount != nil && opts.InitRuntime == nil {
		return claimOptions{}, fmt.Errorf("init runtime is required when csi mount is specified")
	}
	if opts.InplaceUpdate != nil {
		if opts.InplaceUpdate.Image == "" && opts.InplaceUpdate.Resources == nil {
			return claimOptions{}, fmt.Errorf("inplace update requires at least one of image or resources to be set")
		}
		if res := opts.InplaceUpdate.Resources; res != nil {
			if len(res.Requests) == 0 && len(res.Limits) == 0 {
				return claimOptions{}, fmt.Errorf("resources must specify at least one of requests or limits")
			}
			for _, rl := range []corev1.ResourceList{res.Requests, res.Limits} {
				if cpu, ok := rl[corev1.ResourceCPU]; ok {
					if cpu.IsZero() || cpu.Cmp(resource.Quantity{}) < 0 {
						return claimOptions{}, fmt.Errorf("target cpu must be a positive value")
					}
				}
			}
		}
	}
	if opts.CandidateCounts <= 0 {
		opts.CandidateCounts = defaultCandidateCounts
	}
	if opts.WaitReadyTimeout <= 0 {
		opts.WaitReadyTimeout = defaultWaitReadyTimeout
	}
	if opts.ReserveFailedSandboxFor == nil {
		reserve := defaultReserveFailedSandboxFor
		opts.ReserveFailedSandboxFor = &reserve
	}
	return opts, nil
}

// tryClaimSandbox attempts to claim a sandbox based on the provided options.
// The returned sandbox is valid only when a nil error is returned; once a
// non-nil sandbox is returned, it should not be reused and needs appropriate
// handling by the caller.
//
// validateAndInitClaimOptions must be called before this function.
func tryClaimSandbox(ctx context.Context, opts claimOptions, pickCache *sync.Map, provider cache.Provider, createLimiter *rate.Limiter) (claimed *agentsv1alpha1.Sandbox, metrics claimMetrics, err error) {
	ctx, span := tracing.StartControllerSpan(ctx, tracing.SpanControllerClaimSandbox)
	// Use a deferred closure (instead of defer span.End()) so the total claim
	// duration and the final error, known only at return time, can be
	// attached before ending the span.
	defer func() {
		span.SetAttributes(attribute.Float64(tracing.AttrClaimDuration, metrics.Total.Seconds()))
		tracing.EndSpan(ctx, span, err)
	}()
	log := klog.FromContext(ctx)

	select {
	case <-ctx.Done():
		err = fmt.Errorf("context canceled before claiming sandbox: %w", ctx.Err())
		log.Error(err, "context canceled before claiming sandbox")
		return
	default:
	}

	defer func() {
		metrics.Total = metrics.PickAndLock + metrics.WaitReady + metrics.InitRuntime + metrics.CSIMount + metrics.SecurityToken + metrics.TrafficToken
		log.Info("try claim sandbox result", "metrics", metrics.String())
		clearFailedSandbox(ctx, provider, claimed, err, opts.ReserveFailedSandboxFor)
	}()

	// Step 1: Pick an available sandbox.
	pickStart := time.Now()
	sbx, lt, err := pickAnAvailableSandbox(ctx, opts, pickCache, provider)
	if err != nil {
		log.Error(err, "failed to select available sandbox")
		return nil, metrics, err
	}
	defer func() {
		if sbx != nil && lt == lockTypeUpdate {
			pickCache.Delete(getPickKey(sbx))
		}
	}()
	log.Info("sandbox picked", "sandbox", klog.KObj(sbx), "lockType", lt)

	if lt == lockTypeCreate && createLimiter != nil && !createLimiter.Allow() {
		return nil, metrics, fmt.Errorf("no available sandboxes for template %s (sandbox creation is not allowed by rate limiter)", opts.Template)
	}

	// Step 2: Modify and lock the sandbox. All mutations to be applied are
	// performed here.
	sbx, err = modifyPickedSandbox(sbx, lt, opts)
	if err != nil {
		log.Error(err, "failed to modify picked sandbox")
		return nil, metrics, fmt.Errorf("failed to modify picked sandbox: %w", err)
	}

	sbx, err = lockPickedSandbox(ctx, sbx, lt, opts, provider)
	if err != nil {
		return nil, metrics, err
	}
	metrics.LockType = lt
	metrics.PickAndLock = time.Since(pickStart)
	expectations.ResourceVersionExpectationExpect(sbx)
	log = log.WithValues("sandbox", klog.KObj(sbx))
	log.Info("sandbox locked", "cost", metrics.PickAndLock, "type", metrics.LockType)
	claimed = sbx

	// Step 3: Built-in post processes. The locked sandbox must always be
	// returned so it can be cleared properly on failure.
	sbx, err = runClaimPostProcesses(ctx, sbx, lt, opts, provider, &metrics)
	claimed = sbx
	return claimed, metrics, err
}

func pickAnAvailableSandbox(ctx context.Context, opts claimOptions, pickCache *sync.Map, provider cache.Provider) (*agentsv1alpha1.Sandbox, lockType, error) {
	template, cnt := opts.Template, opts.CandidateCounts
	log := klog.FromContext(ctx).WithValues("template", template).V(utils.DebugLogLevel)
	objects, err := provider.ListSandboxesInPool(ctx, cache.ListSandboxesInPoolOptions{Namespace: opts.Namespace, Pool: template})
	if err != nil {
		return nil, "", err
	}
	if len(objects) == 0 {
		if opts.CreateOnNoStock {
			log.Info("will create a new sandbox", "reason", "NoStock")
			return newSandboxFromPool(ctx, opts, provider)
		}
		return nil, "", noAvailableError(template, "no stock")
	}

	// Get the SandboxSet's current update revision to prefer matching sandboxes.
	var updateRevision string
	if sbs, sErr := provider.PickSandboxSet(ctx, cache.PickSandboxSetOptions{Namespace: opts.Namespace, Name: template}); sErr == nil && sbs != nil {
		updateRevision = sbs.Status.UpdateRevision
	}

	availableCandidates := make([]*agentsv1alpha1.Sandbox, 0, cnt)
	for _, obj := range objects {
		if len(availableCandidates) >= cnt {
			break
		}
		if !expectations.ResourceVersionExpectationSatisfied(obj) {
			log.Info("skip out-dated sandbox cache", "sandbox", klog.KObj(obj))
			continue
		}
		if checkErr := preCheckCandidate(obj); checkErr != nil {
			log.Error(checkErr, "skip invalid sandbox", "sandbox", klog.KObj(obj), "resourceVersion", obj.GetResourceVersion())
			continue
		}
		state, _ := utils.GetSandboxState(obj)
		if state != agentsv1alpha1.SandboxStateAvailable {
			continue
		}
		if obj.Status.PodInfo.PodIP == "" {
			log.Info("skip available sandbox without podIP", "sandbox", klog.KObj(obj))
			continue
		}
		availableCandidates = append(availableCandidates, obj)
	}
	log.Info("candidates collected", "available", len(availableCandidates))

	// Split available candidates into updated (current revision) and old
	// groups. Try updated candidates first to reduce conflicts with
	// SandboxSet rolling update, which targets old-version sandboxes.
	var updatedCandidates, oldCandidates []*agentsv1alpha1.Sandbox
	if updateRevision != "" {
		updatedCandidates = make([]*agentsv1alpha1.Sandbox, 0, len(availableCandidates))
		oldCandidates = make([]*agentsv1alpha1.Sandbox, 0, len(availableCandidates))
		for _, c := range availableCandidates {
			if c.Labels[agentsv1alpha1.LabelTemplateHash] == updateRevision {
				updatedCandidates = append(updatedCandidates, c)
			} else {
				oldCandidates = append(oldCandidates, c)
			}
		}
	} else {
		updatedCandidates = availableCandidates
	}

	log.Info("picking from available candidates", "updated", len(updatedCandidates), "old", len(oldCandidates))
	sbx, pickErr := pickFromCandidates(ctx, updatedCandidates, pickCache)
	if pickErr != nil && len(oldCandidates) > 0 {
		log.Info("falling back to old available candidates")
		sbx, pickErr = pickFromCandidates(ctx, oldCandidates, pickCache)
	}
	if pickErr == nil {
		return sbx, lockTypeUpdate, nil
	}
	log.Error(pickErr, "failed to pick from available candidates")

	if opts.CreateOnNoStock {
		log.Info("will create a new sandbox")
		return newSandboxFromPool(ctx, opts, provider)
	}
	return nil, "", noAvailableError(template, pickErr.Error())
}

func pickFromCandidates(ctx context.Context, candidates []*agentsv1alpha1.Sandbox, pickCache *sync.Map) (*agentsv1alpha1.Sandbox, error) {
	log := klog.FromContext(ctx).V(utils.DebugLogLevel)
	if len(candidates) == 0 {
		return nil, errors.New("no candidate")
	}
	start := rand.IntN(len(candidates)) // #nosec G404 -- non-security random for load distribution
	i := start
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context canceled while picking sandbox: %w", ctx.Err())
		default:
		}

		obj := candidates[i]
		key := getPickKey(obj)
		if _, loaded := pickCache.LoadOrStore(key, struct{}{}); !loaded {
			// The flow of the first-level lock introduced by pickCache is:
			// Acquire pickCache -> Attempt second-level optimistic lock via k8s
			// update api -> Release pickCache. This ensures that for the same
			// object, acquiring pickCache must happen after another request
			// completes the expectation, and this check guarantees the same
			// object will not be selected twice.
			if !expectations.ResourceVersionExpectationSatisfied(obj) {
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

func newSandboxFromPool(ctx context.Context, opts claimOptions, provider cache.Provider) (*agentsv1alpha1.Sandbox, lockType, error) {
	sbs, err := provider.PickSandboxSet(ctx, cache.PickSandboxSetOptions{Namespace: opts.Namespace, Name: opts.Template})
	if err != nil {
		return nil, "", noAvailableError(opts.Template, "cannot create new sandbox: "+err.Error())
	}
	var refTemplate *agentsv1alpha1.SandboxTemplate
	if sbs.Spec.TemplateRef != nil {
		refTemplate = &agentsv1alpha1.SandboxTemplate{}
		if err := provider.GetClient().Get(ctx, client.ObjectKey{
			Namespace: sbs.Namespace,
			Name:      sbs.Spec.TemplateRef.Name,
		}, refTemplate); err != nil {
			return nil, "", noAvailableError(opts.Template, "cannot resolve sandbox template: "+err.Error())
		}
	}
	sbx := sandboxset.NewSandboxFromSandboxSet(sbs, refTemplate)
	// The claim engine creates a high-priority sandbox, matching the
	// sandbox-manager's own on-demand claim path.
	sbx.Annotations[agentsv1alpha1.SandboxAnnotationPriority] = "100"
	return sbx, lockTypeCreate, nil
}

func preCheckCandidate(sbx *agentsv1alpha1.Sandbox) error {
	if lock := sbx.Annotations[agentsv1alpha1.AnnotationLock]; lock != "" {
		return fmt.Errorf("sandbox is locked by %s", lock)
	}
	if sbx.CreationTimestamp.IsZero() {
		return errors.New("creation timestamp is zero")
	}
	return nil
}

func noAvailableError(template, reason string) error {
	return fmt.Errorf("no available sandboxes for template %s (%s)", template, reason)
}

// modifyPickedSandbox applies opts.Modifier, the in-place update, and
// claim-time bookkeeping (labels/annotations) to the picked sandbox. For a
// picked (not newly created) sandbox it deep-copies first so the shared
// informer-cache object is never mutated in place.
func modifyPickedSandbox(sbx *agentsv1alpha1.Sandbox, lt lockType, opts claimOptions) (*agentsv1alpha1.Sandbox, error) {
	if lt != lockTypeCreate {
		sbx = sbx.DeepCopy()
	}

	if opts.Modifier != nil {
		opts.Modifier(sbx)
	}
	if opts.InplaceUpdate != nil {
		if opts.InplaceUpdate.Image != "" {
			setSandboxImage(sbx, opts.InplaceUpdate.Image)
		}
		if opts.InplaceUpdate.Resources != nil {
			setSandboxResources(sbx, opts.InplaceUpdate.Resources.Requests, opts.InplaceUpdate.Resources.Limits)
		}
	}

	// Claim the sandbox: clearing owner references makes the owning
	// SandboxSet scale back up.
	sbx.SetOwnerReferences([]metav1.OwnerReference{})
	labels := sbx.GetLabels()
	if labels == nil {
		labels = make(map[string]string, 1)
	}
	labels[agentsv1alpha1.LabelSandboxIsClaimed] = agentsv1alpha1.True
	sbx.SetLabels(labels)

	annotations := sbx.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string, 1)
	}
	annotations[agentsv1alpha1.AnnotationClaimTime] = time.Now().Format(time.RFC3339)

	if opts.InitRuntime != nil {
		initRuntimeJSON, err := json.Marshal(opts.InitRuntime)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal init runtime options: %w", err)
		}
		annotations[agentsv1alpha1.AnnotationInitRuntimeRequest] = string(initRuntimeJSON)
		if opts.InitRuntime.AccessToken != "" {
			annotations[agentsv1alpha1.AnnotationRuntimeAccessToken] = opts.InitRuntime.AccessToken
		}
	}

	if opts.CSIMount != nil && opts.CSIMount.MountOptionListRaw != "" {
		annotations[agentsv1alpha1.AnnotationCSIVolumeConfig] = opts.CSIMount.MountOptionListRaw
	}

	if opts.UserMetadataKeys != nil && (len(opts.UserMetadataKeys.Labels) > 0 || len(opts.UserMetadataKeys.Annotations) > 0) {
		raw, err := json.Marshal(opts.UserMetadataKeys)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal user metadata keys: %w", err)
		}
		annotations[agentsv1alpha1.AnnotationUpdatedMetadataInClaim] = string(raw)
	}

	sbx.SetAnnotations(annotations)
	return sbx, nil
}

func lockPickedSandbox(ctx context.Context, sbx *agentsv1alpha1.Sandbox, lt lockType, opts claimOptions, provider cache.Provider) (*agentsv1alpha1.Sandbox, error) {
	log := klog.FromContext(ctx)
	utils.LockSandbox(sbx, utils.NewLockString(), opts.User)
	// Sync owner onto the pod template as a label so selectors/policies can
	// match it. Reuse AnnotationOwner as the label key so the value stays
	// aligned with the sandbox owner annotation written by LockSandbox.
	// opts.User was validated as a label value in validateAndInitClaimOptions.
	podLabels := getPodLabels(sbx)
	if podLabels == nil {
		podLabels = make(map[string]string, 1)
	}
	podLabels[agentsv1alpha1.AnnotationOwner] = opts.User
	setPodLabels(sbx, podLabels)

	c := provider.GetClient()
	var updated *agentsv1alpha1.Sandbox
	var err error
	if lt == lockTypeCreate {
		log.Info("locking new sandbox via create", "sandbox", klog.KObj(sbx))
		updated, err = createSandbox(ctx, sbx, c)
	} else {
		log.Info("locking existing sandbox via update", "sandbox", klog.KObj(sbx))
		sbx.Annotations = tracing.InjectTraceContext(ctx, sbx.Annotations)
		if err = c.Update(ctx, sbx); err == nil {
			updated = sbx
		}
	}
	if err != nil {
		log.Error(err, "failed to lock sandbox")
		if apierrors.IsConflict(err) {
			expectations.ResourceVersionExpectationExpect(&metav1.ObjectMeta{
				UID:             sbx.GetUID(),
				ResourceVersion: expectations.GetNewerResourceVersion(sbx),
			})
		}
		return nil, fmt.Errorf("failed to lock sandbox: %w", err)
	}
	return updated, nil
}

func createSandbox(ctx context.Context, sbx *agentsv1alpha1.Sandbox, c client.Client) (*agentsv1alpha1.Sandbox, error) {
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("sandbox create not attempted: %w", ctx.Err())
	default:
	}
	// Inject trace context into annotations before creating so the
	// sandbox-controller can establish a parent-child span relationship.
	sbx.Annotations = tracing.InjectTraceContext(ctx, sbx.Annotations)
	if err := c.Create(ctx, sbx); err != nil {
		return nil, err
	}
	return sbx, nil
}

func runClaimPostProcesses(ctx context.Context, sbx *agentsv1alpha1.Sandbox, lt lockType, opts claimOptions, provider cache.Provider, metrics *claimMetrics) (*agentsv1alpha1.Sandbox, error) {
	log := klog.FromContext(ctx)

	if lt == lockTypeCreate || opts.InplaceUpdate != nil {
		log.Info("should wait for sandbox ready", "inplaceUpdate", opts.InplaceUpdate != nil)
		var err error
		var cost time.Duration
		sbx, cost, err = waitForSandboxReady(ctx, sbx, opts, provider)
		metrics.WaitReady = cost
		if err != nil {
			log.Error(err, "failed to wait for sandbox ready", "cost", cost)
			return sbx, fmt.Errorf("failed to wait for sandbox ready: %w", err)
		}
		log.Info("sandbox is ready", "cost", cost)
	}

	// Resolve the per-sandbox runtime transport once for both runtime calls
	// below (init handshake and CSI mounts). This MUST run after the
	// wait-ready gate so the sandbox already advertises the capabilities of
	// the pod that actually runs it.
	rtOpts, err := runtime.TransportOptionsFor(sbx, opts.RuntimeTLSBundle)
	if err != nil {
		log.Error(err, "failed to resolve runtime transport")
		return sbx, err
	}

	if opts.InitRuntime != nil {
		log.Info("starting to init runtime", "opts", opts.InitRuntime)
		refresh := func(ctx context.Context) (*agentsv1alpha1.Sandbox, error) {
			return inplaceRefreshSandbox(ctx, provider, sbx)
		}
		cost, err := runtime.InitRuntime(ctx, sbx, *opts.InitRuntime, refresh, rtOpts...)
		metrics.InitRuntime = cost
		if err != nil {
			log.Error(err, "failed to init runtime")
			return sbx, fmt.Errorf("failed to init runtime: %w", err)
		}
		log.Info("runtime inited", "cost", cost)
	}

	// Issue and propagate the identity-provider security token before
	// performing CSI mounts, mirroring the ordering used by the
	// sandbox-manager claim path: the runtime sidecar (initialized above)
	// receives the token first, and any downstream storage authentication
	// decisions made by the provider can rely on the sandbox annotations
	// already injected by modifyPickedSandbox.
	if identity.IsIDTokenRequested(sbx) {
		cost, err := identity.ProcessSandboxToken(ctx, provider.GetClient(), sbx, rtOpts...)
		metrics.SecurityToken = cost
		if err != nil {
			return sbx, fmt.Errorf("failed to process security token: %w", err)
		}
	}

	if identity.IsAccessTokenRequested(sbx) {
		start := time.Now()
		_, err := identity.IssueSandboxAccessToken(ctx, sbx)
		metrics.TrafficToken = time.Since(start)
		if err != nil {
			return sbx, err
		}
	}

	if opts.CSIMount != nil {
		log.Info("starting to perform csi mount")
		cost, err := runtime.ProcessCSIMounts(ctx, sbx, *opts.CSIMount, rtOpts...)
		metrics.CSIMount = cost
		if err != nil {
			log.Error(err, "failed to perform csi mount")
			return sbx, fmt.Errorf("failed to perform csi mount: %w", err)
		}
		log.Info("csi mount completed", "cost", cost)
	}

	return sbx, nil
}

// clearFailedSandbox cleans up (or reserves) a failed sandbox according to
// reserveFor. A nil reserveFor falls back to defaultReserveFailedSandboxFor.
func clearFailedSandbox(ctx context.Context, provider cache.Provider, sbx *agentsv1alpha1.Sandbox, claimErr error, reserveFor *time.Duration) {
	if claimErr == nil || sbx == nil {
		return
	}
	effective := defaultReserveFailedSandboxFor
	if reserveFor != nil {
		effective = *reserveFor
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultCleanupTimeout)
	defer cancel()
	log := klog.FromContext(cleanupCtx).WithValues("sandbox", klog.KObj(sbx))

	if effective < 0 {
		log.Info("reserving failed sandbox forever for debugging", "reason", claimErr)
		if err := reserveFailedSandbox(cleanupCtx, provider, sbx, timeoututils.Options{}); err != nil {
			log.Error(err, "failed to mark failed sandbox as reserved, deleting it instead")
			deleteFailedSandbox(cleanupCtx, provider, sbx, log)
		}
		return
	}

	if effective > 0 {
		shutdownTime := time.Now().Add(effective)
		log.Info("the failed sandbox will be reserved before delayed deletion", "reason", claimErr, "shutdownTime", shutdownTime)
		if err := reserveFailedSandbox(cleanupCtx, provider, sbx, timeoututils.Options{ShutdownTime: shutdownTime}); err != nil {
			log.Error(err, "failed to reserve failed sandbox, deleting it instead")
			deleteFailedSandbox(cleanupCtx, provider, sbx, log)
		}
		return
	}

	log.Info("the failed sandbox will be deleted", "reason", claimErr)
	deleteFailedSandbox(cleanupCtx, provider, sbx, log)
}

func reserveFailedSandbox(ctx context.Context, provider cache.Provider, sbx *agentsv1alpha1.Sandbox, opts timeoututils.Options) error {
	_, err := retryUpdateSandbox(ctx, provider, sbx, func(s *agentsv1alpha1.Sandbox) (bool, error) {
		labels := s.GetLabels()
		if labels == nil {
			labels = make(map[string]string, 1)
		}
		changed := labels[agentsv1alpha1.LabelSandboxReservedFailed] != agentsv1alpha1.True
		labels[agentsv1alpha1.LabelSandboxReservedFailed] = agentsv1alpha1.True
		s.SetLabels(labels)

		if !opts.ShutdownTime.IsZero() && !timeoututils.Equal(timeoututils.GetTimeoutFromSandbox(s), opts) {
			setSandboxTimeout(s, opts)
			changed = true
		}
		return changed, nil
	})
	return err
}

func deleteFailedSandbox(ctx context.Context, provider cache.Provider, sbx *agentsv1alpha1.Sandbox, log klog.Logger) {
	if err := client.IgnoreNotFound(provider.GetClient().Delete(ctx, sbx)); err != nil {
		log.Error(err, "failed to delete failed sandbox")
		return
	}
	log.Info("sandbox deleted or already gone")
}

// retryUpdateSandbox loads the latest sandbox from the informer cache first,
// applies modifier, and retries on conflict. Conflict retries refresh from
// the API reader to avoid re-reading stale informer data.
func retryUpdateSandbox(ctx context.Context, provider cache.Provider, sbx *agentsv1alpha1.Sandbox, modifier func(*agentsv1alpha1.Sandbox) (bool, error)) (bool, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
	objectKey := client.ObjectKeyFromObject(sbx)
	updated := false
	first := true
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &agentsv1alpha1.Sandbox{}
		var err error
		if first {
			err = provider.GetClient().Get(ctx, objectKey, latest)
		} else {
			err = provider.GetAPIReader().Get(ctx, objectKey, latest)
		}
		first = false
		if err != nil {
			return err
		}

		copied := latest.DeepCopy()
		shouldUpdate, err := modifier(copied)
		if err != nil {
			return err
		}
		if !shouldUpdate {
			updated = false
			return nil
		}
		copied.Annotations = tracing.InjectTraceContext(ctx, copied.Annotations)
		if err := provider.GetClient().Update(ctx, copied); err != nil {
			return err
		}
		expectations.ResourceVersionExpectationExpect(copied)
		updated = true
		return nil
	})
	if err != nil {
		log.Error(err, "failed to update sandbox after retries")
		return false, err
	}
	if updated {
		log.Info("sandbox updated successfully")
	} else {
		log.Info("sandbox update skipped")
	}
	return updated, nil
}

// inplaceRefreshSandbox returns the latest version of sbx, preferring the
// informer cache and falling back to the API reader when the cache is
// missing or stale.
func inplaceRefreshSandbox(ctx context.Context, provider cache.Provider, sbx *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx)).V(utils.DebugLogLevel)
	objectKey := client.ObjectKeyFromObject(sbx)
	newSbx := &agentsv1alpha1.Sandbox{}
	fetchFromAPIServer := false
	if err := provider.GetClient().Get(ctx, objectKey, newSbx); err != nil {
		log.Info("failed to get claimed sandbox from cache, fetch from api-server", "reason", err.Error())
		fetchFromAPIServer = true
	} else if !expectations.ResourceVersionExpectationSatisfied(newSbx) {
		log.Info("sandbox cache is out-dated, fetch from api-server")
		fetchFromAPIServer = true
	}
	if fetchFromAPIServer {
		if err := provider.GetAPIReader().Get(ctx, objectKey, newSbx); err != nil {
			return sbx, err
		}
	}
	if expectations.IsResourceVersionReallyNewer(sbx.GetResourceVersion(), newSbx.GetResourceVersion()) {
		return newSbx.DeepCopy(), nil
	}
	return sbx, nil
}

func waitForSandboxReady(ctx context.Context, sbx *agentsv1alpha1.Sandbox, opts claimOptions, provider cache.Provider) (*agentsv1alpha1.Sandbox, time.Duration, error) {
	log := klog.FromContext(ctx).V(utils.DebugLogLevel).WithValues("sandbox", klog.KObj(sbx))
	start := time.Now()
	log.Info("waiting for sandbox ready", "timeout", opts.WaitReadyTimeout)
	if err := provider.NewSandboxWaitReadyTask(ctx, sbx).Wait(opts.WaitReadyTimeout); err != nil {
		cost := time.Since(start)
		log.Error(err, "failed to wait for sandbox ready")
		if errors.Is(err, cacheutils.ErrWaitNotSatisfied) {
			refreshed, refreshErr := inplaceRefreshSandbox(ctx, provider, sbx)
			if refreshErr != nil {
				log.Error(refreshErr, "failed to refresh sandbox for ready failure diagnosis")
				return sbx, cost, errors.New(sandboxReadyFailureMessage(sbx))
			}
			return refreshed, cost, errors.New(sandboxReadyFailureMessage(refreshed))
		}
		return sbx, cost, err
	}
	refreshed, err := inplaceRefreshSandbox(ctx, provider, sbx)
	if err != nil {
		log.Error(err, "failed to refresh sandbox")
		return sbx, time.Since(start), err
	}
	return refreshed, time.Since(start), nil
}

func sandboxReadyFailureMessage(sbx *agentsv1alpha1.Sandbox) string {
	readyCond := getSandboxCondition(sbx, agentsv1alpha1.SandboxConditionReady)
	inplaceCond := getSandboxCondition(sbx, agentsv1alpha1.SandboxConditionInplaceUpdate)
	state, _ := utils.GetSandboxState(sbx)

	reason := sandboxReadyFailureReason(sbx, state, readyCond, inplaceCond)
	fields := []string{
		fmt.Sprintf("reason=%s", reason),
		fmt.Sprintf("state=%s", state),
		fmt.Sprintf("ready=%s", readyCond.Reason),
	}
	if inplaceCond.Reason != "" {
		fields = append(fields, fmt.Sprintf("inplaceUpdate=%s", inplaceCond.Reason))
	}
	if sbx.Status.ObservedGeneration != sbx.Generation {
		fields = append(fields, fmt.Sprintf("generation=%d/%d", sbx.Status.ObservedGeneration, sbx.Generation))
	}
	return fmt.Sprintf("sandbox %s/%s is not ready before wait timeout: %s", sbx.Namespace, sbx.Name, strings.Join(fields, ", "))
}

func sandboxReadyFailureReason(sbx *agentsv1alpha1.Sandbox, state string, readyCond, inplaceCond metav1.Condition) string {
	if sbx.Status.ObservedGeneration != sbx.Generation {
		return "controller has not observed latest generation"
	}
	if inplaceCond.Reason == agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating {
		return "inplace update is still in progress"
	}
	if sbx.Status.PodInfo.PodIP == "" {
		return "sandbox has no pod IP"
	}
	if readyCond.Reason == agentsv1alpha1.SandboxReadyReasonStartContainerFailed {
		reason := fmt.Sprintf("ready condition reports %s", readyCond.Reason)
		if readyCond.Message != "" {
			reason = fmt.Sprintf("%s: %s", reason, readyCond.Message)
		}
		return reason
	}
	if state != agentsv1alpha1.SandboxStateRunning {
		return fmt.Sprintf("sandbox state is %s", state)
	}
	if readyCond.Reason != "" {
		reason := fmt.Sprintf("ready condition reports %s", readyCond.Reason)
		if readyCond.Message != "" {
			reason = fmt.Sprintf("%s: %s", reason, readyCond.Message)
		}
		return reason
	}
	return "sandbox ready condition is not satisfied"
}

func getSandboxCondition(sbx *agentsv1alpha1.Sandbox, tp agentsv1alpha1.SandboxConditionType) metav1.Condition {
	for _, condition := range sbx.Status.Conditions {
		if condition.Type == string(tp) {
			return condition
		}
	}
	return metav1.Condition{}
}

func getPickKey(sbx *agentsv1alpha1.Sandbox) string {
	return client.ObjectKeyFromObject(sbx).String()
}

func getPodLabels(sbx *agentsv1alpha1.Sandbox) map[string]string {
	if sbx.Spec.Template != nil {
		return sbx.Spec.Template.Labels
	}
	return nil
}

func setPodLabels(sbx *agentsv1alpha1.Sandbox, labels map[string]string) {
	if sbx.Spec.Template != nil {
		sbx.Spec.Template.Labels = labels
	}
}

// mergePodAnnotations merges the given annotations into the sandbox's pod
// template annotations. Existing annotations with the same key are
// overwritten. The pod template's annotations map is initialized if necessary.
func mergePodAnnotations(sbx *agentsv1alpha1.Sandbox, annotations map[string]string) {
	if len(annotations) == 0 || sbx.Spec.Template == nil {
		return
	}
	existing := sbx.Spec.Template.Annotations
	if existing == nil {
		existing = make(map[string]string, len(annotations))
	}
	for k, v := range annotations {
		existing[k] = v
	}
	sbx.Spec.Template.Annotations = existing
}

func setSandboxImage(sbx *agentsv1alpha1.Sandbox, image string) {
	if sbx.Spec.Template != nil && len(sbx.Spec.Template.Spec.Containers) > 0 {
		sbx.Spec.Template.Spec.Containers[0].Image = image
	}
}

// setSandboxResources applies in-place resource resize to the first container.
func setSandboxResources(sbx *agentsv1alpha1.Sandbox, requests, limits corev1.ResourceList) {
	if sbx.Spec.Template == nil {
		return
	}
	pod := &corev1.Pod{Spec: sbx.Spec.Template.Spec}
	resized := buildResourceResizedPod(pod, requests, limits)
	sbx.Spec.Template.Spec = resized.Spec
}

func buildResourceResizedPod(pod *corev1.Pod, requests, limits corev1.ResourceList) *corev1.Pod {
	if len(pod.Spec.Containers) == 0 {
		return pod.DeepCopy()
	}
	clone := pod.DeepCopy()
	setContainerResources(&clone.Spec.Containers[0], requests, limits)
	return clone
}

// supportedResizeResources defines which resources are allowed for in-place resize.
var supportedResizeResources = map[corev1.ResourceName]bool{
	corev1.ResourceCPU: true,
}

// setContainerResources updates the container's requests and limits for
// resources listed in supportedResizeResources. Unsupported resource types
// are silently ignored. A resource is also skipped if it was not originally
// set on the container.
func setContainerResources(container *corev1.Container, requests, limits corev1.ResourceList) {
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
	}
}

func setSandboxTimeout(sbx *agentsv1alpha1.Sandbox, opts timeoututils.Options) {
	if !opts.PauseTime.IsZero() {
		sbx.Spec.PauseTime = &metav1.Time{Time: timeoututils.NormalizeTime(opts.PauseTime)}
	} else {
		sbx.Spec.PauseTime = nil
	}
	if !opts.ShutdownTime.IsZero() {
		sbx.Spec.ShutdownTime = &metav1.Time{Time: timeoututils.NormalizeTime(opts.ShutdownTime)}
	} else {
		sbx.Spec.ShutdownTime = nil
	}
}
