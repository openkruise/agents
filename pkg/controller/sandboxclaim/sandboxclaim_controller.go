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

package sandboxclaim

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"reflect"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	sandboxclient "github.com/openkruise/agents/client/clientset/versioned"
	informers "github.com/openkruise/agents/client/informers/externalversions"
	"github.com/openkruise/agents/pkg/controller/sandboxclaim/core"
	"github.com/openkruise/agents/pkg/discovery"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
	k8sinformers "k8s.io/client-go/informers"
)

func init() {
	flag.IntVar(&concurrentReconciles, "sandboxclaim-workers", concurrentReconciles, "Max concurrent workers for SandboxClaim controller.")
	flag.IntVar(&maxClaimBatchSize, "sandboxclaim-max-batch-size", maxClaimBatchSize, "Maximum batch size for claiming sandboxes in a single reconcile cycle")
}

var (
	concurrentReconciles = 500
	maxClaimBatchSize    = 10
	controllerKind       = agentsv1alpha1.GroupVersion.WithKind("SandboxClaim")
)

func Add(mgr manager.Manager) error {
	if !utilfeature.DefaultFeatureGate.Enabled(features.SandboxClaimGate) || !discovery.DiscoverGVK(controllerKind) {
		return nil
	}

	config := mgr.GetConfig()

	// Create typed clients
	sandboxClientset, err := sandboxclient.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create sandbox clientset: %w", err)
	}

	k8sClientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes clientset: %w", err)
	}

	// Create informers for cache
	informerFactory := informers.NewSharedInformerFactory(sandboxClientset, time.Minute*10)
	sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
	sandboxSetInformer := informerFactory.Api().V1alpha1().SandboxSets().Informer()

	coreInformerFactory := k8sinformers.NewSharedInformerFactory(k8sClientset, time.Minute*10)
	persistentVolumeInformer := coreInformerFactory.Core().V1().PersistentVolumes().Informer()
	coreInformerFactorySpecifiedNs := k8sinformers.NewSharedInformerFactoryWithOptions(k8sClientset, time.Minute*10, k8sinformers.WithNamespace(utils.DefaultSandboxDeployNamespace))
	secretInformer := coreInformerFactorySpecifiedNs.Core().V1().Secrets().Informer()

	// Initialize cache
	cache, err := sandboxcr.NewCache(informerFactory, sandboxInformer, sandboxSetInformer, coreInformerFactorySpecifiedNs, secretInformer, coreInformerFactory, persistentVolumeInformer)
	if err != nil {
		return fmt.Errorf("failed to create cache: %w", err)
	}

	// Register cache as a runnable so it starts when manager starts
	err = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		klog.Info("Starting SandboxClaim cache and waiting for sync...")
		if err := cache.Run(ctx); err != nil {
			klog.Errorf("cache run error: %v", err)
			return err
		}
		klog.Info("SandboxClaim cache synced successfully")
		return nil
	}))
	if err != nil {
		return fmt.Errorf("failed to add cache runnable: %w", err)
	}

	recorder := mgr.GetEventRecorderFor("sandboxclaim")
	err = (&Reconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		recorder: recorder,
		controls: core.NewClaimControl(mgr.GetClient(), recorder, &clients.ClientSet{
			K8sClient:     k8sClientset,
			SandboxClient: sandboxClientset,
			Config:        config,
		}, cache),
	}).SetupWithManager(mgr)
	if err != nil {
		return err
	}
	klog.Infof("start SandboxClaimReconciler success")
	return nil
}

// Reconciler reconciles a SandboxClaim object
type Reconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	controls map[string]core.ClaimControl
	recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxclaims,verbs=get;list;watch;patch;delete
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes,verbs=get;list;update;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxsets,verbs=get
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;update;patch
// +kubebuilder:rbac:groups=core,resources=persistentvolumes,verbs=get;list;watch

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Fetch the SandboxClaim instance
	claim := &agentsv1alpha1.SandboxClaim{}
	if err := r.Get(ctx, req.NamespacedName, claim); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	logger := logf.FromContext(ctx).WithValues("sandboxclaim", klog.KObj(claim))
	logger.Info("Began to process SandboxClaim for reconcile")

	// Check resourceVersion expectations
	core.ResourceVersionExpectations.Observe(claim)
	if isSatisfied, unsatisfiedDuration := core.ResourceVersionExpectations.IsSatisfied(claim); !isSatisfied {
		if unsatisfiedDuration < expectations.ExpectationTimeout {
			logger.Info("Not satisfied resourceVersion for SandboxClaim, wait for cache event")
			return reconcile.Result{RequeueAfter: expectations.ExpectationTimeout - unsatisfiedDuration}, nil
		}
		logger.Info("Expectation unsatisfied overtime for SandboxClaim, wait for cache event timeout", "timeout", unsatisfiedDuration)
		core.ResourceVersionExpectations.Delete(claim)
	}

	// Initialize new status
	newStatus := claim.Status.DeepCopy()

	// Fetch SandboxSet
	sandboxSet := &agentsv1alpha1.SandboxSet{}
	sandboxSetKey := client.ObjectKey{Namespace: claim.Namespace, Name: claim.Spec.TemplateName}
	if err := r.Get(ctx, sandboxSetKey, sandboxSet); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("SandboxSet not found, marking claim as completed")
			core.TransitionToCompleted(newStatus, "SandboxSetNotFound",
				fmt.Sprintf("SandboxSet %s not found", claim.Spec.TemplateName))
			return ctrl.Result{}, r.updateClaimStatus(ctx, *newStatus, claim)
		}
		return reconcile.Result{}, err
	}

	// Construct args
	args := core.ClaimArgs{
		Claim:      claim,
		SandboxSet: sandboxSet,
		NewStatus:  newStatus,
	}

	// Calculate status
	newStatus, shouldRequeue := core.CalculateClaimStatus(args)
	if shouldRequeue {
		return reconcile.Result{}, r.updateClaimStatus(ctx, *newStatus, claim)
	}

	// Execute business logic and get requeue strategy
	var strategy core.RequeueStrategy
	var err error

	// State-driven execution - each Ensure method returns its own requeue strategy
	switch newStatus.Phase {
	case agentsv1alpha1.SandboxClaimPhaseClaiming:
		strategy, err = r.getControl().EnsureClaimClaiming(ctx, args)

	case agentsv1alpha1.SandboxClaimPhaseCompleted:
		strategy, err = r.getControl().EnsureClaimCompleted(ctx, args)

	default:
		logger.Info("Unknown phase encountered", "phase", newStatus.Phase)
		r.recorder.Event(claim, "Warning", "UnknownPhase",
			fmt.Sprintf("Unknown phase: %s", newStatus.Phase))
		return ctrl.Result{}, nil
	}

	if err != nil {
		// Return error to controller-runtime for exponential backoff retry
		logger.Error(err, "Failed to ensure claim, will retry with backoff")
		return reconcile.Result{}, err
	}

	// Update status after successful execution
	// If update fails, return error to trigger retry (but lose calculated strategy)
	if err := r.updateClaimStatus(ctx, *newStatus, claim); err != nil {
		logger.Error(err, "Failed to update status, will retry")
		return ctrl.Result{}, err
	}

	// Convert RequeueStrategy to ctrl.Result
	if strategy.Immediate {
		logger.V(1).Info("Immediate requeue requested")
		return ctrl.Result{Requeue: true}, nil
	}
	if strategy.After > 0 {
		logger.V(1).Info("Delayed requeue requested", "after", strategy.After)
		return ctrl.Result{RequeueAfter: strategy.After}, nil
	}
	// No requeue, wait for Watch events
	return ctrl.Result{}, nil
}

func (r *Reconciler) getControl() core.ClaimControl {
	return r.controls[core.CommonControlName]
}

func (r *Reconciler) updateClaimStatus(ctx context.Context, newStatus agentsv1alpha1.SandboxClaimStatus, claim *agentsv1alpha1.SandboxClaim) error {
	logger := logf.FromContext(ctx).WithValues("sandboxclaim", klog.KObj(claim))

	if reflect.DeepEqual(claim.Status, newStatus) {
		return nil
	}

	// Use strategic patch for status update
	by, _ := json.Marshal(newStatus)
	patchStatus := fmt.Sprintf(`{"status":%s}`, string(by))
	rcvObject := &agentsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: claim.Namespace,
			Name:      claim.Name,
		},
	}

	err := client.IgnoreNotFound(r.Status().Patch(ctx, rcvObject, client.RawPatch(types.MergePatchType, []byte(patchStatus))))
	if err != nil {
		logger.Error(err, "update sandboxclaim status failed", "patchStatus", patchStatus)
		return err
	}

	// Set expectation for resource version
	core.ResourceVersionExpectations.Expect(rcvObject)

	logger.Info("update sandboxclaim status success", "status", utils.DumpJson(newStatus))
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Note: We don't watch Sandbox resources because:
	// 1. SandboxClaim is a one-time claim operation, not continuous management
	// 2. After Completed phase, the controller no longer manages claimed sandboxes (by design)
	// 3. This reduces unnecessary reconcile triggers and improves performance
	return ctrl.NewControllerManagedBy(mgr).
		Named("sandboxclaim-controller").
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentReconciles}).
		For(&agentsv1alpha1.SandboxClaim{}).
		Watches(&agentsv1alpha1.SandboxClaim{}, &handler.EnqueueRequestForObject{}, builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				core.ResourceVersionExpectations.Observe(e.ObjectNew)
				return true
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				core.ResourceVersionExpectations.Delete(e.Object)
				return false
			},
		})).
		Complete(r)
}
