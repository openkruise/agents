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

package commit

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"reflect"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/commit/core"
	"github.com/openkruise/agents/pkg/discovery"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/utils"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

var (
	concurrentReconciles = 30
	controllerName       = "commit-controller"
	controllerKind       = agentsv1alpha1.SchemeGroupVersion.WithKind(agentsv1alpha1.CommitKind)
)

func init() {
	flag.IntVar(&concurrentReconciles, "commit-workers", concurrentReconciles, "Max concurrent workers for Commit controller.")
}

// CommitReconciler reconciles a Commit object
type CommitReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	controls map[string]core.CommitControl
}

func Add(mgr ctrl.Manager) error {
	if !utilfeature.DefaultFeatureGate.Enabled(features.CommitGate) || !discovery.DiscoverGVK(controllerKind) {
		return nil
	}
	recorder := mgr.GetEventRecorderFor(controllerName)
	controls, err := core.NewCommitControl(mgr.GetClient(), recorder)
	if err != nil {
		return err
	}
	if err = (&CommitReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: recorder,
		controls: controls,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	klog.InfoS("Started CommitReconciler successfully")
	return nil
}

// +kubebuilder:rbac:groups=agents.kruise.io,resources=commits,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.kruise.io,resources=commits/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=commits/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=create;delete;get;watch;list

func (r *CommitReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	klog.InfoS("Reconcile", "name", req.Name, "namespace", req.Namespace)
	commit := &agentsv1alpha1.Commit{}
	if err = r.Get(ctx, req.NamespacedName, commit); err != nil {
		if errors.IsNotFound(err) {
			klog.V(4).InfoS("Commit not found", "name", req.Name, "namespace", req.Namespace)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	start := time.Now()
	defer func() {
		klog.V(4).InfoS("Reconcile finished", "name", req.Name, "namespace", req.Namespace, "elapsed", time.Since(start), "err", err)
	}()

	// Fetch target pod
	pod := &corev1.Pod{}
	if err = r.Get(ctx, client.ObjectKey{Namespace: commit.Namespace, Name: commit.Spec.PodName}, pod); err != nil {
		if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		pod = nil
	}

	newStatus := commit.Status.DeepCopy()

	// Get control implementation
	control, err := r.getControl(commit)
	if err != nil {
		klog.ErrorS(err, "get control failed", "commit", klog.KObj(commit))
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !commit.DeletionTimestamp.IsZero() {
		args := &core.EnsureFuncArgs{Pod: pod, Commit: commit, NewStatus: newStatus}
		_, err := control.EnsureCommitDeleted(ctx, args)
		return ctrl.Result{}, err
	}

	// Skip terminal phases
	if commit.Status.Phase == agentsv1alpha1.CommitSucceeded || commit.Status.Phase == agentsv1alpha1.CommitFailed {
		return r.handleCommitTTL(ctx, commit)
	}

	// Add finalizer
	if !controllerutil.ContainsFinalizer(commit, agentsv1alpha1.CommitFinalizer) {
		if _, err = utils.PatchFinalizer(ctx, r.Client, commit, utils.AddFinalizerOpType, agentsv1alpha1.CommitFinalizer); err != nil {
			return ctrl.Result{}, err
		}
	}

	args := &core.EnsureFuncArgs{Pod: pod, Commit: commit, NewStatus: newStatus}

	switch commit.Status.Phase {
	case agentsv1alpha1.CommitPending, "":
		return r.handleCommitPending(ctx, args, control)
	case agentsv1alpha1.CommitRunning:
		return r.handleCommitRunning(ctx, args, control)
	default:
		klog.InfoS("Unknown commit phase", "commit", klog.KObj(commit), "phase", commit.Status.Phase)
		return ctrl.Result{}, nil
	}
}

func (r *CommitReconciler) getControl(commit *agentsv1alpha1.Commit) (core.CommitControl, error) {
	if mode, ok := commit.Annotations[utils.CommitAnnotationModeKey]; ok && mode != "" {
		control, ok := r.controls[mode]
		if !ok {
			return nil, fmt.Errorf("commit mode(%s) control not found", mode)
		}
		return control, nil
	}
	control, ok := r.controls[core.CommonControlName]
	if !ok {
		return nil, fmt.Errorf("commit mode(%s) control not found", core.CommonControlName)
	}
	return control, nil
}

func (r *CommitReconciler) handleCommitPending(ctx context.Context, args *core.EnsureFuncArgs, control core.CommitControl) (ctrl.Result, error) {
	commit := args.Commit
	if args.Pod == nil {
		now := metav1.Now()
		args.NewStatus.Phase = agentsv1alpha1.CommitFailed
		args.NewStatus.CompletionTime = &now
		r.Recorder.Eventf(commit, corev1.EventTypeWarning, "PodNotFound", "Target pod %s not found", commit.Spec.PodName)
		return ctrl.Result{}, r.updateCommitStatus(ctx, *args.NewStatus, commit)
	}
	requeueAfter, err := control.EnsureCommitRunning(ctx, args)
	if err != nil {
		if retErr := r.updateCommitStatus(ctx, *args.NewStatus, commit); retErr != nil {
			klog.ErrorS(retErr, "Failed to update commit status on error", "commit", klog.KObj(commit))
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, r.updateCommitStatus(ctx, *args.NewStatus, commit)
}

func (r *CommitReconciler) handleCommitRunning(ctx context.Context, args *core.EnsureFuncArgs, control core.CommitControl) (ctrl.Result, error) {
	commit := args.Commit
	requeueAfter, err := control.EnsureCommitUpdated(ctx, args)
	if err != nil {
		if retErr := r.updateCommitStatus(ctx, *args.NewStatus, commit); retErr != nil {
			klog.ErrorS(retErr, "Failed to update commit status on error", "commit", klog.KObj(commit))
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, r.updateCommitStatus(ctx, *args.NewStatus, commit)
}

func (r *CommitReconciler) handleCommitTTL(ctx context.Context, commit *agentsv1alpha1.Commit) (ctrl.Result, error) {
	if commit.Spec.Ttl == nil {
		return ctrl.Result{}, nil
	}
	ttl := commit.Spec.Ttl.Duration
	completionTime := commit.Status.CompletionTime
	if completionTime == nil {
		return ctrl.Result{}, nil
	}
	timeSinceCompletion := time.Since(completionTime.Time)
	if timeSinceCompletion > ttl {
		klog.InfoS("TTL expired, deleting commit", "commit", klog.KObj(commit), "ttl", ttl)
		r.Recorder.Eventf(commit, corev1.EventTypeNormal, "TTLExpired", "Deleting Commit after TTL of %v", ttl)
		return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, commit))
	}
	requeueAfter := ttl - timeSinceCompletion
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *CommitReconciler) updateCommitStatus(ctx context.Context, newStatus agentsv1alpha1.CommitStatus, commit *agentsv1alpha1.Commit) error {
	if reflect.DeepEqual(commit.Status, newStatus) {
		return nil
	}
	by, _ := json.Marshal(newStatus)
	patchStatus := fmt.Sprintf(`{"status":%s}`, string(by))
	rcvObject := &agentsv1alpha1.Commit{ObjectMeta: metav1.ObjectMeta{Namespace: commit.Namespace, Name: commit.Name}}
	if err := client.IgnoreNotFound(r.Status().Patch(ctx, rcvObject, client.RawPatch(types.MergePatchType, []byte(patchStatus)))); err != nil {
		klog.ErrorS(err, "Failed to update commit status", "commit", klog.KObj(commit))
		return err
	}
	klog.InfoS("Updated commit status", "commit", klog.KObj(commit), "phase", newStatus.Phase)
	commit.Status = newStatus
	return nil
}

func (r *CommitReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentReconciles}).
		For(&agentsv1alpha1.Commit{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}
