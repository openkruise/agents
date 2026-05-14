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

package sandboxset

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/events"
	"github.com/openkruise/agents/pkg/discovery"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
	"github.com/openkruise/agents/pkg/utils/fieldindex"
	managerutils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
)

func init() {
	flag.IntVar(&concurrentReconciles, "sandboxset-workers", concurrentReconciles, "Max concurrent workers for SandboxSet controller.")
	flag.IntVar(&initialBatchSize, "sandboxset-initial-batch-size", initialBatchSize, "The initial batch size to use for the api-server operation")
}

var (
	concurrentReconciles = 3
	initialBatchSize     = 16
	controllerKind       = agentsv1alpha1.GroupVersion.WithKind("SandboxSet")
)

func Add(mgr manager.Manager) error {
	if !utilfeature.DefaultFeatureGate.Enabled(features.SandboxSetGate) || !discovery.DiscoverGVK(controllerKind) {
		return nil
	}
	err := (&Reconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
	if err != nil {
		return err
	}
	klog.Infof("Started SandboxSetReconciler successfully")
	return nil
}

// Reconciler reconciles a Sandbox object
type Reconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Codec    runtime.Codec
}

// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxsets/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	totalStart := time.Now()
	klog.InfoS("Reconciling SandboxSet", "sandboxSet", req.NamespacedName)
	sbs := &agentsv1alpha1.SandboxSet{}
	if err := r.Get(ctx, req.NamespacedName, sbs); err != nil {
		if apierrors.IsNotFound(err) {
			scaleUpExpectation.DeleteExpectations(req.String())
			scaleDownExpectation.DeleteExpectations(req.String())
			// Remove metrics when sandboxset is deleted
			deleteSandboxSetMetrics(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	recordSandboxSetMetrics(sbs)

	// Preparation
	newStatus, err := r.initNewStatus(ctx, sbs)
	if err != nil {
		klog.ErrorS(err, "Failed to init new status", "sandboxSet", klog.KObj(sbs))
		return ctrl.Result{}, err
	}

	controllerKey := GetControllerKey(sbs)
	groups, err := r.groupAllSandboxes(ctx, sbs)
	if err != nil {
		klog.ErrorS(err, "Failed to group sandboxes", "sandboxSet", klog.KObj(sbs))
		return ctrl.Result{}, err
	}
	var requeueAfter time.Duration
	scaleUpSatisfied, dirtyScaleUp, scaleUpTimeoutAfter := scaleExpectationSatisfied(ctx, scaleUpExpectation, controllerKey)
	scaleDownSatisfied, _, scaleDownTimeoutAfter := scaleExpectationSatisfied(ctx, scaleDownExpectation, controllerKey)
	requeueAfter = min(scaleUpTimeoutAfter, scaleDownTimeoutAfter)

	calculateSandboxSetStatusFromGroup(ctx, newStatus, groups, dirtyScaleUp)
	// Set selector in status for scale subresource
	if newStatus.Selector == "" {
		selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
			MatchLabels: map[string]string{
				agentsv1alpha1.LabelSandboxPool:      sbs.Name,
				agentsv1alpha1.LabelSandboxIsClaimed: "false",
			},
		})
		if err != nil {
			klog.ErrorS(err, "Failed to generate selector", "sandboxSet", klog.KObj(sbs))
		} else {
			newStatus.Selector = selector.String()
		}
	}

	var allErrors error
	// Step 1: perform scale
	start := time.Now()
	delta := calculateScaleDelta(sbs, newStatus)
	klog.InfoS("Performing scale", "sandboxSet", klog.KObj(sbs), "expect", sbs.Spec.Replicas, "actual", newStatus.Replicas,
		"available", newStatus.AvailableReplicas, "delta", delta)
	if delta > 0 {
		err = r.scaleUp(ctx, delta, sbs, newStatus.UpdateRevision)
	} else if delta < 0 {
		if !scaleUpSatisfied || !scaleDownSatisfied {
			klog.InfoS("Skip scale down for expectations not satisfied", "sandboxSet", klog.KObj(sbs))
		} else {
			err = r.scaleDown(ctx, -delta, sbs, groups, newStatus.UpdateRevision)
		}
	}
	if err != nil {
		klog.ErrorS(err, "Failed to perform scale", "sandboxSet", klog.KObj(sbs), "cost", time.Since(start))
		allErrors = errors.Join(allErrors, err)
	} else {
		klog.InfoS("Scale finished", "sandboxSet", klog.KObj(sbs), "cost", time.Since(start))
	}

	// Step 2: delete dead sandboxes
	start = time.Now()
	if err = r.deleteDeadSandboxes(ctx, groups.Dead); err != nil {
		klog.ErrorS(err, "Failed to perform garbage collection", "sandboxSet", klog.KObj(sbs))
		allErrors = errors.Join(allErrors, err)
	} else {
		klog.InfoS("All dead sandboxes deleted", "sandboxSet", klog.KObj(sbs), "cost", time.Since(start))
	}

	// Step 3: perform rolling update if needed
	// update groups because status may change after scale
	if delta == 0 && scaleUpSatisfied && scaleDownSatisfied {
		updateGroups := buildUpdateGroups(groups, newStatus.UpdateRevision)
		if updateGroups == nil {
			klog.InfoS("Skip rolling update: scale expectations not satisfied", "sandboxSet", klog.KObj(sbs))
		} else if needsUpdate(updateGroups) {
			start = time.Now()
			updateInfo := calculateUpdateInfo(sbs, updateGroups)
			// Update status with update progress
			newStatus.UpdatedReplicas = int32(updateInfo.CurrentUpdated)
			newStatus.UpdatedAvailableReplicas = int32(len(updateGroups.UpdatedAvailable))

			if !isUpdateComplete(updateInfo) {
				klog.InfoS("Performing rolling update", "sandboxSet", klog.KObj(sbs), "toUpdate", updateInfo.ToUpdate)
				r.Recorder.Eventf(sbs, corev1.EventTypeNormal, events.SandboxSetRollingUpdate, "Rolling update started, %d sandboxes to update", updateInfo.ToUpdate)
				deleted, err := r.performRollingUpdate(ctx, sbs, updateGroups, updateInfo)
				if err != nil {
					klog.ErrorS(err, "Failed to perform rolling update", "sandboxSet", klog.KObj(sbs))
					allErrors = errors.Join(allErrors, err)
				} else {
					klog.InfoS("Rolling update step finished", "sandboxSet", klog.KObj(sbs), "deleted", deleted, "cost", time.Since(start))
				}
			}
		} else {
			// All sandboxes are up to date
			newStatus.UpdatedReplicas = newStatus.Replicas
			newStatus.UpdatedAvailableReplicas = newStatus.AvailableReplicas
			// Only emit completion event when transitioning from an in-progress rolling update
			if sbs.Status.UpdatedReplicas < sbs.Status.Replicas {
				klog.InfoS("All sandboxes are up to date", "sandboxSet", klog.KObj(sbs))
				r.Recorder.Eventf(sbs, corev1.EventTypeNormal, events.SandboxSetRollingUpdateCompleted,
					"Rolling update completed, all sandboxes are up to date")
			}
		}
	}

	klog.InfoS("Reconcile done", "sandboxSet", klog.KObj(sbs), "totalCost", time.Since(totalStart))
	if err = r.updateSandboxSetStatus(ctx, *newStatus, sbs); err != nil {
		klog.ErrorS(err, "Failed to update sandboxset status", "sandboxSet", klog.KObj(sbs))
		allErrors = errors.Join(allErrors, err)
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, allErrors
}

// scaleUp is allowed when scaleUpExpectation is satisfied
func (r *Reconciler) scaleUp(ctx context.Context, count int, sbs *agentsv1alpha1.SandboxSet, revision string) error {
	klog.InfoS("Scaling up SandboxSet", "sandboxSet", klog.KObj(sbs), "count", count)
	r.Recorder.Eventf(sbs, corev1.EventTypeNormal, events.SandboxSetScalingUp, "Scaling up SandboxSet from %d to %d replicas", int(sbs.Spec.Replicas)-count, int(sbs.Spec.Replicas))
	successes, err := utils.DoItSlowly(count, initialBatchSize, func() error {
		created, err := r.createSandbox(ctx, sbs, revision)
		if err != nil {
			klog.ErrorS(err, "Failed to create sandbox", "sandboxSet", klog.KObj(sbs))
			return err
		}
		klog.V(consts.DebugLogLevel).InfoS("Sandbox created", "sandboxSet", klog.KObj(sbs), "sandbox", klog.KObj(created))
		return nil
	})
	klog.InfoS("Scale up finished", "sandboxSet", klog.KObj(sbs), "successes", successes, "fails", count-successes)
	return err
}

// scaleDown is allowed when both scaleUpExpectation and scaleDownExpectation are satisfied.
// It prioritizes deleting old revision sandboxes first, then updated ones.
func (r *Reconciler) scaleDown(ctx context.Context, count int, sbs *agentsv1alpha1.SandboxSet, groups GroupedSandboxes, updateRevision string) error {
	controllerKey := GetControllerKey(sbs)
	lock := uuid.New().String()
	klog.InfoS("Scaling down SandboxSet", "sandboxSet", klog.KObj(sbs), "count", count)
	r.Recorder.Eventf(sbs, corev1.EventTypeNormal, events.SandboxSetScalingDown, "Scaling down SandboxSet from %d to %d replicas", int(sbs.Spec.Replicas)+count, int(sbs.Spec.Replicas))

	// Separate candidates into old revision and updated revision.
	candidates := append(groups.Creating, groups.Available...)
	var oldCandidates, updatedCandidates []*agentsv1alpha1.Sandbox
	for _, sbx := range candidates {
		if sbx.Labels[agentsv1alpha1.LabelTemplateHash] != updateRevision {
			oldCandidates = append(oldCandidates, sbx)
		} else {
			updatedCandidates = append(updatedCandidates, sbx)
		}
	}

	deleteFunc := func(sbx *agentsv1alpha1.Sandbox) error {
		key := client.ObjectKeyFromObject(sbx)
		scaleDownExpectation.ExpectScale(controllerKey, expectations.Delete, key.Name)
		err := r.scaleDownSandbox(ctx, sbx, lock)
		if err != nil {
			klog.ErrorS(err, "Failed to scale down sandbox", "sandboxSet", klog.KObj(sbs), "sandbox", key.Name)
			scaleDownExpectation.ObserveScale(controllerKey, expectations.Delete, key.Name)
		}
		return err
	}

	// Phase 1: Delete old revision sandboxes first
	oldToDelete := oldCandidates[:min(count, len(oldCandidates))]
	var totalSuccesses int
	successes, err := utils.DoItSlowlyWithInputs(oldToDelete, initialBatchSize, deleteFunc)
	totalSuccesses += successes
	if err != nil {
		klog.InfoS("Scale down finished with errors", "sandboxSet", klog.KObj(sbs), "success", totalSuccesses, "fails", len(oldToDelete)-successes)
		return err
	}

	remaining := count - len(oldToDelete)
	if remaining <= 0 {
		return nil
	}

	// Phase 2: Delete updated revision sandboxes if more needed
	updatedToDelete := updatedCandidates[:min(remaining, len(updatedCandidates))]
	successes, err = utils.DoItSlowlyWithInputs(updatedToDelete, initialBatchSize, deleteFunc)
	totalSuccesses += successes
	if err != nil {
		klog.InfoS("Scale down finished with errors", "sandboxSet", klog.KObj(sbs), "success", totalSuccesses, "fails", len(updatedToDelete)-successes)
		return err
	}

	klog.InfoS("Scale down finished", "sandboxSet", klog.KObj(sbs), "success", totalSuccesses)
	return nil
}

// calculateScaleDelta calculates the delta for scaling, considering MaxUnavailable limit.
// Returns positive value for scale up, negative for scale down, 0 for no scaling needed.
func calculateScaleDelta(sbs *agentsv1alpha1.SandboxSet, newStatus *agentsv1alpha1.SandboxSetStatus) int {
	delta := int(sbs.Spec.Replicas - newStatus.Replicas)
	// scale down
	if delta <= 0 {
		return delta
	}

	// apply maxUnavailable limit only for scale up
	scaleMaxUnavailable := math.MaxInt
	if sbs.Spec.ScaleStrategy.MaxUnavailable != nil {
		scaleMaxUnavailable, _ = intstrutil.GetScaledValueFromIntOrPercent(
			intstrutil.ValueOrDefault(sbs.Spec.ScaleStrategy.MaxUnavailable, intstrutil.FromInt32(math.MaxInt32)),
			int(sbs.Spec.Replicas),
			true)
		// subtract sandboxes that are currently being creating
		scaleMaxUnavailable -= int(newStatus.Replicas - newStatus.AvailableReplicas)
	}
	// ignore negative values
	if scaleMaxUnavailable < 0 {
		scaleMaxUnavailable = 0
	}
	// delta cannot exceed scaleMaxUnavailable
	if delta > scaleMaxUnavailable {
		delta = scaleMaxUnavailable
	}

	return delta
}

func (r *Reconciler) createSandbox(ctx context.Context, sbs *agentsv1alpha1.SandboxSet, revision string) (*agentsv1alpha1.Sandbox, error) {
	var refTemplate *agentsv1alpha1.SandboxTemplate
	if sbs.Spec.TemplateRef != nil {
		refTemplate = &agentsv1alpha1.SandboxTemplate{}
		if err := r.Get(ctx, client.ObjectKey{
			Namespace: sbs.Namespace,
			Name:      sbs.Spec.TemplateRef.Name,
		}, refTemplate); err != nil {
			r.Recorder.Eventf(sbs, corev1.EventTypeWarning, events.CreateSandboxFailed, "Failed to resolve sandbox template: %s", err)
			return nil, fmt.Errorf("failed to resolve sandbox template %s/%s: %w",
				sbs.Namespace, sbs.Spec.TemplateRef.Name, err)
		}
	}
	sbx := NewSandboxFromSandboxSet(sbs, refTemplate)
	sbx.Labels[agentsv1alpha1.LabelTemplateHash] = revision
	if err := ctrl.SetControllerReference(sbs, sbx, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, sbx); err != nil {
		r.Recorder.Eventf(sbs, corev1.EventTypeWarning, events.CreateSandboxFailed, "Failed to create sandbox: %s", err)
		return nil, err
	}
	scaleUpExpectation.ExpectScale(GetControllerKey(sbs), expectations.Create, sbx.Name)
	r.Recorder.Eventf(sbs, corev1.EventTypeNormal, events.SandboxCreated, "Sandbox %s created", klog.KObj(sbx))
	return sbx, nil
}

func (r *Reconciler) scaleDownSandbox(ctx context.Context, sbx *agentsv1alpha1.Sandbox, lock string) (err error) {
	klog.V(consts.DebugLogLevel).InfoS("Try to scale down sandbox", "sandbox", klog.KObj(sbx))
	if sbx.Annotations[agentsv1alpha1.AnnotationLock] != "" && sbx.Annotations[agentsv1alpha1.AnnotationOwner] != consts.OwnerManagerScaleDown {
		klog.V(consts.DebugLogLevel).InfoS("Sandbox to be scaled down claimed before performed, skip", "sandbox", klog.KObj(sbx))
		return errors.New("sandbox to be scaled down claimed before performed, skip")
	}
	managerutils.LockSandbox(sbx, lock, consts.OwnerManagerScaleDown)
	if err = r.Update(ctx, sbx); err != nil {
		return fmt.Errorf("failed to lock sandbox when scaling down: %s", err)
	}
	if err = r.Delete(ctx, sbx); err != nil {
		klog.ErrorS(err, "Failed to delete sandbox", "sandbox", klog.KObj(sbx))
		return err
	}
	klog.V(consts.DebugLogLevel).InfoS("Sandbox locked and deleted", "sandbox", klog.KObj(sbx))
	r.Recorder.Eventf(sbx, corev1.EventTypeNormal, events.SandboxScaledDown, "Sandbox %s locked and deleted", klog.KObj(sbx))
	return nil
}

// deleteDeadSandboxes does not need to use ScaleExpectation, because this is a garbage collection logic that does not
// require maintaining replica counts (or rather, only needs to maintain the dead group's replica count at 0), so just
// delete all dead sandboxes.
func (r *Reconciler) deleteDeadSandboxes(ctx context.Context, dead []*agentsv1alpha1.Sandbox) error {
	failNum := 0
	for _, sbx := range dead {
		if sbx.DeletionTimestamp != nil {
			continue
		}
		if err := r.Delete(ctx, sbx); err != nil {
			klog.ErrorS(err, "Failed to delete sandbox", "sandbox", klog.KObj(sbx))
			failNum++
		}
		klog.V(consts.DebugLogLevel).InfoS("Sandbox deleted", "sandbox", klog.KObj(sbx))
		r.Recorder.Eventf(sbx, corev1.EventTypeNormal, events.FailedSandboxDeleted, "Sandbox %s deleted", klog.KObj(sbx))
	}
	if failNum > 0 {
		return fmt.Errorf("failed to delete %d sandboxes", failNum)
	}
	return nil
}

func (r *Reconciler) updateSandboxSetStatus(ctx context.Context, newStatus agentsv1alpha1.SandboxSetStatus, sbs *agentsv1alpha1.SandboxSet) error {
	clone := sbs.DeepCopy()
	if err := r.Get(ctx, client.ObjectKey{Namespace: sbs.Namespace, Name: sbs.Name}, clone); err != nil {
		klog.ErrorS(err, "Failed to get updated sandboxset from client", "sandboxSet", klog.KObj(sbs))
		return client.IgnoreNotFound(err)
	}
	if reflect.DeepEqual(clone.Status, newStatus) {
		return nil
	}
	clone.Status = newStatus
	err := r.Status().Update(ctx, clone)
	if err == nil {
		klog.V(consts.DebugLogLevel).InfoS("Update sandboxset status success", "sandboxSet", klog.KObj(sbs), "status", utils.DumpJson(newStatus))
		// Update metrics for availableReplicas and replicas
		sandboxSetReplicas.WithLabelValues(sbs.Namespace, sbs.Name).Set(float64(newStatus.Replicas))
		sandboxSetAvailableReplicas.WithLabelValues(sbs.Namespace, sbs.Name).Set(float64(newStatus.AvailableReplicas))
		sandboxSetDesiredReplicas.WithLabelValues(sbs.Namespace, sbs.Name).Set(float64(sbs.Spec.Replicas))
		sandboxSetUpdatedReplicas.WithLabelValues(sbs.Namespace, sbs.Name).Set(float64(newStatus.UpdatedReplicas))
		sandboxSetUpdatedAvailableReplicas.WithLabelValues(sbs.Namespace, sbs.Name).Set(float64(newStatus.UpdatedAvailableReplicas))
	} else {
		klog.ErrorS(err, "Update sandboxset status failed", "sandboxSet", klog.KObj(sbs))
	}
	return err
}

func (r *Reconciler) groupAllSandboxes(ctx context.Context, sbs *agentsv1alpha1.SandboxSet) (GroupedSandboxes, error) {
	sandboxList := &agentsv1alpha1.SandboxList{}
	if err := r.List(ctx, sandboxList,
		client.InNamespace(sbs.Namespace),
		client.MatchingFields{fieldindex.IndexNameForOwnerRefUID: string(sbs.UID)},
		client.UnsafeDisableDeepCopy,
	); err != nil {
		return GroupedSandboxes{}, err
	}
	groups := GroupedSandboxes{}
	for i := range sandboxList.Items {
		sbx := &sandboxList.Items[i]
		scaleUpExpectation.ObserveScale(GetControllerKey(sbs), expectations.Create, sbx.Name)
		state, reason := stateutils.GetSandboxState(sbx)
		switch state {
		case agentsv1alpha1.SandboxStateCreating:
			groups.Creating = append(groups.Creating, sbx)
		case agentsv1alpha1.SandboxStateAvailable:
			groups.Available = append(groups.Available, sbx)
		case agentsv1alpha1.SandboxStateRunning:
			fallthrough
		case agentsv1alpha1.SandboxStatePaused:
			groups.Used = append(groups.Used, sbx)
		case agentsv1alpha1.SandboxStateDead:
			groups.Dead = append(groups.Dead, sbx)
		default: // unknown, impossible, just in case
			return GroupedSandboxes{}, fmt.Errorf("cannot find state for sandbox %s", sbx.Name)
		}
		klog.V(consts.DebugLogLevel).InfoS("Sandbox is grouped", "sandboxSet", klog.KObj(sbs), "sandbox", sbx.Name, "state", state, "reason", reason)
	}
	klog.InfoS("Sandbox group done", "sandboxSet", klog.KObj(sbs), "total", len(sandboxList.Items), "creating", len(groups.Creating),
		"available", len(groups.Available), "used", len(groups.Used), "failed", len(groups.Dead))
	return groups, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	controllerName := "sandboxset-controller"
	r.Recorder = mgr.GetEventRecorderFor(controllerName)
	r.Codec = serializer.NewCodecFactory(mgr.GetScheme()).LegacyCodec(agentsv1alpha1.SchemeGroupVersion)
	return ctrl.NewControllerManagedBy(mgr).
		Named(controllerName).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentReconciles}).
		Watches(&agentsv1alpha1.SandboxSet{}, &handler.EnqueueRequestForObject{}).
		Watches(&agentsv1alpha1.Sandbox{}, &SandboxEventHandler{}).
		Complete(r)
}
