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

package sandboxtemplate

import (
	"context"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/discovery"
	template "github.com/openkruise/agents/pkg/sandboxtemplate"
	runtimeclient "github.com/openkruise/agents/pkg/utils/runtime"
)

const (
	DefaultBuildTimeout = 30 * time.Minute
	retryInterval       = 2 * time.Second

	buildReasonRunning           = "Running"
	buildReasonProvisioned       = "Provisioned"
	buildReasonCaptured          = "Captured"
	buildReasonInterrupted       = "Interrupted"
	buildReasonTimeout           = "Timeout"
	buildReasonExecutionFailed   = "ExecutionFailed"
	buildReasonInvalidDefinition = "InvalidDefinition"
	buildReasonCaptureConflict   = "CaptureConflict"
	buildReasonCaptureFailed     = "CaptureFailed"
)

type Options struct {
	Timeout          time.Duration
	RuntimeTLSBundle *runtimeclient.TLSBundle
}

type Reconciler struct {
	client.Client
	Options Options

	workerMu  sync.Mutex
	workerWG  sync.WaitGroup
	workerCtx context.Context
	workers   map[types.UID]*buildWorker
}

// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxtemplates,verbs=get;list;watch;patch;delete
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxtemplates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=agents.kruise.io,resources=checkpoints,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

func Add(m manager.Manager, options Options) error {
	if !discovery.DiscoverGVK(api.SandboxTemplateControllerKind) {
		return nil
	}
	if options.Timeout <= 0 {
		options.Timeout = DefaultBuildTimeout
	}
	r := &Reconciler{Client: m.GetClient(), Options: options}
	if err := m.Add(r); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(m).Named("sandbox-template-version").For(&api.SandboxTemplate{}).
		Owns(&api.Sandbox{}).Owns(&api.Checkpoint{}).
		Complete(reconcile.Func(r.ReconcileVersion))
}

func (r *Reconciler) ReconcileVersion(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	v := &api.SandboxTemplate{}
	if err := r.Get(ctx, req.NamespacedName, v); err != nil {
		if apierrors.IsNotFound(err) {
			r.stopVersionWorker(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !template.IsBuildVersion(v) {
		r.stopVersionWorker(req.NamespacedName)
		return ctrl.Result{}, nil
	}
	// Owned build resources are released by ownerReference garbage collection
	// once the Version goes away; only the in-process worker needs a stop.
	if v.DeletionTimestamp != nil {
		r.stopVersionWorker(req.NamespacedName)
		return ctrl.Result{}, nil
	}
	if !template.IsSubmitted(v) {
		remaining := time.Until(v.CreationTimestamp.Add(r.Options.Timeout))
		if remaining > 0 {
			return requeueAfter(remaining, r.setPhase(ctx, v, api.SandboxTemplatePhasePending))
		}
		if r.stopVersionWorker(req.NamespacedName) {
			return ctrl.Result{RequeueAfter: retryInterval}, nil
		}
		pending, err := r.requestDeletion(ctx, v, metav1.DeletePropagationForeground)
		if err != nil {
			return ctrl.Result{}, err
		}
		if pending {
			return ctrl.Result{RequeueAfter: retryInterval}, nil
		}
		return ctrl.Result{}, nil
	}
	if (v.Status.Phase == api.SandboxTemplatePhaseReady || v.Status.Phase == api.SandboxTemplatePhaseFailed) && v.Status.ObservedGeneration == v.Generation {
		return r.reconcileBuildCleanup(ctx, v)
	}
	if options := v.Spec.BuildOptions; options != nil && (len(options.Steps) > 0 || v.Spec.Checkpoint != nil && v.Spec.Checkpoint.Enable) {
		return r.reconcileExecution(ctx, v)
	}
	return requeueAfter(retryInterval, r.setPhase(ctx, v, api.SandboxTemplatePhaseReady))
}

func (r *Reconciler) setPhase(ctx context.Context, v *api.SandboxTemplate, phase api.SandboxTemplateBuildPhase) error {
	before := v.DeepCopy()
	v.Status.Phase = phase
	v.Status.ObservedGeneration = v.Generation
	if equality.Semantic.DeepEqual(before.Status, v.Status) {
		return nil
	}
	return r.Status().Patch(ctx, v, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

// requeueAfter returns the waiting result for normal waiting and drops the
// requeue on error so controller-runtime retries with its own backoff
// instead of discarding a set requeue interval.
func requeueAfter(interval time.Duration, err error) (ctrl.Result, error) {
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}
