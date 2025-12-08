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

	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
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
	"github.com/openkruise/agents/pkg/utils"
)

func init() {
	flag.IntVar(&concurrentReconciles, "sandbox-workers", concurrentReconciles, "Max concurrent workers for Sandbox controller.")
}

var (
	concurrentReconciles  = 500
	sandboxControllerKind = agentsv1alpha1.GroupVersion.WithKind("Sandbox")
)

func Add(mgr manager.Manager) error {
	err := (&SandboxReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		controls: core.NewSandboxControl(mgr.GetClient()),
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
// +kubebuilder:rbac:groups=core,resources=events,verbs=create

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Sandbox object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Fetch the sandbox instance
	box := &agentsv1alpha1.Sandbox{}
	err := r.Get(context.TODO(), req.NamespacedName, box)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	} else if box.Status.Phase == agentsv1alpha1.SandboxFailed || box.Status.Phase == agentsv1alpha1.SandboxSucceeded {
		return reconcile.Result{}, nil
	}
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))

	// Add finalizer
	if box.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(box, utils.SandboxFinalizer) {
		err = utils.UpdateFinalizer(r.Client, box, utils.AddFinalizerOpType, utils.SandboxFinalizer)
		if err != nil {
			logger.Error(err, "update sandbox finalizer failed")
			return reconcile.Result{}, err
		}
		logger.Info("add sandbox finalizer success")
	}

	// Check Shutdown
	now := metav1.Now()
	var requeueAfter time.Duration
	if box.Spec.ShutdownTime != nil && box.DeletionTimestamp == nil {
		if box.Spec.ShutdownTime.Before(&now) {
			logger.Info("Sandbox is shutdown time reached, will be deleted")
			return ctrl.Result{}, r.Delete(ctx, box)
		} else {
			requeueAfter = box.Spec.ShutdownTime.Time.Sub(now.Time)
		}
	}

	logger.V(consts.DebugLogLevel).Info("Began to process Sandbox for reconcile")
	newStatus := box.Status.DeepCopy()
	newStatus.ObservedGeneration = box.Generation
	if newStatus.Phase == "" {
		newStatus.Phase = agentsv1alpha1.SandboxPending
	}

	// fetch pod
	pod := &corev1.Pod{}
	err = r.Get(ctx, client.ObjectKey{Namespace: box.Namespace, Name: box.Name}, pod)
	if err != nil && !errors.IsNotFound(err) {
		logger.Error(err, "Get Pod failed")
		return reconcile.Result{}, err
	} else if errors.IsNotFound(err) {
		pod = nil
	} else if !pod.DeletionTimestamp.IsZero() {
		return reconcile.Result{RequeueAfter: min(requeueAfter, time.Second*3)}, nil
	} else if pod.Status.Phase == corev1.PodSucceeded {
		newStatus.Phase = agentsv1alpha1.SandboxSucceeded
		return ctrl.Result{RequeueAfter: requeueAfter}, r.updateSandboxStatus(ctx, *newStatus, box)
	} else if pod.Status.Phase == corev1.PodFailed &&
		box.Status.Phase != agentsv1alpha1.SandboxPaused {
		// During the paused phase, the pod transitions to the Failed state and should be ignored.
		newStatus.Phase = agentsv1alpha1.SandboxFailed
		return ctrl.Result{RequeueAfter: requeueAfter}, r.updateSandboxStatus(ctx, *newStatus, box)
	}

	if !box.DeletionTimestamp.IsZero() {
		newStatus.Phase = agentsv1alpha1.SandboxTerminating
		cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady))
		if cond != nil && cond.Status == metav1.ConditionTrue {
			cond.Status = metav1.ConditionFalse
			cond.LastTransitionTime = metav1.Now()
			utils.SetSandboxCondition(newStatus, *cond)
		}
		// If it is paused, first set the sandbox to the Paused state.
	} else if box.Spec.Paused && box.Status.Phase == agentsv1alpha1.SandboxRunning {
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

	args := core.EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus}
	switch newStatus.Phase {
	case agentsv1alpha1.SandboxPending:
		err = r.getControl(args.Pod).EnsureSandboxPhasePending(ctx, args)
		if err != nil {
			return reconcile.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, r.updateSandboxStatus(ctx, *newStatus, box)

	case agentsv1alpha1.SandboxRunning:
		err = r.getControl(args.Pod).EnsureSandboxPhaseRunning(ctx, args)
		if err != nil {
			return reconcile.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, r.updateSandboxStatus(ctx, *newStatus, box)

	case agentsv1alpha1.SandboxPaused:
		err = r.getControl(args.Pod).EnsureSandboxPhasePaused(ctx, args)
		if err != nil {
			return reconcile.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, r.updateSandboxStatus(ctx, *newStatus, box)

	case agentsv1alpha1.SandboxResuming:
		err = r.getControl(args.Pod).EnsureSandboxPhaseResuming(ctx, args)
		if err != nil {
			return reconcile.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, r.updateSandboxStatus(ctx, *newStatus, box)

	case agentsv1alpha1.SandboxTerminating:
		err = r.getControl(args.Pod).EnsureSandboxPhaseTerminating(ctx, args)
		if err != nil {
			return reconcile.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, r.updateSandboxStatus(ctx, *newStatus, box)
	}
	logger.Info("sandbox status phase is invalid", "phase", box.Status.Phase)
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *SandboxReconciler) updateSandboxStatus(ctx context.Context, newStatus agentsv1alpha1.SandboxStatus, box *agentsv1alpha1.Sandbox) error {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
	if reflect.DeepEqual(box.Status, newStatus) {
		return nil
	}

	by, _ := json.Marshal(newStatus)
	patchStatus := fmt.Sprintf(`{"status":%s}`, string(by))
	rcvObject := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: box.Namespace, Name: box.Name}}
	err := r.Status().Patch(context.TODO(), rcvObject, client.RawPatch(types.MergePatchType, []byte(patchStatus)))
	if err != nil {
		logger.Error(err, "update sandbox status failed")
		return err
	}
	logger.Info("update sandbox status success", "status", utils.DumpJson(newStatus))
	return nil
}

func (r *SandboxReconciler) getControl(pod *corev1.Pod) core.SandboxControl {
	return r.controls[core.CommonControlName]
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
