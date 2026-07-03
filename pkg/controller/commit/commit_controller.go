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
	"flag"
	"fmt"
	"reflect"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/commit/core"
	jobutil "github.com/openkruise/agents/pkg/controller/commit/job"
	"github.com/openkruise/agents/pkg/discovery"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

var (
	concurrentReconciles = 5
	controllerName       = "commit-controller"
	controllerKind       = agentsv1alpha1.SchemeGroupVersion.WithKind("Commit")
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

	// Register field indexes for LabelCommitUID to speed up List queries.
	commitUIDIndex := func(obj client.Object) []string {
		if uid, ok := obj.GetLabels()[jobutil.LabelCommitUID]; ok {
			return []string{uid}
		}
		return nil
	}
	if err = mgr.GetFieldIndexer().IndexField(context.Background(), &batchv1.Job{}, jobutil.IndexFieldCommitUID, commitUIDIndex); err != nil {
		return err
	}
	if err = mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, jobutil.IndexFieldCommitUID, commitUIDIndex); err != nil {
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
	ctrl.Log.Info("Started CommitReconciler successfully")
	return nil
}

// +kubebuilder:rbac:groups=agents.kruise.io,resources=commits,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.kruise.io,resources=commits/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=commits/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list
// +kubebuilder:rbac:groups=core,resources=events,verbs=create
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=create;delete;get;watch;list

func (r *CommitReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	log := log.FromContext(ctx)
	log.Info("Reconcile", "name", req.Name, "namespace", req.Namespace)
	commit := &agentsv1alpha1.Commit{}
	if err = r.Get(ctx, req.NamespacedName, commit); err != nil {
		if errors.IsNotFound(err) {
			log.V(4).Info("Commit not found", "name", req.Name, "namespace", req.Namespace)
			core.ScaleExpectations.DeleteExpectations(req.NamespacedName.String())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	start := time.Now()
	defer func() {
		log.V(4).Info("Reconcile finished", "name", req.Name, "namespace", req.Namespace, "elapsed", time.Since(start), "err", err)
	}()

	// Fetch target pod from informer cache. Commit only targets Sandbox pods,
	// which always carry the created-by label and are guaranteed in the cache
	// even with CachePodLabelSelector enabled.
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
		log.Error(err, "get control failed", "commit", klog.KObj(commit))
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !commit.DeletionTimestamp.IsZero() {
		core.ScaleExpectations.DeleteExpectations(utils.GetControllerKey(commit))
		args := &core.EnsureFuncArgs{Pod: pod, Commit: commit, NewStatus: newStatus}
		return r.handleCommitDelete(ctx, args, control)
	}

	// Skip terminal phases
	if commit.Status.Phase == agentsv1alpha1.CommitPhaseSucceeded || commit.Status.Phase == agentsv1alpha1.CommitPhaseFailed {
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
	case agentsv1alpha1.CommitPhasePending, "":
		// Observe expectations in reconcile loop instead of event handler to avoid lock contention.
		jobList := &batchv1.JobList{}
		if err := r.List(ctx, jobList, client.InNamespace(commit.Namespace), client.MatchingFields{jobutil.IndexFieldCommitUID: string(commit.UID)}); err == nil {
			for _, j := range jobList.Items {
				core.ScaleExpectations.ObserveScale(utils.GetControllerKey(commit), expectations.Create, j.Name)
			}
		}
		// Check ScaleExpectations to avoid duplicated Job creation due to stale cache.
		if isSatisfied, unsatisfiedDuration, _ := core.ScaleExpectations.SatisfiedExpectations(utils.GetControllerKey(commit)); !isSatisfied {
			if unsatisfiedDuration < expectations.ExpectationTimeout {
				log.Info("Not satisfied ScaleExpectation for Commit, wait for cache event", "commit", klog.KObj(commit))
				return ctrl.Result{RequeueAfter: expectations.ExpectationTimeout - unsatisfiedDuration}, nil
			}
			log.Info("ScaleExpectation unsatisfied overtime, proceeding", "commit", klog.KObj(commit))
			core.ScaleExpectations.DeleteExpectations(utils.GetControllerKey(commit))
		}
		return r.handleCommitPending(ctx, args, control)
	case agentsv1alpha1.CommitPhaseRunning:
		return r.handleCommitRunning(ctx, args, control)
	default:
		log.Info("Unknown commit phase", "commit", klog.KObj(commit), "phase", commit.Status.Phase)
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

func (r *CommitReconciler) handleCommitDelete(ctx context.Context, args *core.EnsureFuncArgs, control core.CommitControl) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	commit := args.Commit
	log.V(3).Info("handleCommitDelete", "commit", klog.KObj(commit), "commitID", commit.Status.CommitID, "uid", commit.UID)

	// If commit never reached Running (no CommitID), just remove finalizer without
	// calling the provider to delete a non-existent remote resource.
	if commit.Status.CommitID == "" {
		log.Info("Commit has no commitID, just remove finalizer", "commit", klog.KObj(commit))
		if _, err := utils.PatchFinalizer(ctx, r.Client, commit, utils.RemoveFinalizerOpType, agentsv1alpha1.CommitFinalizer); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	requeueAfter, err := control.EnsureCommitDeleted(ctx, args)
	if err != nil {
		log.Error(err, "handleCommitDelete failed", "commit", klog.KObj(commit))
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *CommitReconciler) handleCommitPending(ctx context.Context, args *core.EnsureFuncArgs, control core.CommitControl) (ctrl.Result, error) {
	commit := args.Commit
	if args.Pod == nil || !args.Pod.DeletionTimestamp.IsZero() {
		now := metav1.Now()
		args.NewStatus.Phase = agentsv1alpha1.CommitPhaseFailed
		args.NewStatus.CompletionTime = &now
		r.Recorder.Eventf(commit, corev1.EventTypeWarning, "PodNotFound", "Target pod %s not found or deleting", commit.Spec.PodName)
		return ctrl.Result{}, r.updateCommitStatus(ctx, *args.NewStatus, commit)
	}
	return r.ensureAndApply(ctx, args, control.EnsureCommitRunning)
}

func (r *CommitReconciler) handleCommitRunning(ctx context.Context, args *core.EnsureFuncArgs, control core.CommitControl) (ctrl.Result, error) {
	return r.ensureAndApply(ctx, args, control.EnsureCommitUpdated)
}

// ensureAndApply calls the given ensure function, then applies the resulting status.
// On error the status is still persisted (best-effort) before returning the error.
func (r *CommitReconciler) ensureAndApply(ctx context.Context, args *core.EnsureFuncArgs, ensureFunc func(context.Context, *core.EnsureFuncArgs) (time.Duration, error)) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	commit := args.Commit
	requeueAfter, err := ensureFunc(ctx, args)
	if err != nil {
		if retErr := r.updateCommitStatus(ctx, *args.NewStatus, commit); retErr != nil {
			log.Error(retErr, "Failed to update commit status on error", "commit", klog.KObj(commit))
		}
		return ctrl.Result{}, err
	}
	if retErr := r.updateCommitStatus(ctx, *args.NewStatus, commit); retErr != nil {
		return ctrl.Result{}, retErr
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *CommitReconciler) handleCommitTTL(ctx context.Context, commit *agentsv1alpha1.Commit) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	if commit.Spec.TtlAfterFinished == nil {
		return ctrl.Result{}, nil
	}
	ttl := commit.Spec.TtlAfterFinished.Duration
	completionTime := commit.Status.CompletionTime
	if completionTime == nil {
		return ctrl.Result{}, nil
	}
	timeSinceCompletion := time.Since(completionTime.Time)
	if timeSinceCompletion > ttl {
		log.Info("TTL expired, deleting commit", "commit", klog.KObj(commit), "ttl", ttl)
		r.Recorder.Eventf(commit, corev1.EventTypeNormal, "TTLExpired", "Deleting Commit after TTL of %v", ttl)
		return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, commit))
	}
	requeueAfter := ttl - timeSinceCompletion
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *CommitReconciler) updateCommitStatus(ctx context.Context, newStatus agentsv1alpha1.CommitStatus, commit *agentsv1alpha1.Commit) error {
	log := log.FromContext(ctx)
	if reflect.DeepEqual(commit.Status, newStatus) {
		return nil
	}
	updatedCommit := commit.DeepCopy()
	updatedCommit.Status = newStatus
	if err := client.IgnoreNotFound(r.Status().Patch(ctx, updatedCommit, client.MergeFrom(commit))); err != nil {
		log.Error(err, "Failed to update commit status", "commit", klog.KObj(commit))
		return err
	}
	log.Info("Updated commit status", "commit", klog.KObj(commit), "phase", newStatus.Phase)
	commit.Status = newStatus
	return nil
}

func (r *CommitReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentReconciles}).
		For(&agentsv1alpha1.Commit{}).
		Watches(&batchv1.Job{}, &enqueueRequestForJob{}).
		Complete(r)
}
