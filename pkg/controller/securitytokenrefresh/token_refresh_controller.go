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

// Package securitytokenrefresh implements a controller-runtime reconciler that
// refreshes the security token of claimed sandboxes shortly before the token
// stored in the sandbox annotation identity.AgentKeyTokenRefreshStatus expires.
//
// The controller is meant to run inside the agent-sandbox-controller binary,
// alongside the existing Sandbox / SandboxClaim / Checkpoint controllers. It is
// gated by the SecurityIdentityProvider feature gate so that clusters without
// an identity provider behave exactly like before.
package securitytokenrefresh

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/securitytokenrefresh/core"
	"github.com/openkruise/agents/pkg/discovery"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/identity"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

const (
	controllerName = "security-token-refresh"

	// defaultRefreshLeadTime is how long BEFORE AccessTokenExpiration the
	// controller proactively rotates the token. With the default of 30m, a
	// token that expires at T will normally be refreshed somewhere in
	// [T-30m ± jitter], so the runtime never observes an expired credential
	// even under transient identity-provider slowdowns.
	//
	// Operators that want refreshes to happen even earlier (e.g. for tokens
	// with very short TTLs) can shorten this via the
	// --security-token-refresh-lead-time flag.
	defaultRefreshLeadTime = 30 * time.Minute
	// defaultJitterRatio randomises the lead time by ±ratio so a fleet of
	// sandboxes does not stampede the identity provider at the same instant.
	defaultJitterRatio = 0.1
	// defaultRefreshRetryAfter is the requeue interval used after a refresh
	// attempt fails. It is intentionally aligned with the lead window: the
	// reconciler only ever calls refresher.Refresh once we are inside
	// [refreshAt, expireAt] (or past expireAt), so a 1-minute requeue means
	// that, with the default 30m lead time, the controller gets up to ~30
	// retry chances before the credential actually expires. controller-runtime
	// additionally applies its own rate-limited workqueue back-off on top of
	// this value when the reconciler returns an error.
	defaultRefreshRetryAfter = 1 * time.Minute
	// minRequeueAfter clamps the requeue duration so we never schedule a
	// zero-duration requeue (which would behave like an immediate retry).
	minRequeueAfter = time.Second
)

func init() {
	flag.IntVar(&concurrentReconciles, "security-token-refresh-workers", concurrentReconciles,
		"Max concurrent reconciles for SecurityTokenRefresh controller.")
	flag.DurationVar(&refreshLeadTime, "security-token-refresh-lead-time", defaultRefreshLeadTime,
		"How long BEFORE AccessTokenExpiration the controller starts a refresh (default 30m).")
	flag.Float64Var(&jitterRatio, "security-token-refresh-jitter-ratio", defaultJitterRatio,
		"Jitter ratio (0~1) applied to the refresh lead time to spread refresh load.")
	flag.DurationVar(&refreshRetryAfter, "security-token-refresh-retry-after", defaultRefreshRetryAfter,
		"Requeue interval applied after a refresh attempt fails; with the default 30m lead window this gives ~30 retry chances before the token actually expires.")
}

var (
	concurrentReconciles = 500
	refreshLeadTime      = defaultRefreshLeadTime
	jitterRatio          = defaultJitterRatio
	refreshRetryAfter    = defaultRefreshRetryAfter

	securityTokenRefreshControllerKind = agentsv1alpha1.GroupVersion.WithKind("Sandbox")

	// setupHooks holds extension hooks registered via RegisterSetupHook. They
	// run at the tail of Add, after the reconciler is wired with the manager,
	// so downstream distributions can attach extra peer workers (e.g. a CA
	// bundle sync runnable, additional metrics emitters, ...) without forking
	// this package. The slice is mutated only at init() time and during tests.
	setupHooks []SetupHook
)

// SetupHook is the canonical extension point invoked from Add after the
// SecurityTokenRefresh reconciler has been registered with the manager.
//
// The manager passed in is fully constructed but not yet started, so a hook
// may freely call mgr.Add(...) to attach additional Runnables, or use
// mgr.GetClient() / mgr.GetEventRecorderFor(...) when wiring its own state.
// A hook that returns a non-nil error aborts controller setup, which mirrors
// the behaviour of the reconciler's own SetupWithManager failure.
type SetupHook func(mgr manager.Manager) error

// RegisterSetupHook appends h to the list of hooks executed at the end of
// Add. It is meant to be called from a package-level init() in a downstream
// (e.g. enterprise) build that wants to extend the token-refresh controller
// without modifying this file.
//
// Registration order is preserved and hooks run sequentially; the first hook
// that returns an error stops further execution. The function is safe to call
// before Add but is NOT safe for concurrent use, mirroring the standard
// controller-runtime registration pattern.
func RegisterSetupHook(h SetupHook) {
	if h == nil {
		return
	}
	setupHooks = append(setupHooks, h)
}

// runSetupHooks executes every registered SetupHook in registration order. It
// is split out from Add purely for testability: tests can drive it directly
// without needing a fully-fledged manager. The error returned by the first
// failing hook is wrapped so the caller can tell it came from the extension
// surface rather than from the reconciler itself.
func runSetupHooks(mgr manager.Manager) error {
	for i, h := range setupHooks {
		if err := h(mgr); err != nil {
			return fmt.Errorf("securitytokenrefresh setup hook #%d failed: %w", i, err)
		}
	}
	return nil
}

func Add(mgr manager.Manager) error {
	if !utilfeature.DefaultFeatureGate.Enabled(features.SecurityIdentityProviderGate) ||
		!discovery.DiscoverGVK(securityTokenRefreshControllerKind) {
		return nil
	}

	err := (&SecurityTokenRefreshReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor(controllerName),
		refresher: core.NewDefaultRefresher(mgr.GetClient()),
		now:       time.Now,
		// rand.Float64 returns [0,1); centred to [-1,1) below for symmetric jitter.
		randFloat: rand.Float64,
	}).SetupWithManager(mgr)
	if err != nil {
		return err
	}
	// Run downstream-registered extension hooks. They execute AFTER the
	// reconciler is wired so they can rely on the same manager / client /
	// recorder, and BEFORE Add returns so a hook failure aborts startup.
	if err := runSetupHooks(mgr); err != nil {
		return err
	}
	klog.Infof("Started SecurityTokenRefreshReconciler successfully, refreshLeadTime=%s jitterRatio=%v refreshRetryAfter=%s setupHooks=%d",
		refreshLeadTime, jitterRatio, refreshRetryAfter, len(setupHooks))
	return nil
}

// SecurityTokenRefreshReconciler refreshes sandbox security tokens before they expire.
type SecurityTokenRefreshReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	refresher core.Refresher

	// now and randFloat are injected to keep the reconciler deterministic in tests.
	now       func() time.Time
	randFloat func() float64
}

// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch;update

// Reconcile decides, for the sandbox referenced by req, whether its security
// token has to be refreshed right now and, if not, when to come back.
//
// The flow is intentionally side-effect-light: every decision (refresh vs.
// requeue) is derived from the AccessTokenExpiration recorded in the
// identity.AgentKeyTokenRefreshStatus annotation. The actual issue/propagate/patch
// chain lives in core.Refresher to keep this method focused on policy.
func (r *SecurityTokenRefreshReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Fetch the sandbox instance
	box := &agentsv1alpha1.Sandbox{}
	err := r.Get(ctx, req.NamespacedName, box)
	if err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		klog.ErrorS(err, "get sandbox failed", "sandbox", req.NamespacedName)
		return reconcile.Result{}, err
	}

	// Re-check eligibility on the freshly-read object. The predicate only
	// guarantees an event was relevant *at enqueue time*; by the time the
	// reconciler picks it up the sandbox might have been released or deleted.
	if !isRefreshTarget(box) {
		klog.V(5).InfoS("sandbox is not a refresh target, skip", "sandbox", klog.KObj(box))
		return reconcile.Result{}, nil
	}

	// Only refresh the token of a sandbox whose runtime is actually serving. When
	// there is no serving runtime pod (paused, resuming, pending, recreate-upgrading,
	// ...) there is nothing to receive a refreshed token, so a refresh here would
	// fail at the propagate step and spin the workqueue with TokenRefreshFailed
	// noise while burning identity-provider issuance calls. An expired token is
	// harmless with no serving runtime because nothing is consuming it, so we defer
	// all refresh work until a serving runtime exists again: the
	// no-runtime->serving predicate transition re-enqueues the sandbox (and the
	// resume/recreate flow additionally rewrites the token-status annotation via
	// reinitSecurityToken), so the schedule re-arms itself without any timing
	// requeue here. We deliberately emit no event, since a sandbox with no serving
	// runtime generating zero token traffic is exactly the intended behaviour.
	//
	// Crucially the gate is token-INDEPENDENT (Phase + RuntimeInitialized, never
	// the aggregate Ready condition): a business container that consumes our token
	// can fail its readiness probe once the token expires, and gating on Ready
	// would then deadlock (expired token -> not Ready -> refresh deferred -> token
	// never refreshed). See hasServingRuntime for the full rationale.
	if !hasServingRuntime(box) {
		klog.V(5).InfoS("sandbox has no serving runtime, defer security token refresh until it is serving", "sandbox", klog.KObj(box))
		return reconcile.Result{}, nil
	}

	klog.V(5).InfoS("Began to process Sandbox for security token refresh", "sandbox", klog.KObj(box))

	raw := box.Annotations[identity.AgentKeyTokenRefreshStatus]
	status, err := decodeTokenRefreshStatus(raw)
	if err != nil {
		// A malformed annotation should not put us into a hot retry loop.
		// Surface it as an event and back off; an operator must clean it up.
		klog.ErrorS(err, "decode token-status annotation failed", "sandbox", klog.KObj(box), "raw", raw)
		r.Recorder.Eventf(box, corev1.EventTypeWarning, "TokenStatusDecodeFailed",
			"failed to decode %s annotation: %v", identity.AgentKeyTokenRefreshStatus, err)
		return ctrl.Result{RequeueAfter: refreshRetryAfter}, nil
	}
	// Annotation absent or decoded into an empty / partial object: there is
	// nothing to refresh yet. The empty-string AccessTokenExpiration case is
	// reachable when legacy annotation data (from before the upgrade that
	// removed UUID fallback) persists, or when a misconfigured provider does
	// not populate an expiration. We defend in depth here so a missing
	// expiration never spirals into a hot retry loop emitting
	// TokenExpirationInvalid events forever.
	if status == nil || status.AccessTokenExpiration == "" {
		klog.V(5).InfoS("token refresh status is empty, skip", "sandbox", klog.KObj(box), "raw", raw)
		return reconcile.Result{}, nil
	}

	expireAt, err := parseExpiration(status.AccessTokenExpiration)
	if err != nil {
		klog.ErrorS(err, "parse access token expiration failed", "sandbox", klog.KObj(box), "expiration", status.AccessTokenExpiration)
		r.Recorder.Eventf(box, corev1.EventTypeWarning, "TokenExpirationInvalid",
			"failed to parse accessTokenExpiration %q: %v", status.AccessTokenExpiration, err)
		return ctrl.Result{RequeueAfter: refreshRetryAfter}, nil
	}

	// Schedule semantics: rotate the token at refreshAt = expireAt - leadTime,
	// where leadTime is `refreshLeadTime ± jitter` (default 30m ±10%). Until
	// that moment, requeue with the remaining duration so the workqueue stays
	// idle for the bulk of the token's lifetime. Once we are inside the lead
	// window (refreshAt is not after now) we hand over to the refresher
	// unconditionally; the runtime should always see a fresh credential well
	// before T-0.
	now := r.now()
	leadTime := r.jitteredRefreshLeadTime()
	refreshAt := expireAt.Add(-leadTime)
	if refreshAt.After(now) {
		d := refreshAt.Sub(now)
		klog.V(5).InfoS("token not yet due for refresh", "sandbox", klog.KObj(box),
			"expireAt", expireAt, "refreshAt", refreshAt, "leadTime", leadTime, "requeueIn", d)
		return ctrl.Result{RequeueAfter: clampRequeue(d)}, nil
	}

	klog.InfoS("refreshing security token", "sandbox", klog.KObj(box),
		"expireAt", expireAt, "leadTime", leadTime, "overdueBy", now.Sub(refreshAt))
	tokenResp, err := r.refresher.Refresh(ctx, box)
	if err != nil {
		// We are already inside the lead window (or past expireAt). A failure
		// here must be retried promptly so that the runtime is not left with a
		// stale credential. We therefore requeue every refreshRetryAfter
		// (default 1m) instead of waiting for the next normal refreshAt; with
		// the default 30m lead window this gives the refresher ~30 attempts
		// before the token actually expires.
		//
		// NOTE: we deliberately return a nil error alongside RequeueAfter.
		// controller-runtime prioritises a non-nil reconcile error and applies
		// its own rate-limited exponential backoff, which would override the
		// fixed retryAfter we want here. Logging + the TokenRefreshFailed
		// event below already preserve full observability of the failure.
		klog.ErrorS(err, "refresh security token failed, will retry within lead window",
			"sandbox", klog.KObj(box), "retryAfter", refreshRetryAfter,
			"expireAt", expireAt, "timeToExpire", expireAt.Sub(now))
		r.Recorder.Eventf(box, corev1.EventTypeWarning, "TokenRefreshFailed",
			"failed to refresh security token (will retry in %s): %v", refreshRetryAfter, err)
		return ctrl.Result{RequeueAfter: refreshRetryAfter}, nil
	}

	r.Recorder.Eventf(box, corev1.EventTypeNormal, "TokenRefreshed",
		"security token refreshed, new expiration %s", tokenResp.AccessTokenExpiration)

	nextExpire, err := parseExpiration(tokenResp.AccessTokenExpiration)
	if err != nil {
		// A successful refresh that produced an unparsable expiration is
		// suspicious but not fatal; rely on the watch on the patched
		// annotation to re-enqueue the sandbox once its store reflects it.
		klog.ErrorS(err, "parse new access token expiration failed", "sandbox", klog.KObj(box),
			"expiration", tokenResp.AccessTokenExpiration)
		return reconcile.Result{}, nil
	}
	// Schedule the next refresh at (newExpiration - leadTime); this keeps the
	// invariant that every reconcile leaves the workqueue armed for the next
	// rotation, even if no further annotation update arrives.
	remaining := nextExpire.Sub(r.now())
	leadTime = r.jitteredRefreshLeadTime()
	next := remaining - leadTime
	if remaining > 0 && next <= 0 {
		// Token TTL shorter than configured lead time (e.g. a 10-minute
		// token while leadTime is 30 minutes). Refresh at the midpoint of
		// the token's lifetime so we rotate before expiry without spinning
		// the controller in a tight requeue loop.
		next = remaining / 2
	}
	next = clampRequeue(next)
	klog.InfoS("security token refresh succeeded", "sandbox", klog.KObj(box),
		"nextRefreshIn", next, "remaining", remaining, "leadTime", leadTime)
	return ctrl.Result{RequeueAfter: next}, nil
}

// jitteredRefreshLeadTime returns refreshLeadTime ± (refreshLeadTime * jitterRatio).
// A jitterRatio outside [0, 1) is clamped, and a zero ratio short-circuits to
// the raw lead time so unit tests can assert deterministic behaviour by
// setting it to 0.
func (r *SecurityTokenRefreshReconciler) jitteredRefreshLeadTime() time.Duration {
	if jitterRatio <= 0 {
		return refreshLeadTime
	}
	ratio := jitterRatio
	if ratio >= 1 {
		ratio = 0.99
	}
	// rand.Float64 ∈ [0,1) → centred to [-1,1) for symmetric jitter.
	delta := (r.randFloat()*2 - 1) * ratio
	jittered := time.Duration(float64(refreshLeadTime) * (1 + delta))
	if jittered <= 0 {
		return refreshLeadTime
	}
	return jittered
}

// parseExpiration parses an RFC3339 timestamp. An empty value is reported as
// an error so the caller surfaces a Kubernetes event instead of silently
// treating the token as already expired (which would cause an immediate
// refresh storm).
func parseExpiration(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("accessTokenExpiration is empty")
	}
	return time.Parse(time.RFC3339, s)
}

// clampRequeue ensures the RequeueAfter passed to controller-runtime is at
// least minRequeueAfter, so a near-zero or negative computed delay never
// degenerates into an immediate-retry loop.
func clampRequeue(d time.Duration) time.Duration {
	if d < minRequeueAfter {
		return minRequeueAfter
	}
	return d
}

// SetupWithManager sets up the controller with the Manager.
func (r *SecurityTokenRefreshReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentReconciles}).
		For(&agentsv1alpha1.Sandbox{}, builder.WithPredicates(needsRefreshPredicate())).
		Named(controllerName).
		Complete(r)
}

// DecodeTokenRefreshStatus parses a JSON string previously produced by EncodeTokenRefreshStatus.
// An empty input is treated as a non-error nil result so callers can short-circuit
// reconciliation with a simple nil-check when the annotation is not yet populated.
func decodeTokenRefreshStatus(raw string) (*identity.TokenRefreshStatus, error) {
	if raw == "" {
		return nil, nil
	}
	var status identity.TokenRefreshStatus
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		return nil, fmt.Errorf("failed to unmarshal token refresh status: %w", err)
	}
	return &status, nil
}
