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
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/sandbox/core"
	"github.com/openkruise/agents/pkg/discovery"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/utils"
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

	logger.V(consts.DebugLogLevel).Info("Began to process Sandbox for reconcile")
	newStatus := calculateStatus(box)
	if box.Annotations == nil {
		box.Annotations = map[string]string{}
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
	if r.isTerminalPhase(box.Status.Phase) {
		return ctrl.Result{}, nil
	}

	// add finalizer
	if err = r.ensureFinalizer(ctx, box); err != nil {
		return reconcile.Result{}, err
	}

	// Check Shutdown
	requeueAfter, err := r.handleShutdownTime(ctx, box)
	if err != nil {
		return reconcile.Result{}, err
	}

	// check pod
	shouldRequeue := checkPodStatus(args)
	if shouldRequeue {
		return reconcile.Result{RequeueAfter: requeueAfter}, r.updateSandboxStatus(ctx, *newStatus, box)
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

func (r *SandboxReconciler) isTerminalPhase(phase agentsv1alpha1.SandboxPhase) bool {
	return phase == agentsv1alpha1.SandboxFailed || phase == agentsv1alpha1.SandboxSucceeded
}

func (r *SandboxReconciler) ensureFinalizer(ctx context.Context, box *agentsv1alpha1.Sandbox) error {
	if box.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(box, utils.SandboxFinalizer) {
		err := utils.PatchFinalizer(r.Client, box, utils.AddFinalizerOpType, utils.SandboxFinalizer)
		if err != nil {
			return err
		}
		logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box)).Info("add sandbox finalizer success")
	}
	return nil
}

func (r *SandboxReconciler) handleShutdownTime(ctx context.Context, box *agentsv1alpha1.Sandbox) (time.Duration, error) {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
	now := metav1.Now()
	var requeueAfter time.Duration
	if box.Spec.ShutdownTime != nil && box.DeletionTimestamp == nil {
		if box.Spec.ShutdownTime.Before(&now) {
			logger.Info("Sandbox is shutdown time reached, will be deleted")
			return 0, r.Delete(ctx, box)
		}
		requeueAfter = box.Spec.ShutdownTime.Sub(now.Time)
	}
	return requeueAfter, nil
}

func checkPodStatus(args core.EnsureFuncArgs) bool {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	if pod == nil {
		return false
	}

	if !pod.DeletionTimestamp.IsZero() {
		return true
	} else if pod.Status.Phase == corev1.PodSucceeded {
		newStatus.Phase = agentsv1alpha1.SandboxSucceeded
		return true
	} else if pod.Status.Phase == corev1.PodFailed &&
		box.Status.Phase != agentsv1alpha1.SandboxPaused {
		// During the paused phase, the pod transitions to the Failed state and should be ignored.
		newStatus.Phase = agentsv1alpha1.SandboxFailed
		return true
	}
	return false
}

func (r *SandboxReconciler) patchSandboxAnnotationHash(ctx context.Context, box *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
	if box.Annotations[agentsv1alpha1.SandboxHashWithoutImageAndResources] != "" {
		return box, nil
	}

	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
	_, hashWithoutImageAndResource := core.HashSandbox(box)
	clone := box.DeepCopy()
	body := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, agentsv1alpha1.SandboxHashWithoutImageAndResources, hashWithoutImageAndResource)
	if err := r.Patch(context.TODO(), clone, client.RawPatch(types.MergePatchType, []byte(body))); err != nil {
		logger.Error(err, "patch sandbox annotation failed")
		return nil, err
	}
	logger.Info("patch sandbox annotation success", "annotation", agentsv1alpha1.SandboxHashWithoutImageAndResources)
	clone.Annotations[agentsv1alpha1.SandboxHashWithoutImageAndResources] = hashWithoutImageAndResource
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
	err := client.IgnoreNotFound(r.Status().Patch(context.TODO(), rcvObject, client.RawPatch(types.MergePatchType, []byte(patchStatus))))
	if err != nil {
		logger.Error(err, "update sandbox status failed", "patchStatus", patchStatus)
		return err
	}
	logger.Info("update sandbox status success", "status", utils.DumpJson(newStatus))
	return nil
}

func calculateStatus(box *agentsv1alpha1.Sandbox) *agentsv1alpha1.SandboxStatus {
	newStatus := box.Status.DeepCopy()
	hash, _ := core.HashSandbox(box)
	newStatus.ObservedGeneration = box.Generation
	newStatus.UpdateRevision = hash
	if newStatus.Phase == "" {
		newStatus.Phase = agentsv1alpha1.SandboxPending
	}
	return newStatus
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentReconciles}).
		For(&agentsv1alpha1.Sandbox{}).
		Named("sandbox").
		Watches(&agentsv1alpha1.Sandbox{}, &handler.EnqueueRequestForObject{}).
		Watches(&corev1.Pod{}, &SandboxPodEventHandler{}).
		Complete(r)
}
