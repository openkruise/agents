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

package poolautoscaler

import (
	"context"
	"flag"
	"fmt"
	"reflect"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/discovery"
	"github.com/openkruise/agents/pkg/features"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

func init() {
	flag.IntVar(&concurrentReconciles, "poolautoscaler-workers", concurrentReconciles,
		"Max concurrent workers for PoolAutoscaler controller.")
	flag.IntVar(&observationWindowSeconds, "observation-window-seconds", observationWindowSeconds,
		"Observation window in seconds for PoolAutoscaler capacity monitoring. "+
			"Samples within this window are averaged before making scaling decisions.")
	flag.IntVar(&samplingIntervalSeconds, "sampling-interval-seconds", samplingIntervalSeconds,
		"Sampling interval in seconds for PoolAutoscaler capacity monitoring. "+
			"Controls how frequently (available, statusReplicas) samples are collected.")
}

var (
	concurrentReconciles = 1
	controllerKind       = agentsv1alpha1.GroupVersion.WithKind("PoolAutoscaler")
)

// Add creates a new PoolAutoscaler Controller and adds it to the Manager.
func Add(mgr manager.Manager) error {
	if !utilfeature.DefaultFeatureGate.Enabled(features.PoolAutoscalerGate) || !discovery.DiscoverGVK(controllerKind) {
		return nil
	}
	r := &Reconciler{
		Client:   mgr.GetClient(),
		recorder: mgr.GetEventRecorderFor("pool-autoscaler-controller"),
		monitors: make(map[types.NamespacedName]*capacityMonitor),
	}
	err := r.SetupWithManager(mgr)
	if err != nil {
		return err
	}
	klog.Infof("Started PoolAutoscalerReconciler successfully")
	return nil
}

// Reconciler reconciles a PoolAutoscaler object.
type Reconciler struct {
	client.Client
	recorder record.EventRecorder
	mu       sync.Mutex
	monitors map[types.NamespacedName]*capacityMonitor
}

func (r *Reconciler) SetupWithManager(mgr manager.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&agentsv1alpha1.PoolAutoscaler{},
		"spec.scaleTargetRef.name",
		func(obj client.Object) []string {
			pa := obj.(*agentsv1alpha1.PoolAutoscaler)
			if pa.Spec.ScaleTargetRef.Name == "" {
				return nil
			}
			return []string{pa.Spec.ScaleTargetRef.Name}
		}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.PoolAutoscaler{}).
		Watches(&agentsv1alpha1.SandboxSet{}, handler.EnqueueRequestsFromMapFunc(r.sandboxSetToPoolAutoscaler)).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentReconciles}).
		Complete(r)
}

// sandboxSetToPoolAutoscaler maps a SandboxSet change to the PoolAutoscaler that targets it.
func (r *Reconciler) sandboxSetToPoolAutoscaler(ctx context.Context, obj client.Object) []ctrl.Request {
	sbs := obj.(*agentsv1alpha1.SandboxSet)
	paList := &agentsv1alpha1.PoolAutoscalerList{}
	if err := r.List(ctx, paList, client.InNamespace(sbs.Namespace),
		client.MatchingFields{"spec.scaleTargetRef.name": sbs.Name}); err != nil {
		return nil
	}
	var requests []ctrl.Request
	for i := range paList.Items {
		requests = append(requests, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: paList.Items[i].Namespace, Name: paList.Items[i].Name},
		})
	}
	return requests
}

// +kubebuilder:rbac:groups=agents.kruise.io,resources=poolautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.kruise.io,resources=poolautoscalers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxsets,verbs=get;list;watch

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := klog.FromContext(ctx).WithValues("poolautoscaler", req.NamespacedName)

	pa := &agentsv1alpha1.PoolAutoscaler{}
	if err := r.Get(ctx, req.NamespacedName, pa); err != nil {
		if errors.IsNotFound(err) {
			r.deleteMonitor(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if pa.Spec.Suspend != nil && *pa.Spec.Suspend {
		logger.V(5).Info("PoolAutoscaler is suspended, skipping reconciliation")
		setCondition(pa, metav1.Condition{
			Type:               string(agentsv1alpha1.ScalingActive),
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "Suspended",
			Message:            "autoscaler is suspended",
		})
		if err := r.updateStatus(ctx, pa, pa.Status.CurrentReplicas, pa.Status.DesiredReplicas, pa.Status.CurrentCapacity.Available, nil, true, false); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Read SandboxSet once for the entire reconciliation
	sbs, err := r.getSandboxSet(ctx, pa)
	if err != nil {
		setCondition(pa, metav1.Condition{
			Type:               string(agentsv1alpha1.ScalingActive),
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "FailedGetTarget",
			Message:            fmt.Sprintf("failed to get SandboxSet: %v", err),
		})
		setCondition(pa, metav1.Condition{
			Type:               string(agentsv1alpha1.AbleToScale),
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "FailedGetTarget",
			Message:            fmt.Sprintf("failed to get SandboxSet: %v", err),
		})
		r.recorder.Eventf(pa, "Warning", "FailedGetScale",
			"Failed to get SandboxSet %s/%s: %v", pa.Namespace, pa.Spec.ScaleTargetRef.Name, err)
		return ctrl.Result{}, err
	}

	setCondition(pa, metav1.Condition{
		Type:               string(agentsv1alpha1.AbleToScale),
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "ReadyToScale",
		Message:            "the autoscaler can access the target resource",
	})

	specReplicas := sbs.Spec.Replicas
	statusReplicas := sbs.Status.Replicas
	available := sbs.Status.AvailableReplicas

	avgAvailable, avgReplicas := r.observeAndAggregate(ctx, pa, available, statusReplicas)
	result, err := r.computeDesiredReplicas(ctx, pa, specReplicas, avgReplicas, avgAvailable)
	if err != nil {
		r.recorder.Eventf(pa, "Warning", "FailedComputeReplicas",
			"Failed to compute desired replicas: %v", err)
		return ctrl.Result{}, err
	}

	desiredReplicas := r.clampToBounds(pa, result.desiredReplicas)
	reason := result.reason

	// Cron-triggered scaling bypasses the stabilization window — cron represents
	// an explicit user intent for a specific replica count at a specific time.
	if !result.cronTriggered {
		desiredReplicas = r.applyStabilizationWindow(pa, specReplicas, desiredReplicas)
	}

	// Compare against spec (what we previously told SandboxSet), not status
	if desiredReplicas != specReplicas {
		if err := r.doScale(ctx, sbs, desiredReplicas); err != nil {
			setCondition(pa, metav1.Condition{
				Type:               string(agentsv1alpha1.AbleToScale),
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             "FailedScale",
				Message:            fmt.Sprintf("failed to scale: %v", err),
			})
			r.recorder.Eventf(pa, "Warning", "FailedScale",
				"Failed to scale %s/%s from %d to %d: %v",
				pa.Namespace, pa.Spec.ScaleTargetRef.Name, specReplicas, desiredReplicas, err)
			return ctrl.Result{}, err
		}
		action := "ScaledUp"
		if desiredReplicas < specReplicas {
			action = "ScaledDown"
		}
		r.recorder.Eventf(pa, "Normal", action,
			"Scaled %s/%s from %d to %d: %s",
			pa.Namespace, pa.Spec.ScaleTargetRef.Name, specReplicas, desiredReplicas, reason)

		// Record scale action for cooldown (does NOT clear observation window samples)
		r.recordScaleAction(types.NamespacedName{Namespace: pa.Namespace, Name: pa.Name}, desiredReplicas > specReplicas)
	}

	scaled := (desiredReplicas != specReplicas)
	if err := r.updateStatus(ctx, pa, statusReplicas, desiredReplicas, available, result.appliedCronPolicies, false, scaled); err != nil {
		return ctrl.Result{}, err
	}

	// Requeue aligned with last sample time so that observeAndAggregate
	// collects a new sample on each reconcile pass.
	key := types.NamespacedName{Namespace: pa.Namespace, Name: pa.Name}
	monitor := r.getOrCreateMonitor(key)
	nextDue := monitor.getLastSampleAt().Add(time.Duration(samplingIntervalSeconds) * time.Second)
	requeueAfter := time.Until(nextDue)
	if requeueAfter <= 0 {
		requeueAfter = time.Duration(samplingIntervalSeconds) * time.Second
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// getSandboxSet fetches the target SandboxSet.
func (r *Reconciler) getSandboxSet(ctx context.Context, pa *agentsv1alpha1.PoolAutoscaler) (*agentsv1alpha1.SandboxSet, error) {
	sbs := &agentsv1alpha1.SandboxSet{}
	key := types.NamespacedName{
		Namespace: pa.Namespace,
		Name:      pa.Spec.ScaleTargetRef.Name,
	}
	if err := r.Get(ctx, key, sbs); err != nil {
		return nil, err
	}
	return sbs, nil
}

// clampToBounds enforces minReplicas and maxReplicas constraints.
func (r *Reconciler) clampToBounds(pa *agentsv1alpha1.PoolAutoscaler, desired int32) int32 {
	if desired < pa.Spec.MinReplicas {
		desired = pa.Spec.MinReplicas
	}
	if desired > pa.Spec.MaxReplicas {
		desired = pa.Spec.MaxReplicas
	}
	return desired
}

// doScale patches the SandboxSet spec.replicas.
func (r *Reconciler) doScale(ctx context.Context, sbs *agentsv1alpha1.SandboxSet, desiredReplicas int32) error {
	patch := client.MergeFrom(sbs.DeepCopy())
	sbs.Spec.Replicas = desiredReplicas
	return r.Patch(ctx, sbs, patch)
}

// updateStatus updates the PoolAutoscaler status fields.
func (r *Reconciler) updateStatus(ctx context.Context, pa *agentsv1alpha1.PoolAutoscaler, currentReplicas, desiredReplicas, available int32, appliedCronPolicies []agentsv1alpha1.CronScalingPolicyStatus, suspended bool, scaled bool) error {
	paCopy := pa.DeepCopy()
	pa.Status.ObservedGeneration = &pa.Generation
	pa.Status.CurrentReplicas = currentReplicas
	pa.Status.DesiredReplicas = desiredReplicas
	pa.Status.Suspended = suspended
	pa.Status.CurrentCapacity = agentsv1alpha1.CapacityStatus{Available: available}
	if appliedCronPolicies != nil {
		pa.Status.AppliedCronPolicies = appliedCronPolicies
	}

	if scaled {
		now := metav1.Now()
		pa.Status.LastScaleTime = &now
	}

	r.setConditions(pa, desiredReplicas)

	if reflect.DeepEqual(pa.Status, paCopy.Status) {
		return nil
	}

	patch := client.MergeFrom(paCopy)
	return r.Status().Patch(ctx, pa, patch)
}

// setConditions updates the conditions on the PoolAutoscaler.
func (r *Reconciler) setConditions(pa *agentsv1alpha1.PoolAutoscaler, desiredReplicas int32) {
	now := metav1.Now()

	setCondition(pa, metav1.Condition{
		Type:               string(agentsv1alpha1.ScalingActive),
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "ValidPolicy",
		Message:            "the autoscaler is able to scale",
	})

	setCondition(pa, metav1.Condition{
		Type:               string(agentsv1alpha1.AbleToScale),
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "ReadyToScale",
		Message:            "the autoscaler can access the target resource",
	})

	minReplicas := pa.Spec.MinReplicas
	if desiredReplicas == pa.Spec.MaxReplicas {
		setCondition(pa, metav1.Condition{
			Type:               string(agentsv1alpha1.ScalingLimited),
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "TooManyReplicas",
			Message:            fmt.Sprintf("the desired replica count is limited to the maximum %d", desiredReplicas),
		})
	} else if desiredReplicas == minReplicas && minReplicas > 0 {
		setCondition(pa, metav1.Condition{
			Type:               string(agentsv1alpha1.ScalingLimited),
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "TooFewReplicas",
			Message:            fmt.Sprintf("the desired replica count is limited to the minimum %d", desiredReplicas),
		})
	} else {
		setCondition(pa, metav1.Condition{
			Type:               string(agentsv1alpha1.ScalingLimited),
			Status:             metav1.ConditionFalse,
			LastTransitionTime: now,
			Reason:             "DesiredWithinRange",
			Message:            "the desired count is within the acceptable range",
		})
	}
}

// setCondition updates or appends a condition in the PoolAutoscaler status.
func setCondition(pa *agentsv1alpha1.PoolAutoscaler, condition metav1.Condition) {
	for i, c := range pa.Status.Conditions {
		if c.Type == condition.Type {
			if c.Status != condition.Status {
				pa.Status.Conditions[i] = condition
			} else if c.Reason != condition.Reason || c.Message != condition.Message {
				// Status unchanged but reason/message differs — update without bumping LastTransitionTime
				pa.Status.Conditions[i].Reason = condition.Reason
				pa.Status.Conditions[i].Message = condition.Message
			}
			return
		}
	}
	pa.Status.Conditions = append(pa.Status.Conditions, condition)
}
