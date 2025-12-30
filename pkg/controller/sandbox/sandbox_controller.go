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
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/sandbox/core"
	"github.com/openkruise/agents/pkg/discovery"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

func init() {
	flag.IntVar(&concurrentReconciles, "sandbox-workers", concurrentReconciles, "Max concurrent workers for Sandbox controller.")
}

var (
	concurrentReconciles  = 500
	sandboxControllerKind = agentsv1alpha1.GroupVersion.WithKind("Sandbox")
)

func Add(mgr manager.Manager) error {
	if !utilfeature.DefaultFeatureGate.Enabled(features.SandboxGate) || !discovery.DiscoverGVK(sandboxControllerKind) {
		return nil
	}
	err := (&SandboxReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		controls: core.NewSandboxControl(mgr.GetClient(), mgr.GetEventRecorderFor("sandbox")),
	}).SetupWithManager(mgr)
	if err != nil {
		return err
	}
	klog.Infof("start SandboxReconciler success")
	return nil
}

// SandboxReconciler reconciles a Sandbox object
type SandboxReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	controls map[string]core.SandboxControl
}

// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;update;patch
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete

func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Fetch the sandbox instance
	box := &agentsv1alpha1.Sandbox{}
	err := r.Get(ctx, req.NamespacedName, box)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))

	if box.Spec.Template == nil {
		logger.Info("sandbox template is nil, and ignore")
		return reconcile.Result{}, nil
	}

	// If resourceVersion expectations have not satisfied yet, just skip this reconcile
	core.ResourceVersionExpectations.Observe(box)
	if isSatisfied, unsatisfiedDuration := core.ResourceVersionExpectations.IsSatisfied(box); !isSatisfied {
		if unsatisfiedDuration < expectations.ExpectationTimeout {
			logger.Info("Not satisfied resourceVersion for Sandbox, wait for cache event")
			return reconcile.Result{RequeueAfter: expectations.ExpectationTimeout - unsatisfiedDuration}, nil
		}
		klog.InfoS("Expectation unsatisfied overtime for Sandbox, wait for cache event timeout", "timeout", unsatisfiedDuration)
		core.ResourceVersionExpectations.Delete(box)
	}

	logger.V(consts.DebugLogLevel).Info("Began to process Sandbox for reconcile")
	newStatus := box.Status.DeepCopy()
	if box.Annotations == nil {
		box.Annotations = map[string]string{}
	}

	// Process VolumeClaimTemplates for persistent data recovery during sleep/wake operations
	if err := r.ensureVolumeClaimTemplates(ctx, box); err != nil {
		logger.Error(err, "failed to ensure volume claim templates")
		return reconcile.Result{}, err
	}

	// fetch pod
	pod := &corev1.Pod{}
	err = r.Get(ctx, client.ObjectKey{Namespace: box.Namespace, Name: box.Name}, pod)
	if client.IgnoreNotFound(err) != nil {
		logger.Error(err, "Get Pod failed")
		return reconcile.Result{}, err
	} else if errors.IsNotFound(err) {
		pod = nil
	}
	args := core.EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus}

	// ensure sandbox terminating
	if !box.DeletionTimestamp.IsZero() {
		return r.handleTerminating(ctx, args)
	}

	// if sandbox phase = Failed, Success
	if r.isCompletedPhase(box.Status.Phase) {
		return ctrl.Result{}, nil
	}

	// add finalizer
	if box, err = r.ensureFinalizer(ctx, box); err != nil {
		return reconcile.Result{}, err
	}

	// Check ShutdownTime and PauseTime
	now := metav1.Now()
	var requeueAfter time.Duration
	if box.Spec.ShutdownTime != nil && box.DeletionTimestamp == nil {
		if box.Spec.ShutdownTime.Before(&now) {
			logger.Info("sandbox shutdown time reached, will be deleted")
			return ctrl.Result{}, r.Delete(ctx, box)
		}
		requeueAfter = box.Spec.ShutdownTime.Sub(now.Time)
	}
	if box.Spec.PauseTime != nil && !box.Spec.Paused {
		if box.Spec.PauseTime.Before(&now) {
			logger.Info("sandbox pause time reached, will be paused")
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
		if box, err = r.patchSandboxAnnotationHash(ctx, box); err != nil {
			return ctrl.Result{}, err
		}
		err = r.getControl(args.Pod).EnsureSandboxRunning(ctx, args)
	case agentsv1alpha1.SandboxRunning:
		err = r.getControl(args.Pod).EnsureSandboxUpdated(ctx, args)
	case agentsv1alpha1.SandboxPaused:
		err = r.getControl(args.Pod).EnsureSandboxPaused(ctx, args)
	case agentsv1alpha1.SandboxResuming:
		err = r.getControl(args.Pod).EnsureSandboxResumed(ctx, args)
	default:
		logger.Info("sandbox status phase is invalid", "phase", box.Status.Phase)
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, r.updateSandboxStatus(ctx, *newStatus, box)
}

func (r *SandboxReconciler) handleTerminating(ctx context.Context, args core.EnsureFuncArgs) (ctrl.Result, error) {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	newStatus.Phase = agentsv1alpha1.SandboxTerminating
	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady))
	if cond != nil && cond.Status == metav1.ConditionTrue {
		cond.Status = metav1.ConditionFalse
		cond.LastTransitionTime = metav1.Now()
		utils.SetSandboxCondition(newStatus, *cond)
	}
	err := r.getControl(pod).EnsureSandboxTerminated(ctx, args)
	if err != nil {
		return reconcile.Result{}, err
	}
	return ctrl.Result{}, r.updateSandboxStatus(ctx, *newStatus, box)
}

func (r *SandboxReconciler) isCompletedPhase(phase agentsv1alpha1.SandboxPhase) bool {
	return phase == agentsv1alpha1.SandboxFailed || phase == agentsv1alpha1.SandboxSucceeded
}

func (r *SandboxReconciler) ensureFinalizer(ctx context.Context, box *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
	if box.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(box, utils.SandboxFinalizer) {
		newObj, err := utils.PatchFinalizer(ctx, r.Client, box, utils.AddFinalizerOpType, utils.SandboxFinalizer)
		if err != nil {
			logger.Error(err, "patch finalizer failed")
			return nil, err
		}
		box = newObj.(*agentsv1alpha1.Sandbox)
		logger.Info("add sandbox finalizer success")
	}
	return box, nil
}

func (r *SandboxReconciler) patchSandboxAnnotationHash(ctx context.Context, box *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
	if box.Annotations[agentsv1alpha1.SandboxHashWithoutImageAndResources] != "" {
		return box, nil
	}

	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
	_, hashWithoutImageAndResource := core.HashSandbox(box)
	clone := box.DeepCopy()
	body := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, agentsv1alpha1.SandboxHashWithoutImageAndResources, hashWithoutImageAndResource)
	if err := r.Patch(ctx, clone, client.RawPatch(types.MergePatchType, []byte(body))); err != nil {
		logger.Error(err, "patch sandbox annotation failed")
		return nil, err
	}
	logger.Info("patch sandbox annotation success", "annotation", agentsv1alpha1.SandboxHashWithoutImageAndResources)
	return clone, nil
}

func (r *SandboxReconciler) updateSandboxStatus(ctx context.Context, newStatus agentsv1alpha1.SandboxStatus, box *agentsv1alpha1.Sandbox) error {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
	if reflect.DeepEqual(box.Status, newStatus) {
		return nil
	}

	by, _ := json.Marshal(newStatus)
	patchStatus := fmt.Sprintf(`{"status":%s}`, string(by))
	rcvObject := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: box.Namespace, Name: box.Name}}
	err := client.IgnoreNotFound(r.Status().Patch(ctx, rcvObject, client.RawPatch(types.MergePatchType, []byte(patchStatus))))
	if err != nil {
		logger.Error(err, "update sandbox status failed", "patchStatus", patchStatus)
		return err
	}
	core.ResourceVersionExpectations.Expect(rcvObject)
	logger.Info("update sandbox status success", "status", utils.DumpJson(newStatus))
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

	if pod != nil {
		if !pod.DeletionTimestamp.IsZero() {
			return newStatus, true
		} else if pod.Status.Phase == corev1.PodSucceeded && !box.Spec.Paused {
			newStatus.Phase = agentsv1alpha1.SandboxSucceeded
			return newStatus, true
		} else if pod.Status.Phase == corev1.PodFailed && !box.Spec.Paused {
			// During the paused phase, the pod transitions to the Failed state and should be ignored.
			newStatus.Phase = agentsv1alpha1.SandboxFailed
			return newStatus, true
		}
	}

	// If it is paused, first set the sandbox to the Paused state.
	// To prevent loss of state information, the state immediately before Paused must currently be Running.
	if box.Spec.Paused && box.Status.Phase == agentsv1alpha1.SandboxRunning {
		// The paused and resumed condition are exclusive
		utils.RemoveSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionResumed))
		newStatus.Phase = agentsv1alpha1.SandboxPaused
		// enter resume phase
	} else if !box.Spec.Paused && newStatus.Phase == agentsv1alpha1.SandboxPaused {
		// delete paused condition
		utils.RemoveSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
		newStatus.Phase = agentsv1alpha1.SandboxResuming
		cond := metav1.Condition{
			Type:               string(agentsv1alpha1.SandboxConditionResumed),
			Status:             metav1.ConditionFalse,
			Reason:             agentsv1alpha1.SandboxResumeReasonCreatePod,
			LastTransitionTime: metav1.Now(),
		}
		utils.SetSandboxCondition(newStatus, cond)
	}
	return newStatus, false
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentReconciles}).
		For(&agentsv1alpha1.Sandbox{}).
		Named("sandbox").
		Watches(&agentsv1alpha1.Sandbox{}, &handler.EnqueueRequestForObject{}, builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				core.ResourceVersionExpectations.Observe(e.ObjectNew)
				return true
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				core.ResourceVersionExpectations.Delete(e.Object)
				return false
			},
		})).Watches(&corev1.Pod{}, &SandboxPodEventHandler{}).
		Complete(r)
}

// ensureVolumeClaimTemplates creates and ensures PVCs exist for persistent data recovery during sleep/wake operations
func (r *SandboxReconciler) ensureVolumeClaimTemplates(ctx context.Context, box *agentsv1alpha1.Sandbox) error {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))

	if len(box.Spec.VolumeClaimTemplates) == 0 {
		return nil
	}

	for _, template := range box.Spec.VolumeClaimTemplates {
		// Generate PVC name based on template name and sandbox name
		pvcName, err := core.GeneratePVCName(template.Name, box.Name)
		if err != nil {
			logger.Error(err, "failed to generate PVC name", "template", template.Name, "sandbox", box.Name)
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
			logger.Error(err, "failed to set sandbox as owner of PVC", "pvc", pvcName)
			return err
		}

		// Check if PVC already exists
		existingPVC := &corev1.PersistentVolumeClaim{}
		err = r.Get(ctx, client.ObjectKey{Namespace: box.Namespace, Name: pvcName}, existingPVC)

		if err == nil {
			logger.Info("PVC already exists for persistent data recovery", "pvc", pvcName)
			continue
		}

		if !errors.IsNotFound(err) {
			logger.Error(err, "failed to get PVC", "pvc", pvcName)
			return err
		}

		if err = r.Create(ctx, pvc); err == nil {
			logger.Info("created PVC for persistent data recovery", "pvc", pvcName)
			continue
		}

		if !errors.IsAlreadyExists(err) {
			logger.Error(err, "failed to create PVC", "pvc", pvcName)
			return err
		}
		logger.Info("PVC already exists after create attempt", "pvc", pvcName)
	}

	return nil
}
