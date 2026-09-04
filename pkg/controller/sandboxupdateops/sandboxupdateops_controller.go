/*
Copyright 2025.

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

package sandboxupdateops

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"reflect"
	"sort"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/discovery"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

const finalizerName = "agents.kruise.io/sandboxupdateops-protection"

var (
	concurrentReconciles        = 1
	controllerKind              = agentsv1alpha1.GroupVersion.WithKind("SandboxUpdateOps")
	ResourceVersionExpectations = expectations.NewResourceVersionExpectation()
)

const (
	eventReasonSandboxUpdateOpsPhaseChanged = "SandboxUpdateOpsPhaseChanged"
)

func init() {
	flag.IntVar(&concurrentReconciles, "sandboxupdateops-workers", concurrentReconciles, "Max concurrent workers for SandboxUpdateOps controller.")
}

// Reconciler reconciles a SandboxUpdateOps object
type Reconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// Add creates a new SandboxUpdateOps controller and adds it to the Manager.
func Add(mgr manager.Manager) error {
	if !discovery.DiscoverGVK(controllerKind) {
		return nil
	}
	err := (&Reconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
	if err != nil {
		return err
	}
	klog.Infof("Started SandboxUpdateOpsReconciler successfully")
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("sandboxupdateops")
	return ctrl.NewControllerManagedBy(mgr).
		Named("sandboxupdateops-controller").
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentReconciles}).
		For(&agentsv1alpha1.SandboxUpdateOps{}).
		Watches(&agentsv1alpha1.Sandbox{}, &SandboxEventHandler{}).
		Complete(r)
}

// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxupdateops,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxupdateops/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes,verbs=get;list;watch;patch;update

// Reconcile handles SandboxUpdateOps reconciliation.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	klog.InfoS("Reconciling SandboxUpdateOps", "namespacedName", req.NamespacedName)

	// 1. Get SandboxUpdateOps
	ops := &agentsv1alpha1.SandboxUpdateOps{}
	if err := r.Get(ctx, req.NamespacedName, ops); err != nil {
		if apierrors.IsNotFound(err) {
			deleteSandboxUpdateOpsGaugeMetrics(req.Namespace, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	recordSandboxUpdateOpsMetrics(ops)

	// 2. Check if another SandboxUpdateOps in the same namespace is already Updating
	// TODO: This is a short-term solution to prevent concurrent ops in the same namespace.
	//  A more robust approach would be using a webhook to reject creation when an active ops exists,
	//  or implementing a queue/priority-based scheduling mechanism.
	opsList := &agentsv1alpha1.SandboxUpdateOpsList{}
	if err := r.List(ctx, opsList, client.InNamespace(ops.Namespace), client.UnsafeDisableDeepCopy); err != nil {
		return ctrl.Result{}, err
	}
	for i := range opsList.Items {
		other := &opsList.Items[i]
		if other.Name != ops.Name && other.Status.Phase == agentsv1alpha1.SandboxUpdateOpsUpdating {
			klog.InfoS("Another SandboxUpdateOps is already updating, skipping this one",
				"current", klog.KObj(ops), "active", klog.KObj(other))
			return ctrl.Result{}, nil
		}
	}

	// 3. Handle deletion: clean up sandbox labels
	if !ops.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, ops)
	}

	// 3. Ensure finalizer is present
	if !controllerutil.ContainsFinalizer(ops, finalizerName) {
		controllerutil.AddFinalizer(ops, finalizerName)
		if err := r.Update(ctx, ops); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 4. Terminal state, return directly
	if ops.Status.Phase == agentsv1alpha1.SandboxUpdateOpsCompleted ||
		ops.Status.Phase == agentsv1alpha1.SandboxUpdateOpsFailed {
		return ctrl.Result{}, nil
	}

	// 3. Convert selector to labels.Selector
	selector, err := metav1.LabelSelectorAsSelector(ops.Spec.Selector)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 4. List matching sandboxes in the same namespace (exclude sandboxes controlled by SandboxSet)
	sandboxList := &agentsv1alpha1.SandboxList{}
	if err := r.List(ctx, sandboxList,
		client.InNamespace(ops.Namespace),
		client.MatchingLabelsSelector{Selector: selector},
		client.UnsafeDisableDeepCopy); err != nil {
		return ctrl.Result{}, err
	}

	// 5. Classify sandbox states
	updated, failed, updating, candidates, resumeSucceedCandidates, requeueResult := r.classifySandboxes(ctx, sandboxList, ops)
	if requeueResult != nil {
		return *requeueResult, nil
	}

	total := updated + failed + updating + int32(len(candidates)) // #nosec G115 -- K8s object count
	newStatus := ops.Status.DeepCopy()
	newStatus.ObservedGeneration = ops.Generation
	newStatus.Replicas = total
	newStatus.UpdatedReplicas = updated
	newStatus.FailedReplicas = failed
	newStatus.UpdatingReplicas = updating

	// sort candidates by name for deterministic upgrade order
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Name < candidates[j].Name
	})

	// 6. Phase state machine
	switch {
	case ops.Status.Phase == "" || ops.Status.Phase == agentsv1alpha1.SandboxUpdateOpsPending:
		newStatus.Phase = agentsv1alpha1.SandboxUpdateOpsUpdating

	case updated+failed == total && len(candidates) == 0:
		if failed > 0 {
			newStatus.Phase = agentsv1alpha1.SandboxUpdateOpsFailed
		} else {
			newStatus.Phase = agentsv1alpha1.SandboxUpdateOpsCompleted
		}
	}

	// Process phase 2: patch template for ResumeSucceed sandboxes (no concurrency
	// limit). These sandboxes already resumed with the OLD template and just need
	// the template patch + annotation removal to proceed with the actual upgrade.
	var patchErr error
	if newStatus.Phase == agentsv1alpha1.SandboxUpdateOpsUpdating && !ops.Spec.Paused {
		patchErr = r.patchResumeSucceedSandboxes(ctx, resumeSucceedCandidates, ops)
	}

	// Only initiate new upgrades when in Updating phase, not paused, and there are candidates
	if newStatus.Phase == agentsv1alpha1.SandboxUpdateOpsUpdating && !ops.Spec.Paused && len(candidates) > 0 {
		maxConcurrent := calculateMaxUnavailable(ops.Spec.UpdateStrategy.MaxUnavailable, total)
		toUpgrade := int(maxConcurrent) - int(updating) - int(failed)
		if toUpgrade > len(candidates) {
			toUpgrade = len(candidates)
		}
		for i := 0; i < toUpgrade; i++ {
			klog.InfoS("Applying patch to sandbox", "sandbox", klog.KObj(candidates[i]), "ops", klog.KObj(ops))

			if err := r.applySandboxPatch(ctx, candidates[i], ops); err != nil {
				klog.ErrorS(err, "Failed to apply patch to sandbox",
					"sandbox", klog.KObj(candidates[i]), "ops", klog.KObj(ops))
				r.Recorder.Eventf(ops, v1.EventTypeWarning, "PatchFailed",
					"Failed to patch sandbox %s: %v", candidates[i].Name, err)
				if patchErr == nil {
					patchErr = err
				}
			} else {
				r.Recorder.Eventf(ops, v1.EventTypeNormal, "SandboxUpgrading",
					"Upgrading sandbox %s", candidates[i].Name)
			}
		}
	}

	// 7. Update status first, then return patch error for requeue
	if err := r.updateStatus(ctx, ops, newStatus); err != nil {
		return ctrl.Result{}, err
	}
	if patchErr != nil {
		return ctrl.Result{}, patchErr
	}
	return ctrl.Result{}, nil
}

// patchResumeSucceedSandboxes applies template patches (phase 2) to sandboxes
// that have already resumed with the old template. Returns the first error
// encountered, if any.
func (r *Reconciler) patchResumeSucceedSandboxes(ctx context.Context, candidates []*agentsv1alpha1.Sandbox, ops *agentsv1alpha1.SandboxUpdateOps) error {
	var patchErr error
	for _, sbx := range candidates {
		klog.InfoS("Applying template patch (phase 2)", "sandbox", klog.KObj(sbx), "ops", klog.KObj(ops))
		if err := r.applyTemplatePatch(ctx, sbx, ops); err != nil {
			klog.ErrorS(err, "Failed to apply template patch (phase 2)",
				"sandbox", klog.KObj(sbx), "ops", klog.KObj(ops))
			r.Recorder.Eventf(ops, v1.EventTypeWarning, "PatchFailed",
				"Failed to patch sandbox %s: %v", sbx.Name, err)
			if patchErr == nil {
				patchErr = err
			}
		} else {
			r.Recorder.Eventf(ops, v1.EventTypeNormal, "SandboxUpgrading",
				"Patching sandbox %s after resuming", sbx.Name)
		}
	}
	return patchErr
}

// isStateIncluded checks whether the given sandbox phase is among the states
// eligible as upgrade candidates. Upgrading is always implicitly included,
// because sandboxes already being upgraded by this ops must be tracked
// regardless of the StateFilter configuration. When StateFilter is nil or
// has no States, only Running is eligible by default — Paused sandboxes
// require an explicit StateFilter to avoid breaking existing installations.
func isStateIncluded(ops *agentsv1alpha1.SandboxUpdateOps, phase agentsv1alpha1.SandboxPhase) bool {
	if phase == agentsv1alpha1.SandboxUpgrading {
		return true
	}
	var states []agentsv1alpha1.SandboxPhase
	if ops.Spec.StateFilter != nil {
		states = ops.Spec.StateFilter.States
	}
	if len(states) == 0 {
		return phase == agentsv1alpha1.SandboxRunning
	}
	for _, s := range states {
		if s == phase {
			return true
		}
	}
	return false
}

func (r *Reconciler) classifySandboxes(ctx context.Context, sandboxList *agentsv1alpha1.SandboxList, ops *agentsv1alpha1.SandboxUpdateOps) (updated, failed, updating int32, candidates, resumeSucceedCandidates []*agentsv1alpha1.Sandbox, requeueResult *ctrl.Result) {
	for i := range sandboxList.Items {
		sbx := &sandboxList.Items[i]
		if !sbx.DeletionTimestamp.IsZero() {
			continue
		}
		// Terminal phases (Succeeded/Failed) are always excluded — these
		// sandboxes have completed their lifecycle and cannot be upgraded.
		if sbx.Status.Phase == agentsv1alpha1.SandboxSucceeded ||
			sbx.Status.Phase == agentsv1alpha1.SandboxFailed {
			continue
		}
		// The state filter only gates new candidates. A sandbox already claimed by
		// this ops must stay tracked regardless of its phase (e.g. Resuming after
		// a user un-pause mid two-phase upgrade), otherwise the ops could complete
		// while the sandbox is still pending its template patch.
		if sbx.Labels[agentsv1alpha1.LabelSandboxUpdateOps] != ops.Name &&
			!isStateIncluded(ops, sbx.Status.Phase) {
			continue
		}

		// Skip sandboxes controlled by SandboxSet (pool sandboxes, not intended for update)
		if utils.IsControlledBySandboxSet(sbx) {
			continue
		}

		// Check ResourceVersionExpectations: skip sandbox if cache is stale
		ResourceVersionExpectations.Observe(sbx)
		if isSatisfied, unsatisfiedDuration := ResourceVersionExpectations.IsSatisfied(sbx); !isSatisfied {
			if unsatisfiedDuration < expectations.ExpectationTimeout {
				klog.InfoS("Not satisfied resourceVersion for Sandbox in SandboxUpdateOps, wait for cache event",
					"sandbox", klog.KObj(sbx), "ops", klog.KObj(ops))
				requeueResult = &ctrl.Result{RequeueAfter: expectations.ExpectationTimeout - unsatisfiedDuration}
				return
			}
			klog.InfoS("ResourceVersionExpectations unsatisfied overtime for Sandbox in SandboxUpdateOps, wait for cache event timeout",
				"timeout", unsatisfiedDuration, "sandbox", klog.KObj(sbx), "ops", klog.KObj(ops))
			ResourceVersionExpectations.Delete(sbx)
		}

		category := r.classifySandbox(ctx, sbx, ops)
		klog.InfoS("Classified sandbox", "sandbox", klog.KObj(sbx), "category", category, "ops", klog.KObj(ops))

		switch category {
		case sandboxUpdated:
			r.syncSandboxUpgradeState(ctx, sbx, ops, sandboxUpdated)
			updated++
		case sandboxFailed:
			r.syncSandboxUpgradeState(ctx, sbx, ops, sandboxFailed)
			failed++
		case sandboxUpdating:
			r.syncSandboxUpgradeState(ctx, sbx, ops, sandboxUpdating)
			updating++
		case sandboxNoNeedUpdate:
			continue
		case sandboxCandidate:
			// upgrade-failed label is cleared during applySandboxPatch
			candidates = append(candidates, sbx)
		case sandboxResumeSucceed:
			r.syncSandboxUpgradeState(ctx, sbx, ops, sandboxResumeSucceed)
			updating++
			resumeSucceedCandidates = append(resumeSucceedCandidates, sbx)
		}
	}
	return
}

type sandboxUpdateState int

const (
	sandboxCandidate     sandboxUpdateState = iota // not yet started
	sandboxUpdating                                // upgrading in progress
	sandboxUpdated                                 // upgrade completed
	sandboxFailed                                  // upgrade failed
	sandboxNoNeedUpdate                            // template already matches patch, skip entirely
	sandboxResumeSucceed                           // resume succeeded, ready for template patch (phase 2)
)

func (s sandboxUpdateState) String() string {
	switch s {
	case sandboxCandidate:
		return "Candidate"
	case sandboxUpdating:
		return "Updating"
	case sandboxUpdated:
		return "Updated"
	case sandboxFailed:
		return "Failed"
	case sandboxNoNeedUpdate:
		return "NoNeedUpdate"
	case sandboxResumeSucceed:
		return "ResumeSucceed"
	default:
		return "Unknown"
	}
}

func (r *Reconciler) classifySandbox(ctx context.Context, sbx *agentsv1alpha1.Sandbox, ops *agentsv1alpha1.SandboxUpdateOps) sandboxUpdateState {
	otherOpsName := sbx.Labels[agentsv1alpha1.LabelSandboxUpdateOps]
	// If the sandbox has a label from another ops, check whether that ops
	// is still active. An active ops (Pending/Updating) owns the sandbox.
	if otherOpsName != "" && otherOpsName != ops.Name {
		otherOps := &agentsv1alpha1.SandboxUpdateOps{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: sbx.Namespace, Name: otherOpsName}, otherOps); err == nil &&
			(otherOps.Status.Phase == agentsv1alpha1.SandboxUpdateOpsPending ||
				otherOps.Status.Phase == agentsv1alpha1.SandboxUpdateOpsUpdating) {
			klog.InfoS("Sandbox is being updated by another active ops, skipping",
				"sandbox", klog.KObj(sbx), "otherOps", otherOpsName, "ops", klog.KObj(ops))
			return sandboxNoNeedUpdate
		}
	}
	// Sandbox has no ops label, or the previous ops is no longer active.
	// Treat it as a fresh candidate for the current ops.
	if otherOpsName != ops.Name {
		if isSandboxTemplateMatchPatch(sbx, ops) && sbx.Status.Phase != agentsv1alpha1.SandboxUpgrading &&
			sbx.Generation == sbx.Status.ObservedGeneration {
			return sandboxNoNeedUpdate
		}
		// Only Running, Upgrading, and explicitly included states are eligible
		// as new upgrade candidates.
		if !isStateIncluded(ops, sbx.Status.Phase) {
			return sandboxNoNeedUpdate
		}
		// Only a fully paused sandbox (Paused condition=True with
		// spec.Paused=true) can enter the two-phase upgrade. Transient
		// states — pause still in progress, or an un-pause already
		// requested — are skipped here; the sandbox will be picked up by a
		// later reconcile once it stabilizes.
		if sbx.Status.Phase == agentsv1alpha1.SandboxPaused {
			pausedCond := findCondition(sbx.Status.Conditions, string(agentsv1alpha1.SandboxConditionPaused))
			if pausedCond == nil || pausedCond.Status != metav1.ConditionTrue {
				return sandboxNoNeedUpdate
			}
			if !sbx.Spec.Paused {
				return sandboxNoNeedUpdate
			}
		}
		return sandboxCandidate
	}

	if sbx.Generation != sbx.Status.ObservedGeneration {
		return sandboxUpdating
	}

	// Terminal: upgrade completed.
	if isUpgradeSucceeded(sbx) {
		return sandboxUpdated
	}

	cond := findCondition(sbx.Status.Conditions, string(agentsv1alpha1.SandboxConditionUpgrading))
	if cond != nil {
		// Terminal: upgrade failed
		if cond.Status == metav1.ConditionFalse &&
			(cond.Reason == agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed ||
				cond.Reason == agentsv1alpha1.SandboxUpgradingReasonPostUpgradeFailed ||
				cond.Reason == agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed ||
				cond.Reason == agentsv1alpha1.SandboxUpgradingReasonCheckpointFailed) {
			return sandboxFailed
		}
		// Non-terminal: resume succeeded, waiting for phase 2 template patch.
		// Reconcile applies the patch and removes the resume trigger annotation,
		// which kicks off the actual pod-replacement upgrade.
		if cond.Reason == agentsv1alpha1.SandboxUpgradingReasonResumeSucceed {
			return sandboxResumeSucceed
		}
	}

	// Patched by ops but never entered upgrade flow — template already matched.
	// The controller has already observed this spec (Generation ==
	// ObservedGeneration), and no Upgrading condition was set, so the
	// template change was applied without the upgrade flow. NoNeedUpdate
	// is correct for all phases.
	if cond == nil && isSandboxTemplateMatchPatch(sbx, ops) {
		return sandboxNoNeedUpdate
	}

	// The sandbox is labeled by this updateops and Running, yet it never entered
	// the upgrade flow (no Upgrading condition) and its template still
	// differs from the patch target. This happens when phase 1 set only the
	// resume trigger and the sandbox was un-paused (spec.Paused=false)
	// before it reached ResumeSucceed, so the template patch was never
	// applied. Treat it as resume-succeeded so Reconcile applies the
	// pending template patch.
	if cond == nil && sbx.Status.Phase == agentsv1alpha1.SandboxRunning {
		return sandboxResumeSucceed
	}

	// All other states with ops label (Running / Upgrading / Pending etc.)
	// are considered upgrading-in-progress, occupying maxUnavailable quota
	return sandboxUpdating
}

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

// isUpgradeSucceeded reports whether the sandbox's Upgrading condition is in
// the terminal-success state (Reason=Succeeded, Status=True).
func isUpgradeSucceeded(sbx *agentsv1alpha1.Sandbox) bool {
	cond := findCondition(sbx.Status.Conditions, string(agentsv1alpha1.SandboxConditionUpgrading))
	return cond != nil &&
		cond.Reason == agentsv1alpha1.SandboxUpgradingReasonSucceeded &&
		cond.Status == metav1.ConditionTrue
}

// syncSandboxUpgradeState aligns the sandbox's upgrade-failed label and
// spec.upgradePolicy with its classification in a single merge patch:
//   - sandboxFailed adds the upgrade-failed label;
//   - sandboxUpdated removes the label and clears spec.upgradePolicy, so
//     later template changes fall back to the default behavior instead of
//     silently re-entering the pod-replacement upgrade lifecycle;
//   - any other state only removes a stale upgrade-failed label, keeping
//     the policy so the sandbox controller can finish the upgrade.
//
// It is best-effort: patch errors are logged but never block reconciliation;
// handleDeletion provides a second cleanup chance when the ops is deleted.
func (r *Reconciler) syncSandboxUpgradeState(ctx context.Context, sbx *agentsv1alpha1.Sandbox, ops *agentsv1alpha1.SandboxUpdateOps, state sandboxUpdateState) {
	hasFailedLabel := sbx.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed] == agentsv1alpha1.True
	wantFailedLabel := state == sandboxFailed
	clearPolicy := state == sandboxUpdated && sbx.Spec.UpgradePolicy != nil

	var labelPatch string
	if wantFailedLabel && !hasFailedLabel {
		labelPatch = fmt.Sprintf(`"%s":"%s"`, agentsv1alpha1.LabelSandboxUpgradeFailed, agentsv1alpha1.True)
	} else if !wantFailedLabel && hasFailedLabel {
		labelPatch = fmt.Sprintf(`"%s":null`, agentsv1alpha1.LabelSandboxUpgradeFailed)
	}
	if labelPatch == "" && !clearPolicy {
		return // already in desired state
	}

	patchJSON := "{"
	if labelPatch != "" {
		patchJSON += fmt.Sprintf(`"metadata":{"labels":{%s}}`, labelPatch)
		if clearPolicy {
			patchJSON += ","
		}
	}
	if clearPolicy {
		patchJSON += `"spec":{"upgradePolicy":null}`
	}
	patchJSON += "}"

	rcvObject := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: sbx.Namespace, Name: sbx.Name},
	}
	if err := r.Patch(ctx, rcvObject, client.RawPatch(types.MergePatchType, []byte(patchJSON))); err != nil {
		klog.ErrorS(err, "Failed to sync sandbox upgrade state",
			"sandbox", klog.KObj(sbx), "ops", klog.KObj(ops), "state", state)
		return
	}
	klog.InfoS("Synced sandbox upgrade state",
		"sandbox", klog.KObj(sbx), "ops", klog.KObj(ops), "state", state)
	ResourceVersionExpectations.Expect(rcvObject)
}

func calculateMaxUnavailable(maxUnavailable *intstrutil.IntOrString, total int32) int32 {
	if maxUnavailable == nil {
		return 1 // default: one at a time
	}
	val, err := intstrutil.GetScaledValueFromIntOrPercent(
		intstrutil.ValueOrDefault(maxUnavailable, intstrutil.FromInt32(1)),
		int(total), true)
	if err != nil || val <= 0 {
		return 1
	}
	return int32(val) // #nosec G115 -- val derived from int32 total
}

func (r *Reconciler) handleDeletion(ctx context.Context, ops *agentsv1alpha1.SandboxUpdateOps) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ops, finalizerName) {
		return ctrl.Result{}, nil
	}

	// List sandboxes with ops label
	sandboxList := &agentsv1alpha1.SandboxList{}
	if err := r.List(ctx, sandboxList,
		client.InNamespace(ops.Namespace),
		client.MatchingLabels{agentsv1alpha1.LabelSandboxUpdateOps: ops.Name},
		client.UnsafeDisableDeepCopy,
	); err != nil {
		return ctrl.Result{}, err
	}

	// Remove ops label and resume trigger annotation from each sandbox.
	// The annotation must be cleaned up so that a stale trigger does not
	// fire the two-phase upgrade flow again after the ops is gone.
	for i := range sandboxList.Items {
		sbx := &sandboxList.Items[i]
		if sbx.Labels[agentsv1alpha1.LabelSandboxUpdateOps] != ops.Name {
			continue
		}
		specPatch := ""
		// Fallback cleanup: also clear the upgrade policy of sandboxes whose
		// upgrade already succeeded, in case the per-sandbox cleanup in the
		// normal reconcile path was missed. Sandboxes still upgrading keep
		// their policy — the sandbox controller still reads it to drive the
		// upgrade state machine.
		if isUpgradeSucceeded(sbx) && sbx.Spec.UpgradePolicy != nil {
			specPatch = `,"spec":{"upgradePolicy":null}`
		}
		patchJSON := fmt.Sprintf(`{"metadata":{"labels":{"%s":null},"annotations":{"%s":null}}%s}`,
			agentsv1alpha1.LabelSandboxUpdateOps, agentsv1alpha1.AnnotationUpgradeResumeTrigger, specPatch)
		rcvObject := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Namespace: sbx.Namespace, Name: sbx.Name},
		}
		if err := r.Patch(ctx, rcvObject, client.RawPatch(types.MergePatchType, []byte(patchJSON))); err != nil {
			klog.ErrorS(err, "Failed to remove ops label and annotation from sandbox",
				"sandbox", klog.KObj(sbx), "ops", klog.KObj(ops))
			return ctrl.Result{}, err
		}
		klog.InfoS("Removed ops label and annotation from sandbox",
			"sandbox", klog.KObj(sbx), "ops", klog.KObj(ops))
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(ops, finalizerName)
	if err := r.Update(ctx, ops); err != nil {
		return ctrl.Result{}, err
	}
	deleteSandboxUpdateOpsMetrics(ops)
	klog.InfoS("Finalizer removed, SandboxUpdateOps can be deleted",
		"ops", klog.KObj(ops))
	return ctrl.Result{}, nil
}

func (r *Reconciler) updateStatus(ctx context.Context, ops *agentsv1alpha1.SandboxUpdateOps, newStatus *agentsv1alpha1.SandboxUpdateOpsStatus) error {
	if reflect.DeepEqual(ops.Status, *newStatus) {
		return nil
	}
	oldPhase := ops.Status.Phase

	by, _ := json.Marshal(newStatus)
	patchStatus := fmt.Sprintf(`{"status":%s}`, string(by))
	rcvObject := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Namespace: ops.Namespace, Name: ops.Name},
	}
	err := client.IgnoreNotFound(
		r.Status().Patch(ctx, rcvObject, client.RawPatch(types.MergePatchType, []byte(patchStatus))))
	if err != nil {
		klog.ErrorS(err, "Failed to update SandboxUpdateOps status", "ops", klog.KObj(ops), "patch", patchStatus)
	} else {
		klog.InfoS("Successfully updated SandboxUpdateOps status", "ops", klog.KObj(ops), "patch", patchStatus)
		recordSandboxUpdateOpsStatusMetrics(ops.Namespace, ops.Name, newStatus)
		recordSandboxUpdateOpsTerminalResult(ops, newStatus)
		r.recordPhaseEvent(ops, oldPhase, newStatus)
	}
	return err
}

func (r *Reconciler) recordPhaseEvent(ops *agentsv1alpha1.SandboxUpdateOps, oldPhase agentsv1alpha1.SandboxUpdateOpsPhase, newStatus *agentsv1alpha1.SandboxUpdateOpsStatus) {
	if r.Recorder == nil || oldPhase == newStatus.Phase || newStatus.Phase == "" {
		return
	}

	eventType := v1.EventTypeNormal
	if newStatus.Phase == agentsv1alpha1.SandboxUpdateOpsFailed {
		eventType = v1.EventTypeWarning
	}
	r.Recorder.Eventf(ops, eventType, eventReasonSandboxUpdateOpsPhaseChanged,
		"SandboxUpdateOps phase changed from %s to %s: total=%d, updated=%d, updating=%d, failed=%d",
		updateOpsPhaseForEvent(oldPhase), newStatus.Phase, newStatus.Replicas,
		newStatus.UpdatedReplicas, newStatus.UpdatingReplicas, newStatus.FailedReplicas)
}

func updateOpsPhaseForEvent(phase agentsv1alpha1.SandboxUpdateOpsPhase) string {
	if phase == "" {
		return "<empty>"
	}
	return string(phase)
}
