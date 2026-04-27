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
	"github.com/openkruise/agents/pkg/utils/expectations"
)

const finalizerName = "agents.kruise.io/sandboxupdateops-protection"

var (
	concurrentReconciles        = 1
	controllerKind              = agentsv1alpha1.GroupVersion.WithKind("SandboxUpdateOps")
	ResourceVersionExpectations = expectations.NewResourceVersionExpectation()
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
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

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

	// 4. List matching sandboxes in the same namespace (only claimed ones)
	sandboxList := &agentsv1alpha1.SandboxList{}
	if err := r.List(ctx, sandboxList,
		client.InNamespace(ops.Namespace),
		client.MatchingLabelsSelector{Selector: selector},
		client.MatchingLabels{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True},
		client.UnsafeDisableDeepCopy); err != nil {
		return ctrl.Result{}, err
	}

	// 5. Classify sandbox states
	updated, failed, updating, candidates, requeueResult := r.classifySandboxes(sandboxList, ops)
	if requeueResult != nil {
		return *requeueResult, nil
	}

	total := updated + failed + updating + int32(len(candidates))
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

	// Only initiate new upgrades when in Updating phase, not paused, and there are candidates
	if newStatus.Phase == agentsv1alpha1.SandboxUpdateOpsUpdating && !ops.Spec.Paused && len(candidates) > 0 {
		maxConcurrent := calculateMaxUnavailable(ops.Spec.UpdateStrategy.MaxUnavailable, total)
		toUpgrade := int(maxConcurrent) - int(updating) - int(failed)
		if toUpgrade > len(candidates) {
			toUpgrade = len(candidates)
		}
		var patchErr error
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

		// 7. Update status first, then return patch error for requeue
		if err := r.updateStatus(ctx, ops, newStatus); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, patchErr
	}

	// 7. Update status
	return ctrl.Result{}, r.updateStatus(ctx, ops, newStatus)
}

func (r *Reconciler) classifySandboxes(sandboxList *agentsv1alpha1.SandboxList, ops *agentsv1alpha1.SandboxUpdateOps) (updated, failed, updating int32, candidates []*agentsv1alpha1.Sandbox, requeueResult *ctrl.Result) {
	for i := range sandboxList.Items {
		sbx := &sandboxList.Items[i]
		if !sbx.DeletionTimestamp.IsZero() || (sbx.Status.Phase != agentsv1alpha1.SandboxRunning &&
			sbx.Status.Phase != agentsv1alpha1.SandboxUpgrading) {
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

		category := classifySandbox(sbx, ops.Name)
		klog.InfoS("Classified sandbox", "sandbox", klog.KObj(sbx), "category", category, "ops", klog.KObj(ops))
		switch category {
		case sandboxUpdated:
			updated++
		case sandboxFailed:
			failed++
		case sandboxUpdating:
			updating++
		case sandboxCandidate:
			candidates = append(candidates, sbx)
		}
	}
	return
}

type sandboxUpdateState int

const (
	sandboxCandidate sandboxUpdateState = iota // not yet started
	sandboxUpdating                            // upgrading in progress
	sandboxUpdated                             // upgrade completed
	sandboxFailed                              // upgrade failed
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
	default:
		return "Unknown"
	}
}

func classifySandbox(sbx *agentsv1alpha1.Sandbox, opsName string) sandboxUpdateState {
	if sbx.Labels[agentsv1alpha1.LabelSandboxUpdateOps] != opsName {
		return sandboxCandidate
	}

	if sbx.Generation != sbx.Status.ObservedGeneration {
		return sandboxUpdating
	}

	// Only Upgrading Condition Reason == Succeeded means upgrade completed
	cond := findCondition(sbx.Status.Conditions, string(agentsv1alpha1.SandboxConditionUpgrading))
	if cond != nil && cond.Reason == agentsv1alpha1.SandboxUpgradingReasonSucceeded && cond.Status == metav1.ConditionTrue {
		return sandboxUpdated
	}

	// Explicit failure: check Upgrading condition for failed reasons
	if cond != nil && cond.Status == metav1.ConditionFalse &&
		(cond.Reason == agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed ||
			cond.Reason == agentsv1alpha1.SandboxUpgradingReasonPostUpgradeFailed ||
			cond.Reason == agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed) {
		return sandboxFailed
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
	return int32(val)
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

	// Remove ops label from each sandbox
	for i := range sandboxList.Items {
		sbx := &sandboxList.Items[i]
		if sbx.Labels[agentsv1alpha1.LabelSandboxUpdateOps] != ops.Name {
			continue
		}
		patchJSON := fmt.Sprintf(`{"metadata":{"labels":{"%s":null}}}`, agentsv1alpha1.LabelSandboxUpdateOps)
		rcvObject := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Namespace: sbx.Namespace, Name: sbx.Name},
		}
		if err := r.Patch(ctx, rcvObject, client.RawPatch(types.MergePatchType, []byte(patchJSON))); err != nil {
			klog.ErrorS(err, "Failed to remove ops label from sandbox",
				"sandbox", klog.KObj(sbx), "ops", klog.KObj(ops))
			return ctrl.Result{}, err
		}
		klog.InfoS("Removed ops label from sandbox",
			"sandbox", klog.KObj(sbx), "ops", klog.KObj(ops))
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(ops, finalizerName)
	if err := r.Update(ctx, ops); err != nil {
		return ctrl.Result{}, err
	}
	klog.InfoS("Finalizer removed, SandboxUpdateOps can be deleted",
		"ops", klog.KObj(ops))
	return ctrl.Result{}, nil
}

func (r *Reconciler) updateStatus(ctx context.Context, ops *agentsv1alpha1.SandboxUpdateOps, newStatus *agentsv1alpha1.SandboxUpdateOpsStatus) error {
	if reflect.DeepEqual(ops.Status, *newStatus) {
		return nil
	}
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
	}
	return err
}
