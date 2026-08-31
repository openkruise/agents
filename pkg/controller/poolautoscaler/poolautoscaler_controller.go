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

package poolautoscaler

import (
	"context"
	"flag"
	"fmt"
	"reflect"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/discovery"
	"github.com/openkruise/agents/pkg/features"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

func init() {
	flag.IntVar(&concurrentReconciles, "poolautoscaler-workers", concurrentReconciles,
		"Max concurrent workers for PoolAutoscaler controller.")
	flag.IntVar(&observationWindowSeconds, "poolautoscaler-observation-window-seconds", observationWindowSeconds,
		"Observation window in seconds for PoolAutoscaler capacity monitoring. "+
			"Samples within this window are averaged before making scaling decisions.")
	flag.IntVar(&samplingIntervalSeconds, "poolautoscaler-sampling-interval-seconds", samplingIntervalSeconds,
		"Sampling interval in seconds for PoolAutoscaler capacity monitoring. "+
			"Controls how frequently (available, statusReplicas) samples are collected.")
}

// defaultScaleUpStabilization and defaultScaleDownStabilization are the
// controller-side fallbacks used when a PoolAutoscaler omits
// StabilizationWindowSeconds. The webhook rejects explicit values below 60s,
// so these defaults align with the minimum admissible configuration.
const (
	defaultScaleUpStabilization   = 60 * time.Second
	defaultScaleDownStabilization = 300 * time.Second
)

var (
	concurrentReconciles = 1
	controllerKind       = agentsv1alpha1.GroupVersion.WithKind("PoolAutoscaler")
)

// scaleTargetRefNameIndex is the field index on PoolAutoscaler objects keyed by
// spec.scaleTargetRef.name. Registered in SetupWithManager and used by the
// SandboxSet event handler and the conflict detection in Reconcile.
const scaleTargetRefNameIndex = "spec.scaleTargetRef.name"

// conflictRequeueInterval is the periodic requeue interval for a PoolAutoscaler
// that lost the conflict resolution. The loser receives no further events while
// the winner scales the shared SandboxSet, so it must re-check periodically to
// take over once the winner disappears.
const conflictRequeueInterval = 30 * time.Second

// validateObservationParameters guards the observation window flags against
// values that would break the controller: a zero sampling interval causes an
// integer divide-by-zero panic and a zero requeueAfter busy loop; a negative
// window or a window smaller than the interval silently breaks the scale-down
// warm-up guard. Invalid values fall back to the defaults instead of returning
// an error, because Add runs in the shared controller-manager process and an
// error here would take down all other controllers.
func validateObservationParameters() {
	if samplingIntervalSeconds <= 0 || observationWindowSeconds <= 0 || observationWindowSeconds < samplingIntervalSeconds {
		klog.Warningf("Invalid PoolAutoscaler observation parameters (poolautoscaler-observation-window-seconds=%d, poolautoscaler-sampling-interval-seconds=%d); "+
			"falling back to defaults (poolautoscaler-observation-window-seconds=%d, poolautoscaler-sampling-interval-seconds=%d)",
			observationWindowSeconds, samplingIntervalSeconds,
			defaultObservationWindowSeconds, defaultSamplingIntervalSeconds)
		observationWindowSeconds = defaultObservationWindowSeconds
		samplingIntervalSeconds = defaultSamplingIntervalSeconds
	}
}

// Add creates a new PoolAutoscaler Controller and adds it to the Manager.
func Add(mgr manager.Manager, sbxMaxPendingTimeout time.Duration) error {
	if !utilfeature.DefaultFeatureGate.Enabled(features.PoolAutoscalerGate) || !discovery.DiscoverGVK(controllerKind) {
		return nil
	}
	validateObservationParameters()
	r := &Reconciler{
		Client:               mgr.GetClient(),
		recorder:             mgr.GetEventRecorderFor("pool-autoscaler-controller"),
		monitors:             make(map[types.NamespacedName]*capacityMonitor),
		sbxMaxPendingTimeout: sbxMaxPendingTimeout,
	}
	err := r.SetupWithManager(mgr)
	if err != nil {
		return err
	}
	klog.Infof("Started PoolAutoscalerReconciler successfully")
	return nil
}

// Reconciler reconciles a PoolAutoscaler object.
type Reconciler struct {
	client.Client
	recorder             record.EventRecorder
	mu                   sync.Mutex
	monitors             map[types.NamespacedName]*capacityMonitor
	sbxMaxPendingTimeout time.Duration
	scaleBlockedReported sync.Map
}

type scaleBlockedReport struct {
	poolAutoscalerUID types.UID
	sandboxSetUID     types.UID
	generation        int64
	conditionReason   string
}

func (r *Reconciler) SetupWithManager(mgr manager.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&agentsv1alpha1.PoolAutoscaler{},
		scaleTargetRefNameIndex,
		func(obj client.Object) []string {
			pa := obj.(*agentsv1alpha1.PoolAutoscaler)
			if pa.Spec.ScaleTargetRef.Name == "" {
				return nil
			}
			return []string{pa.Spec.ScaleTargetRef.Name}
		}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.PoolAutoscaler{}).
		Watches(&agentsv1alpha1.SandboxSet{}, handler.EnqueueRequestsFromMapFunc(r.sandboxSetToPoolAutoscaler)).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentReconciles}).
		Complete(r)
}

// sandboxSetToPoolAutoscaler maps a SandboxSet change to the PoolAutoscaler that targets it.
func (r *Reconciler) sandboxSetToPoolAutoscaler(ctx context.Context, obj client.Object) []ctrl.Request {
	sbs := obj.(*agentsv1alpha1.SandboxSet)
	paList := &agentsv1alpha1.PoolAutoscalerList{}
	if err := r.List(ctx, paList, client.InNamespace(sbs.Namespace),
		client.MatchingFields{scaleTargetRefNameIndex: sbs.Name}); err != nil {
		klog.FromContext(ctx).Error(err, "Failed to list PoolAutoscalers for SandboxSet",
			"sandboxset", klog.KObj(sbs))
		return nil
	}
	var requests []ctrl.Request
	for i := range paList.Items {
		requests = append(requests, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: paList.Items[i].Namespace, Name: paList.Items[i].Name},
		})
	}
	return requests
}

// +kubebuilder:rbac:groups=agents.kruise.io,resources=poolautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.kruise.io,resources=poolautoscalers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxsets,verbs=get;list;watch;update;patch

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := klog.FromContext(ctx).WithValues("poolautoscaler", req.NamespacedName)

	pa := &agentsv1alpha1.PoolAutoscaler{}
	if err := r.Get(ctx, req.NamespacedName, pa); err != nil {
		if errors.IsNotFound(err) {
			r.deleteMonitor(req.NamespacedName)
			r.scaleBlockedReported.Delete(req.NamespacedName.String())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Base copy for best-effort status patches on error paths.
	paOriginal := pa.DeepCopy()

	if pa.Spec.Suspend != nil && *pa.Spec.Suspend {
		logger.V(5).Info("PoolAutoscaler is suspended, skipping reconciliation")
		setCondition(pa, metav1.Condition{
			Type:               string(agentsv1alpha1.ScalingActive),
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: pa.Generation,
			Reason:             "Suspended",
			Message:            "autoscaler is suspended",
		})
		if err := r.updateStatus(ctx, pa, paOriginal, pa.Status.CurrentReplicas, pa.Status.DesiredReplicas, pa.Status.CurrentCapacity.Available, nil, true, false, false, "", "", false); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Read SandboxSet once for the entire reconciliation
	sbs, err := r.getSandboxSet(ctx, pa)
	if err != nil {
		setCondition(pa, metav1.Condition{
			Type:               string(agentsv1alpha1.ScalingActive),
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: pa.Generation,
			Reason:             "FailedGetTarget",
			Message:            fmt.Sprintf("failed to get SandboxSet: %v", err),
		})
		setCondition(pa, metav1.Condition{
			Type:               string(agentsv1alpha1.AbleToScale),
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: pa.Generation,
			Reason:             "FailedGetTarget",
			Message:            fmt.Sprintf("failed to get SandboxSet: %v", err),
		})
		r.recorder.Eventf(pa, "Warning", "FailedGetScale",
			"Failed to get SandboxSet %s/%s: %v", pa.Namespace, pa.Spec.ScaleTargetRef.Name, err)
		r.persistConditions(ctx, pa, paOriginal)
		return ctrl.Result{}, err
	}

	// Conflict detection: only the oldest PoolAutoscaler targeting a SandboxSet
	// may scale it. This complements the webhook, which cannot fully prevent
	// races or objects created while the webhook was unavailable.
	conflicting, winnerName, err := r.findConflictingAutoscaler(ctx, pa)
	if err != nil {
		return ctrl.Result{}, err
	}
	if conflicting {
		logger.Info("Conflicting PoolAutoscaler detected, skipping scaling", "winner", winnerName)
		setCondition(pa, metav1.Condition{
			Type:               string(agentsv1alpha1.ScalingActive),
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: pa.Generation,
			Reason:             "ConflictingAutoscaler",
			Message:            fmt.Sprintf("SandboxSet %q is already managed by older PoolAutoscaler %q", pa.Spec.ScaleTargetRef.Name, winnerName),
		})
		r.persistConditions(ctx, pa, paOriginal)
		// Requeue periodically: the loser receives no events while the winner
		// scales the shared SandboxSet, so it must re-check to take over once
		// the winner is deleted.
		return ctrl.Result{RequeueAfter: conflictRequeueInterval}, nil
	}

	setCondition(pa, metav1.Condition{
		Type:               string(agentsv1alpha1.AbleToScale),
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: pa.Generation,
		Reason:             "ReadyToScale",
		Message:            "the autoscaler can access the target resource",
	})

	specReplicas := sbs.Spec.Replicas
	statusReplicas := sbs.Status.Replicas
	statusAvailable := sbs.Status.AvailableReplicas

	avgAvailable, avgReplicas, _ := r.observeAndAggregate(ctx, pa, statusAvailable, statusReplicas)

	// Retrieve the monitor early — needed for the warm-up check and requeue logic.
	key := types.NamespacedName{Namespace: pa.Namespace, Name: pa.Name}
	monitor := r.getOrCreateMonitorFor(key, pa.UID, pa.Spec.ScaleTargetRef.Name)

	result, err := r.computeDesiredReplicas(ctx, pa, specReplicas, avgReplicas, avgAvailable)
	if err != nil {
		r.recorder.Eventf(pa, "Warning", "FailedComputeReplicas",
			"Failed to compute desired replicas: %v", err)
		return ctrl.Result{}, err
	}

	// Warm-up guard: capacity path requires the observation window to be
	// sufficiently populated before making scaling decisions. Cron-triggered
	// scaling bypasses this check since it represents explicit user intent.
	warmingUp := result.source != sourceCron && !monitor.windowIsWarm()
	if warmingUp {
		result.desiredReplicas = specReplicas
		result.reason = "observation window not yet warm enough for capacity scaling"
		result.limitReason = ""
	}

	// Note: clampToBounds is always applied even during warm-up to enforce hard
	// safety bounds (minReplicas/maxReplicas). If the current spec is outside
	// [min, max] (e.g. due to a manual edit), the clamp will correct it regardless
	// of the warm-up state.

	// Determine ScalingLimited from the capacity path's limitReason first;
	// fall back to clampToBounds (for cron path safety net) when the capacity
	// path did not report a limit.
	var desiredReplicas int32
	var limited bool
	var limitReason string
	if result.limitReason != "" {
		desiredReplicas = result.desiredReplicas
		limited = true
		limitReason = result.limitReason
	} else {
		desiredReplicas, limited, limitReason = r.clampToBounds(pa, result.desiredReplicas)
	}
	reason := result.reason

	// Cron-triggered scaling bypasses the stabilization window because it
	// represents explicit user intent for a specific replica count at a specific time.
	var cooldownRemaining time.Duration
	if result.source != sourceCron {
		var blocked bool
		desiredReplicas, blocked, cooldownRemaining = r.applyStabilizationWindow(pa, specReplicas, desiredReplicas)
		if blocked {
			// The bound-limited value was discarded in favor of the current spec.
			limited, limitReason = false, ""
			reason = fmt.Sprintf("scaling blocked by stabilization window (cooldown remaining: %s)", cooldownRemaining.Round(time.Second))
			r.recorder.Eventf(pa, "Normal", "ScaleBlocked",
				"Scaling %s/%s blocked by stabilization window (cooldown remaining: %s)",
				pa.Namespace, pa.Spec.ScaleTargetRef.Name, cooldownRemaining.Round(time.Second))
		}
	}

	// Only an explicit ScalingLimited=True condition from SandboxSet blocks
	// further scale-up. Missing, stale, or Unknown conditions fail open so
	// upgrades and first-time reconciles do not stall scale-ups.
	allowed, gateReason := sandboxSetAllowsScaleUp(sbs)
	if allowed {
		r.scaleBlockedReported.Delete(client.ObjectKeyFromObject(pa).String())
	} else if desiredReplicas > specReplicas {
		desiredReplicas = specReplicas
		limited, limitReason = false, ""
		reason = gateReason
		klog.FromContext(ctx).Info("scale-up suppressed by SandboxSet startup budget",
			"sandboxSet", sbs.Name, "reason", gateReason)
		r.reportScaleBlocked(pa, sbs, gateReason)
	}

	// Compare against spec (what we previously told SandboxSet), not status
	if desiredReplicas != specReplicas {
		if err := r.doScale(ctx, sbs, desiredReplicas); err != nil {
			setCondition(pa, metav1.Condition{
				Type:               string(agentsv1alpha1.AbleToScale),
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				ObservedGeneration: pa.Generation,
				Reason:             "FailedScale",
				Message:            fmt.Sprintf("failed to scale: %v", err),
			})
			r.recorder.Eventf(pa, "Warning", "FailedScale",
				"Failed to scale %s/%s from %d to %d: %v",
				pa.Namespace, pa.Spec.ScaleTargetRef.Name, specReplicas, desiredReplicas, err)
			r.persistConditions(ctx, pa, paOriginal)
			return ctrl.Result{}, err
		}
		action := "ScaledUp"
		if desiredReplicas < specReplicas {
			action = "ScaledDown"
		}
		r.recorder.Eventf(pa, "Normal", action,
			"Scaled %s/%s from %d to %d: %s",
			pa.Namespace, pa.Spec.ScaleTargetRef.Name, specReplicas, desiredReplicas, reason)

		// Record scale action for cooldown (does NOT clear observation window samples)
		r.recordScaleAction(types.NamespacedName{Namespace: pa.Namespace, Name: pa.Name}, desiredReplicas > specReplicas)
	}

	scaled := (desiredReplicas != specReplicas)
	if err := r.updateStatus(ctx, pa, paOriginal, statusReplicas, desiredReplicas, statusAvailable, result.appliedCronPolicies, false, scaled, limited, limitReason, reason, warmingUp); err != nil {
		return ctrl.Result{}, err
	}

	// Requeue aligned with last sample time so that observeAndAggregate
	// collects a new sample on each reconcile pass.
	nextDue := monitor.getLastSampleAt().Add(time.Duration(samplingIntervalSeconds) * time.Second)
	requeueAfter := time.Until(nextDue)
	if requeueAfter <= 0 {
		requeueAfter = time.Duration(samplingIntervalSeconds) * time.Second
	}
	// When blocked by the stabilization window, wake up as soon as the cooldown
	// expires if that happens before the next sampling slot.
	if cooldownRemaining > 0 && cooldownRemaining < requeueAfter {
		requeueAfter = cooldownRemaining
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *Reconciler) reportScaleBlocked(pa *agentsv1alpha1.PoolAutoscaler, sbs *agentsv1alpha1.SandboxSet, reason string) {
	condition := apiMeta.FindStatusCondition(sbs.Status.Conditions, string(agentsv1alpha1.SandboxSetConditionScalingLimited))
	if condition == nil {
		return
	}
	report := scaleBlockedReport{
		poolAutoscalerUID: pa.UID,
		sandboxSetUID:     sbs.UID,
		generation:        sbs.Generation,
		conditionReason:   condition.Reason,
	}
	key := client.ObjectKeyFromObject(pa).String()
	if previous, ok := r.scaleBlockedReported.Load(key); ok && previous == report {
		return
	}
	r.scaleBlockedReported.Store(key, report)
	r.recorder.Eventf(pa, corev1.EventTypeNormal, "ScaleBlocked",
		"Scaling %s/%s blocked: %s", pa.Namespace, pa.Spec.ScaleTargetRef.Name, reason)
}

// sandboxSetAllowsScaleUp reports whether SandboxSet is currently allowing
// further scale-up. The gate is fail-open: only an explicit
// ScalingLimited=True condition observed against the current generation blocks
// scaling. A missing, stale, or Unknown condition is treated as no signal and
// leaves scale-up untouched.
func sandboxSetAllowsScaleUp(sbs *agentsv1alpha1.SandboxSet) (bool, string) {
	condition := apiMeta.FindStatusCondition(sbs.Status.Conditions, string(agentsv1alpha1.SandboxSetConditionScalingLimited))
	if condition == nil {
		return true, ""
	}
	if condition.Status != metav1.ConditionTrue {
		return true, ""
	}
	if condition.ObservedGeneration != sbs.Generation || sbs.Status.ObservedGeneration < sbs.Generation {
		// The controller reports the budget as exhausted, but the report is
		// against an older generation. Treat as no current signal and let
		// scale-up proceed until the SandboxSet controller catches up.
		return true, ""
	}
	return false, fmt.Sprintf("scaling blocked by SandboxSet ScalingLimited condition (reason=%s)", condition.Reason)
}

// findConflictingAutoscaler checks whether another PoolAutoscaler in the same
// namespace targets the same SandboxSet and has precedence over this one.
// The oldest PoolAutoscaler wins (earliest CreationTimestamp, ties broken by
// name ordering). Returns the winner's name when the given PA loses.
func (r *Reconciler) findConflictingAutoscaler(ctx context.Context, pa *agentsv1alpha1.PoolAutoscaler) (bool, string, error) {
	paList := &agentsv1alpha1.PoolAutoscalerList{}
	if err := r.List(ctx, paList, client.InNamespace(pa.Namespace),
		client.MatchingFields{scaleTargetRefNameIndex: pa.Spec.ScaleTargetRef.Name}); err != nil {
		return false, "", err
	}
	if len(paList.Items) <= 1 {
		return false, "", nil
	}
	winner := &paList.Items[0]
	for i := 1; i < len(paList.Items); i++ {
		candidate := &paList.Items[i]
		if candidate.CreationTimestamp.Before(&winner.CreationTimestamp) ||
			(candidate.CreationTimestamp.Equal(&winner.CreationTimestamp) && candidate.Name < winner.Name) {
			winner = candidate
		}
	}
	if winner.Name == pa.Name {
		return false, "", nil
	}
	return true, winner.Name, nil
}

// persistConditions best-effort patches the status so that condition changes
// on error paths remain visible on the object. Failures are logged and never
// override the primary error.
func (r *Reconciler) persistConditions(ctx context.Context, pa, paOriginal *agentsv1alpha1.PoolAutoscaler) {
	if reflect.DeepEqual(pa.Status, paOriginal.Status) {
		return
	}
	if err := r.Status().Patch(ctx, pa, client.MergeFrom(paOriginal)); err != nil {
		klog.FromContext(ctx).Error(err, "Failed to persist PoolAutoscaler status conditions",
			"poolautoscaler", klog.KObj(pa))
	}
}

// getSandboxSet fetches the target SandboxSet.
func (r *Reconciler) getSandboxSet(ctx context.Context, pa *agentsv1alpha1.PoolAutoscaler) (*agentsv1alpha1.SandboxSet, error) {
	sbs := &agentsv1alpha1.SandboxSet{}
	key := types.NamespacedName{
		Namespace: pa.Namespace,
		Name:      pa.Spec.ScaleTargetRef.Name,
	}
	if err := r.Get(ctx, key, sbs); err != nil {
		return nil, err
	}
	return sbs, nil
}

// clampToBounds enforces minReplicas and maxReplicas constraints. It reports
// whether the desired value was limited by a bound and which bound applied
// ("TooFewReplicas" for the minimum, "TooManyReplicas" for the maximum).
//
// The capacity path already clamps its desired value to [minReplicas,
// maxReplicas] inside computeDesiredReplicas and reports the boundary via its
// reason, so this function only acts as a safety net for paths that do not
// clamp themselves (e.g. cron-triggered targets).
func (r *Reconciler) clampToBounds(pa *agentsv1alpha1.PoolAutoscaler, desired int32) (int32, bool, string) {
	if desired < pa.Spec.MinReplicas {
		return pa.Spec.MinReplicas, true, "TooFewReplicas"
	}
	if desired > pa.Spec.MaxReplicas {
		return pa.Spec.MaxReplicas, true, "TooManyReplicas"
	}
	return desired, false, ""
}

// doScale patches the SandboxSet spec.replicas.
func (r *Reconciler) doScale(ctx context.Context, sbs *agentsv1alpha1.SandboxSet, desiredReplicas int32) error {
	patch := client.MergeFrom(sbs.DeepCopy())
	sbs.Spec.Replicas = desiredReplicas
	return r.Patch(ctx, sbs, patch)
}

// updateStatus updates the PoolAutoscaler status fields.
//
// paOriginal is the object as fetched at the start of reconciliation, before
// any status mutation (including conditions set by the caller, e.g. the
// Suspended condition). It is used as the merge-patch base so that ALL status
// changes made during this reconciliation — not just those inside this
// function — are included in the patch and actually persisted.
func (r *Reconciler) updateStatus(ctx context.Context, pa, paOriginal *agentsv1alpha1.PoolAutoscaler, currentReplicas, desiredReplicas, available int32, appliedCronPolicies []agentsv1alpha1.CronScalingPolicyStatus, suspended bool, scaled bool, limited bool, limitReason, limitMessage string, warmingUp bool) error {
	pa.Status.ObservedGeneration = &pa.Generation
	pa.Status.CurrentReplicas = currentReplicas
	pa.Status.DesiredReplicas = desiredReplicas
	pa.Status.Suspended = suspended
	pa.Status.CurrentCapacity = agentsv1alpha1.CapacityStatus{Available: available}
	// Replace AppliedCronPolicies unconditionally when cron policies are
	// configured (an empty list clears stale entries); clear it entirely when
	// the spec no longer has cron policies.
	if len(pa.Spec.CronPolicies) > 0 {
		pa.Status.AppliedCronPolicies = appliedCronPolicies
	} else {
		pa.Status.AppliedCronPolicies = nil
	}

	if scaled {
		now := metav1.Now()
		pa.Status.LastScaleTime = &now
	}

	// When suspended, the caller already set the correct ScalingActive
	// condition (False/Suspended) and the target was never read, so the
	// generic conditions — which assert ScalingActive=True and
	// AbleToScale=True — must not overwrite it. ScalingLimited needs no
	// refresh either while scaling is halted.
	if !suspended {
		r.setConditions(pa, desiredReplicas, limited, limitReason, limitMessage, warmingUp)
	}

	if reflect.DeepEqual(pa.Status, paOriginal.Status) {
		return nil
	}

	patch := client.MergeFrom(paOriginal)
	return r.Status().Patch(ctx, pa, patch)
}

// setConditions updates the conditions on the PoolAutoscaler.
func (r *Reconciler) setConditions(pa *agentsv1alpha1.PoolAutoscaler, desiredReplicas int32, limited bool, limitReason, limitMessage string, warmingUp bool) {
	now := metav1.Now()

	if warmingUp {
		// While the observation window is filling up the capacity policy makes no
		// scaling decision at all, so ScalingActive must report False — the same
		// polarity used for the suspended state. Reporting True here would keep
		// every alert keyed on "ScalingActive != True" silent precisely while
		// scaling is frozen, and would contradict the reason in kubectl output.
		setCondition(pa, metav1.Condition{
			Type:               string(agentsv1alpha1.ScalingActive),
			Status:             metav1.ConditionFalse,
			LastTransitionTime: now,
			ObservedGeneration: pa.Generation,
			Reason:             "InsufficientObservationWindow",
			Message:            "the observation window has not collected enough samples to make a capacity scaling decision",
		})
	} else {
		setCondition(pa, metav1.Condition{
			Type:               string(agentsv1alpha1.ScalingActive),
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
			ObservedGeneration: pa.Generation,
			Reason:             "ValidPolicy",
			Message:            "the autoscaler is able to scale",
		})
	}

	setCondition(pa, metav1.Condition{
		Type:               string(agentsv1alpha1.AbleToScale),
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		ObservedGeneration: pa.Generation,
		Reason:             "ReadyToScale",
		Message:            "the autoscaler can access the target resource",
	})

	if limited {
		msg := limitMessage
		if msg == "" {
			bound := "maximum"
			if limitReason == "TooFewReplicas" {
				bound = "minimum"
			}
			msg = fmt.Sprintf("the desired replica count is limited to the %s %d", bound, desiredReplicas)
		}
		setCondition(pa, metav1.Condition{
			Type:               string(agentsv1alpha1.ScalingLimited),
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
			ObservedGeneration: pa.Generation,
			Reason:             limitReason,
			Message:            msg,
		})
	} else {
		setCondition(pa, metav1.Condition{
			Type:               string(agentsv1alpha1.ScalingLimited),
			Status:             metav1.ConditionFalse,
			LastTransitionTime: now,
			ObservedGeneration: pa.Generation,
			Reason:             "DesiredWithinRange",
			Message:            "the desired count is within the acceptable range",
		})
	}
}

// setCondition updates or appends a condition in the PoolAutoscaler status.
func setCondition(pa *agentsv1alpha1.PoolAutoscaler, condition metav1.Condition) {
	for i, c := range pa.Status.Conditions {
		if c.Type == condition.Type {
			if c.Status != condition.Status {
				pa.Status.Conditions[i] = condition
			} else if c.Reason != condition.Reason || c.Message != condition.Message {
				// Status unchanged but reason/message differs — update without bumping LastTransitionTime
				pa.Status.Conditions[i].Reason = condition.Reason
				pa.Status.Conditions[i].Message = condition.Message
				pa.Status.Conditions[i].ObservedGeneration = condition.ObservedGeneration
			}
			return
		}
	}
	pa.Status.Conditions = append(pa.Status.Conditions, condition)
}
