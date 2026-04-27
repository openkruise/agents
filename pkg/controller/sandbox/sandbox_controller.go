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

package sandbox

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/sandbox/core"
	"github.com/openkruise/agents/pkg/discovery"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

func init() {
	flag.IntVar(&concurrentReconciles, "sandbox-workers", concurrentReconciles, "Max concurrent reconciles for Sandbox controller.")
}

var (
	concurrentReconciles  = 500
	sandboxControllerKind = agentsv1alpha1.GroupVersion.WithKind("Sandbox")
)

func Add(mgr manager.Manager) error {
	if !utilfeature.DefaultFeatureGate.Enabled(features.SandboxGate) || !discovery.DiscoverGVK(sandboxControllerKind) {
		return nil
	}
	rateLimiter := core.NewRateLimiter()
	err := (&SandboxReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		controls:    core.NewSandboxControl(mgr.GetClient(), mgr.GetEventRecorderFor("sandbox"), rateLimiter),
		rateLimiter: rateLimiter,
	}).SetupWithManager(mgr)
	if err != nil {
		return err
	}
	klog.Infof("Started SandboxReconciler successfully")
	return nil
}

// SandboxReconciler reconciles a Sandbox object
type SandboxReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	controls    map[string]core.SandboxControl
	rateLimiter *core.RateLimiter
}

// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxtemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=checkpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;update;patch
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete

//nolint:gocyclo // This function handles multiple reconciliation scenarios which require branching logic
func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (crl ctrl.Result, err error) {
	// fetch pod
	pod := &corev1.Pod{}
	err = r.Get(ctx, req.NamespacedName, pod)
	if client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, err
	} else if errors.IsNotFound(err) {
		pod = nil
	}

	// Fetch the sandbox instance
	box := &agentsv1alpha1.Sandbox{}
	err = r.Get(ctx, req.NamespacedName, box)
	if err != nil {
		if errors.IsNotFound(err) {
			box.Namespace = req.NamespacedName.Namespace
			box.Name = req.NamespacedName.Name
			core.ResourceVersionExpectations.Delete(box)
			core.ScaleExpectation.DeleteExpectations(utils.GetControllerKey(box))
			deleteSandboxMetrics(req.NamespacedName.Namespace, req.NamespacedName.Name)
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	// Record sandbox lifecycle metrics on every reconcile
	recordSandboxMetrics(box)

	if box.Spec.Template == nil && box.Spec.TemplateRef == nil {
		if !box.DeletionTimestamp.IsZero() {
			newStatus := box.Status.DeepCopy()
			args := core.EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus}
			return r.handleTerminating(ctx, args)
		}
		klog.InfoS("sandbox template is nil, and ignore", "sandbox", klog.KObj(box))
		return reconcile.Result{}, nil
	}

	klog.InfoS("Began to process Sandbox for reconcile", "sandbox", klog.KObj(box))
	if pod != nil {
		core.ScaleExpectation.ObserveScale(utils.GetControllerKey(box), expectations.Create, pod.Name)
	}
	if isSatisfied, unsatisfiedDuration, _ := core.ScaleExpectation.SatisfiedExpectations(utils.GetControllerKey(box)); !isSatisfied {
		if unsatisfiedDuration < expectations.ExpectationTimeout {
			klog.InfoS("Not satisfied ScaleExpectation for Sandbox, wait for cache event", "sandbox", klog.KObj(box))
			return reconcile.Result{RequeueAfter: expectations.ExpectationTimeout - unsatisfiedDuration}, nil
		}
		klog.InfoS("ScaleExpectation unsatisfied overtime for Sandbox, wait for cache event timeout", "timeout", unsatisfiedDuration)
		core.ScaleExpectation.DeleteExpectations(utils.GetControllerKey(box))
	}
	// If resourceVersion expectations have not satisfied yet, just skip this reconcile
	core.ResourceVersionExpectations.Observe(box)
	if isSatisfied, unsatisfiedDuration := core.ResourceVersionExpectations.IsSatisfied(box); !isSatisfied {
		if unsatisfiedDuration < expectations.ExpectationTimeout {
			klog.InfoS("Not satisfied resourceVersion for Sandbox, wait for cache event", "sandbox", klog.KObj(box))
			return reconcile.Result{RequeueAfter: expectations.ExpectationTimeout - unsatisfiedDuration}, nil
		}
		klog.InfoS("ResourceVersionExpectations unsatisfied overtime for Sandbox, wait for cache event timeout", "timeout", unsatisfiedDuration)
		core.ResourceVersionExpectations.Delete(box)
	}

	defer func() {
		if !utilfeature.DefaultFeatureGate.Enabled(features.SandboxCreatePodRateLimitGate) ||
			!core.IsHighPrioritySandbox(ctx, box) || err != nil {
			return
		}

		// At this point, the sandbox status may have changed, so we need to process it
		if inCreatingTrack := r.rateLimiter.UpdateRateLimiter(box); inCreatingTrack {
			requeueDuration := box.CreationTimestamp.Time.Add(time.Duration(core.MaxSandboxCreateDelay()) * time.Second).Sub(time.Now())
			crl = ctrl.Result{RequeueAfter: requeueDuration}
		}
	}()

	newStatus := box.Status.DeepCopy()
	if box.Annotations == nil {
		box.Annotations = map[string]string{}
	}

	// Process VolumeClaimTemplates for persistent data recovery during sleep/wake operations
	if err := r.ensureVolumeClaimTemplates(ctx, box); err != nil {
		klog.ErrorS(err, "failed to ensure volume claim templates", "sandbox", klog.KObj(box))
		return reconcile.Result{}, err
	}

	args := core.EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus}

	// ensure sandbox terminating
	if !box.DeletionTimestamp.IsZero() {
		return r.handleTerminating(ctx, args)
	}

	// if sandbox phase = Failed, Success
	if isSandboxCompletedPhase(box.Status.Phase) {
		return ctrl.Result{}, nil
	}

	// add finalizer
	if box, err = r.addSandboxFinalizerAndHash(ctx, box); err != nil {
		return reconcile.Result{}, err
	}

	// Check ShutdownTime and PauseTime
	now := metav1.Now()
	var requeueAfter time.Duration
	if box.Spec.ShutdownTime != nil && box.DeletionTimestamp == nil {
		if box.Spec.ShutdownTime.Before(&now) {
			klog.InfoS("sandbox shutdown time reached, will be deleted", "sandbox", klog.KObj(box), "shutdownTime", box.Spec.ShutdownTime)
			return ctrl.Result{}, r.Delete(ctx, box)
		}
		requeueAfter = box.Spec.ShutdownTime.Sub(now.Time)
	}
	if box.Spec.PauseTime != nil && !box.Spec.Paused {
		if box.Spec.PauseTime.Before(&now) {
			klog.InfoS("sandbox pause time reached, will be paused", "sandbox", klog.KObj(box))
			modified := box.DeepCopy()
			patch := client.MergeFrom(box)
			modified.Spec.Paused = true
			return ctrl.Result{}, r.Patch(ctx, modified, patch)
		}
		requeueAfter = min(requeueAfter, box.Spec.PauseTime.Sub(now.Time))
	}

	// calculate sandbox status
	var shouldRequeue bool
	newStatus, shouldRequeue = calculateStatus(args)
	if shouldRequeue {
		return reconcile.Result{RequeueAfter: requeueAfter}, r.updateSandboxStatus(ctx, *newStatus, box)
	}

	switch newStatus.Phase {
	case agentsv1alpha1.SandboxPending:
		requeueAfter, err = r.getControl(args.Pod).EnsureSandboxRunning(ctx, args)
	case agentsv1alpha1.SandboxRunning:
		err = r.getControl(args.Pod).EnsureSandboxUpdated(ctx, args)
	case agentsv1alpha1.SandboxPaused:
		err = r.getControl(args.Pod).EnsureSandboxPaused(ctx, args)
	case agentsv1alpha1.SandboxResuming:
		err = r.getControl(args.Pod).EnsureSandboxResumed(ctx, args)
	case agentsv1alpha1.SandboxUpgrading:
		err = r.getControl(args.Pod).EnsureSandboxUpgraded(ctx, args)
	default:
		klog.InfoS("sandbox status phase is invalid", "sandbox", klog.KObj(box), "phase", box.Status.Phase)
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	if err != nil {
		//todo, call updateSandboxStatus
		if newStatus.Phase == agentsv1alpha1.SandboxUpgrading {
			if retErr := r.updateSandboxStatus(ctx, *newStatus, box); retErr != nil {
				klog.ErrorS(retErr, "failed to persist upgrade status on error", "sandbox", klog.KObj(box))
			}
		}
		return reconcile.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, r.updateSandboxStatus(ctx, *newStatus, box)
}

func (r *SandboxReconciler) handleTerminating(ctx context.Context, args core.EnsureFuncArgs) (ctrl.Result, error) {
	pod, _, _ := args.Pod, args.Box, args.NewStatus
	return ctrl.Result{}, r.getControl(pod).EnsureSandboxTerminated(ctx, args)
}

func isSandboxCompletedPhase(phase agentsv1alpha1.SandboxPhase) bool {
	return phase == agentsv1alpha1.SandboxFailed || phase == agentsv1alpha1.SandboxSucceeded
}

func (r *SandboxReconciler) addSandboxFinalizerAndHash(ctx context.Context, box *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
	if !box.DeletionTimestamp.IsZero() || controllerutil.ContainsFinalizer(box, utils.SandboxFinalizer) {
		return box, nil
	}

	originObj := box.DeepCopy()
	patch := client.MergeFrom(box)
	controllerutil.AddFinalizer(originObj, utils.SandboxFinalizer)
	if originObj.Annotations == nil {
		originObj.Annotations = make(map[string]string)
	}
	_, hashImmutablePart := core.HashSandbox(box)
	originObj.Annotations[agentsv1alpha1.SandboxHashImmutablePart] = hashImmutablePart
	if err := client.IgnoreNotFound(r.Patch(ctx, originObj, patch)); err != nil {
		klog.ErrorS(err, "failed to patch sandbox finalizer and hash", "sandbox", klog.KObj(box))
		return nil, fmt.Errorf("failed to patch finalizer: %w", err)
	}
	klog.InfoS("patch sandbox hash annotations and finalizer success", "sandbox", klog.KObj(box))
	return originObj, nil
}

func (r *SandboxReconciler) updateSandboxStatus(ctx context.Context, newStatus agentsv1alpha1.SandboxStatus, box *agentsv1alpha1.Sandbox) error {
	if reflect.DeepEqual(box.Status, newStatus) || newStatus.Phase == agentsv1alpha1.SandboxPending {
		return nil
	}

	by, _ := json.Marshal(newStatus)
	patchStatus := fmt.Sprintf(`{"status":%s}`, string(by))
	rcvObject := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: box.Namespace, Name: box.Name}}
	err := client.IgnoreNotFound(r.Status().Patch(ctx, rcvObject, client.RawPatch(types.MergePatchType, []byte(patchStatus))))
	if err != nil {
		klog.ErrorS(err, "update sandbox status failed", "sandbox", klog.KObj(box), "patchStatus", patchStatus)
		return err
	}
	core.ResourceVersionExpectations.Expect(rcvObject)
	klog.InfoS("update sandbox status success", "sandbox", klog.KObj(box), "status", utils.DumpJson(newStatus))
	box.Status = newStatus
	// Update metrics after status change
	recordSandboxMetrics(box)
	return nil
}

func calculateStatus(args core.EnsureFuncArgs) (*agentsv1alpha1.SandboxStatus, bool) {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus

	hash, _ := core.HashSandbox(box)
	newStatus.ObservedGeneration = box.Generation
	newStatus.UpdateRevision = hash
	if newStatus.Phase == "" {
		newStatus.Phase = agentsv1alpha1.SandboxPending
	}

	switch newStatus.Phase {
	case agentsv1alpha1.SandboxPending:
		updateStatusIfPodCompleted(pod, newStatus)
		if isSandboxCompletedPhase(newStatus.Phase) {
			return newStatus, true
		}
	case agentsv1alpha1.SandboxRunning:
		// At this stage, if the Pod does not exist, it can only be that the Pod was deleted externally, and the sandbox should enter the Failed state
		if pod == nil || !pod.DeletionTimestamp.IsZero() {
			newStatus.Phase = agentsv1alpha1.SandboxFailed
			newStatus.Message = "Pod Not Found"
		} else {
			updateStatusIfPodCompleted(pod, newStatus)
		}
		if isSandboxCompletedPhase(newStatus.Phase) {
			return newStatus, true
		}

		// If it is paused, first set the sandbox to the Paused state.
		// To prevent loss of state information, the state immediately before Paused must currently be Running.
		if box.Spec.Paused {
			// The paused and resumed condition are exclusive
			utils.RemoveSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionResumed))
			newStatus.Phase = agentsv1alpha1.SandboxPaused
			// Check for upgrade: if template has changed (hash mismatch), transition to Upgrading phase
		} else if pod != nil && pod.Labels[agentsv1alpha1.PodLabelTemplateHash] != newStatus.UpdateRevision &&
			box.Spec.UpgradePolicy != nil && box.Spec.UpgradePolicy.Type == agentsv1alpha1.SandboxUpgradePolicyRecreate {
			klog.InfoS("Detected upgrade trigger", "sandbox", klog.KObj(box),
				"podRevision", pod.Labels[agentsv1alpha1.PodLabelTemplateHash],
				"sandboxRevision", newStatus.UpdateRevision)
			newStatus.Phase = agentsv1alpha1.SandboxUpgrading
		}

	case agentsv1alpha1.SandboxPaused:
		cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
		// sandbox will only enter the resuming state after successful paused
		if cond.Status == metav1.ConditionTrue && !box.Spec.Paused {
			// delete paused condition
			utils.RemoveSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
			newStatus.Phase = agentsv1alpha1.SandboxResuming
			rCond := metav1.Condition{
				Type:               string(agentsv1alpha1.SandboxConditionResumed),
				Status:             metav1.ConditionFalse,
				Reason:             agentsv1alpha1.SandboxResumeReasonCreatePod,
				LastTransitionTime: metav1.Now(),
			}
			utils.SetSandboxCondition(newStatus, rCond)
		} else if !box.Spec.Paused && cond.Status == metav1.ConditionFalse {
			klog.InfoS("sandbox pause not completed, cannot enter resume state temporarily", "sandbox", klog.KObj(box))
		}

	case agentsv1alpha1.SandboxUpgrading:
		// This indicates the podTemplate has changed again during an ongoing upgrade.
		// Therefore, the Upgrading condition reason must be reset to PreUpgrade to restart the upgrade lifecycle.
		if newStatus.UpdateRevision != box.Status.UpdateRevision {
			upgradeCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
			if upgradeCond != nil {
				klog.InfoS("podTemplate changed during upgrade, resetting condition Upgrading reason to PreUpgrade",
					"sandbox", klog.KObj(box),
					"previousReason", upgradeCond.Reason,
					"oldRevision", box.Status.UpdateRevision,
					"newRevision", newStatus.UpdateRevision)
				upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonPreUpgrade
				upgradeCond.Message = ""
				utils.SetSandboxCondition(newStatus, *upgradeCond)
			}
		}
	}
	return newStatus, false
}

func updateStatusIfPodCompleted(pod *corev1.Pod, newStatus *agentsv1alpha1.SandboxStatus) {
	if pod == nil || !pod.DeletionTimestamp.IsZero() {
		return
	}
	if pod.Status.Phase == corev1.PodSucceeded {
		newStatus.Phase = agentsv1alpha1.SandboxSucceeded
		newStatus.Message = "Pod status phase is Succeeded"
	} else if pod.Status.Phase == corev1.PodFailed {
		newStatus.Phase = agentsv1alpha1.SandboxFailed
		newStatus.Message = "Pod status phase is Failed"
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentReconciles}).
		For(&agentsv1alpha1.Sandbox{}).
		Named("sandbox-controller").
		Watches(&agentsv1alpha1.Sandbox{}, &handler.EnqueueRequestForObject{}).Watches(&corev1.Pod{}, &SandboxPodEventHandler{}).
		Complete(r)
}

// ensureVolumeClaimTemplates creates and ensures PVCs exist for persistent data recovery during sleep/wake operations
func (r *SandboxReconciler) ensureVolumeClaimTemplates(ctx context.Context, box *agentsv1alpha1.Sandbox) error {
	if len(box.Spec.VolumeClaimTemplates) == 0 {
		return nil
	}

	for _, template := range box.Spec.VolumeClaimTemplates {
		// Generate PVC name based on template name and sandbox name
		pvcName, err := core.GeneratePVCName(template.Name, box.Name)
		if err != nil {
			klog.ErrorS(err, "failed to generate PVC name", "sandbox", klog.KObj(box), "template", template.Name)
			return err
		}

		// Create PVC object based on the template
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pvcName,
				Namespace: box.Namespace,
			},
			Spec: template.Spec,
		}

		// Set the sandbox as the owner of the PVC to align their lifecycles
		if err = ctrl.SetControllerReference(box, pvc, r.Scheme); err != nil {
			klog.ErrorS(err, "failed to set sandbox as owner of PVC", "sandbox", klog.KObj(box), "pvc", pvcName)
			return err
		}

		// Check if PVC already exists
		existingPVC := &corev1.PersistentVolumeClaim{}
		err = r.Get(ctx, client.ObjectKey{Namespace: box.Namespace, Name: pvcName}, existingPVC)

		if err == nil {
			klog.InfoS("PVC already exists for persistent data recovery", "sandbox", klog.KObj(box), "pvc", pvcName)
			continue
		}

		if !errors.IsNotFound(err) {
			klog.ErrorS(err, "failed to get PVC", "sandbox", klog.KObj(box), "pvc", pvcName)
			return err
		}

		if err = r.Create(ctx, pvc); err == nil {
			klog.InfoS("created PVC for persistent data recovery", "sandbox", klog.KObj(box), "pvc", pvcName)
			continue
		}

		if !errors.IsAlreadyExists(err) {
			klog.ErrorS(err, "failed to create PVC", "sandbox", klog.KObj(box), "pvc", pvcName)
			return err
		}
		klog.InfoS("PVC already exists after create attempt", "sandbox", klog.KObj(box), "pvc", pvcName)
	}

	return nil
}
