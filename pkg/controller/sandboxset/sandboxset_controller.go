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
	"reflect"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
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

const (
	EventSandboxCreated       = "SandboxCreated"
	EventCreateSandboxFailed  = "CreateSandboxFailed"
	EventSandboxScaledDown    = "SandboxScaledDown"
	EventFailedSandboxDeleted = "FailedSandboxDeleted"
)

// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxsets/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	totalStart := time.Now()
	log := logf.FromContext(ctx).WithValues("sandboxset", req.NamespacedName)
	ctx = logf.IntoContext(ctx, log)
	sbs := &agentsv1alpha1.SandboxSet{}
	if err := r.Get(ctx, req.NamespacedName, sbs); err != nil {
		if apierrors.IsNotFound(err) {
			scaleUpExpectation.DeleteExpectations(req.String())
			scaleDownExpectation.DeleteExpectations(req.String())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Preparation
	newStatus, err := r.initNewStatus(sbs)
	if err != nil {
		log.Error(err, "failed to init new status")
		return ctrl.Result{}, err
	}

	controllerKey := GetControllerKey(sbs)
	var requeueAfter time.Duration
	var scaleUpSatisfied, scaleDownSatisfied bool
	scaleUpSatisfied, scaleUpTimeoutAfter := scaleExpectationSatisfied(ctx, scaleUpExpectation, controllerKey)
	scaleDownSatisfied, scaleDownTimeoutAfter := scaleExpectationSatisfied(ctx, scaleDownExpectation, controllerKey)
	requeueAfter = min(scaleUpTimeoutAfter, scaleDownTimeoutAfter)
	groups, err := r.groupAllSandboxes(ctx, sbs)
	if err != nil {
		log.Error(err, "failed to group sandboxes")
		return ctrl.Result{}, err
	}
	actualReplicas := saveStatusFromGroup(newStatus, groups)

	// Set selector in status for scale subresource
	if newStatus.Selector == "" {
		selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
			MatchLabels: map[string]string{
				agentsv1alpha1.LabelSandboxPool:      sbs.Name,
				agentsv1alpha1.LabelSandboxIsClaimed: "false",
			},
		})
		if err != nil {
			log.Error(err, "failed to generate selector")
		} else {
			newStatus.Selector = selector.String()
		}
	}

	var allErrors error

	// Step 1: perform scale
	start := time.Now()
	delta := int(sbs.Spec.Replicas - actualReplicas)
	if delta > 0 {
		if !scaleUpSatisfied {
			log.Info("skip scale up for scaleUpExpectation is not satisfied")
		} else {
			err = r.scaleUp(ctx, delta, sbs, newStatus.UpdateRevision)
		}
	} else if delta < 0 {
		if !scaleUpSatisfied || !scaleDownSatisfied {
			log.Info("skip scale down for scaleUpExpectation or scaleDownExpectation is not satisfied")
		} else {
			err = r.scaleDown(ctx, -delta, sbs, groups)
		}
	}
	if err != nil {
		log.Error(err, "failed to perform scale", "cost", time.Since(start))
		allErrors = errors.Join(allErrors, err)
	} else {
		log.Info("scale finished", "cost", time.Since(start))
	}

	// Step 2: delete dead sandboxes
	start = time.Now()
	if err = r.deleteDeadSandboxes(ctx, groups.Dead); err != nil {
		log.Error(err, "failed to perform garbage collection")
		allErrors = errors.Join(allErrors, err)
	} else {
		log.Info("all dead sandboxes deleted", "cost", time.Since(start))
	}
	log.Info("reconcile done", "totalCost", time.Since(totalStart))
	if err = r.updateSandboxSetStatus(ctx, *newStatus, sbs); err != nil {
		log.Error(err, "failed to update sandboxset status")
		allErrors = errors.Join(allErrors, err)
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, allErrors
}

// scaleUp is allowed when scaleUpExpectation is satisfied
func (r *Reconciler) scaleUp(ctx context.Context, count int, sbs *agentsv1alpha1.SandboxSet, revision string) error {
	log := logf.FromContext(ctx)
	log.Info("scale up", "count", count)
	successes, err := utils.DoItSlowly(count, initialBatchSize, func() error {
		created, err := r.createSandbox(ctx, sbs, revision)
		if err != nil {
			log.Error(err, "failed to create sandbox")
			return err
		}
		log.V(consts.DebugLogLevel).Info("sandbox created", "sandbox", klog.KObj(created))
		return nil
	})
	log.Info("scale up finished", "successes", successes, "fails", count-successes)
	return err
}

// scaleDown is allowed when both scaleUpExpectation and scaleDownExpectation are satisfied
func (r *Reconciler) scaleDown(ctx context.Context, count int, sbs *agentsv1alpha1.SandboxSet, groups GroupedSandboxes) error {
	log := logf.FromContext(ctx)
	controllerKey := GetControllerKey(sbs)
	lock := uuid.New().String()
	log.Info("scale down", "count", count)
	var toDelete []client.ObjectKey
	for _, snapshot := range append(groups.Creating, groups.Available...) {
		if count <= 0 {
			break
		}
		toDelete = append(toDelete, client.ObjectKeyFromObject(snapshot))
		count--
	}
	successes, err := utils.DoItSlowlyWithInputs(toDelete, initialBatchSize, func(key client.ObjectKey) error {
		scaleDownExpectation.ExpectScale(controllerKey, expectations.Delete, key.Name)
		err := r.scaleDownSandbox(ctx, key, lock)
		if err != nil {
			log.Error(err, "failed to scale down sandbox")
			scaleDownExpectation.ObserveScale(controllerKey, expectations.Delete, key.Name)
		}
		return err
	})
	log.Info("scale down finished", "success", successes, "fails", len(toDelete)-successes)
	return err

}

func (r *Reconciler) createSandbox(ctx context.Context, sbs *agentsv1alpha1.SandboxSet, revision string) (*agentsv1alpha1.Sandbox, error) {
	generateName := fmt.Sprintf("%s-", sbs.Name)
	template := sbs.Spec.Template.DeepCopy()
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: generateName,
			Namespace:    sbs.Namespace,
			Labels:       template.Labels,
			Annotations:  template.Annotations,
		},
		Spec: agentsv1alpha1.SandboxSpec{
			PersistentContents: sbs.Spec.PersistentContents,
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				TemplateRef:          sbs.Spec.TemplateRef,
				Template:             template,
				VolumeClaimTemplates: sbs.Spec.VolumeClaimTemplates,
			},
		},
	}
	sbx.Annotations = clearAndInitInnerKeys(sbx.Annotations)
	sbx.Labels = clearAndInitInnerKeys(sbx.Labels)
	sbx.Labels[agentsv1alpha1.LabelSandboxPool] = sbs.Name
	sbx.Labels[agentsv1alpha1.LabelSandboxTemplate] = sbs.Name
	sbx.Labels[agentsv1alpha1.LabelSandboxIsClaimed] = "false"
	sbx.Labels[agentsv1alpha1.LabelTemplateHash] = revision
	if sbs.Spec.TemplateRef != nil {
		sbx.Labels[agentsv1alpha1.LabelSandboxTemplate] = sbs.Spec.TemplateRef.Name
	} else {
		sbx.Labels[agentsv1alpha1.LabelSandboxTemplate] = sbs.Name
	}
	if err := ctrl.SetControllerReference(sbs, sbx, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, sbx); err != nil {
		r.Recorder.Eventf(sbs, corev1.EventTypeWarning, EventCreateSandboxFailed, "Failed to create sandbox: %s", err)
		return nil, err
	}
	scaleUpExpectation.ExpectScale(GetControllerKey(sbs), expectations.Create, sbx.Name)
	r.Recorder.Eventf(sbs, corev1.EventTypeNormal, EventSandboxCreated, "Sandbox %s created", klog.KObj(sbx))
	return sbx, nil
}

func (r *Reconciler) scaleDownSandbox(ctx context.Context, key client.ObjectKey, lock string) (err error) {
	log := logf.FromContext(ctx).WithValues("sandbox", key).V(consts.DebugLogLevel)
	sbx := &agentsv1alpha1.Sandbox{}
	log.Info("try to scale down sandbox")
	if err = r.Get(ctx, key, sbx); err != nil {
		return err
	}
	if sbx.Annotations[agentsv1alpha1.AnnotationLock] != "" && sbx.Annotations[agentsv1alpha1.AnnotationOwner] != consts.OwnerManagerScaleDown {
		log.Info("sandbox to be scaled down claimed before performed, skip")
		return errors.New("sandbox to be scaled down claimed before performed, skip")
	}
	managerutils.LockSandbox(sbx, lock, consts.OwnerManagerScaleDown)
	if err = r.Update(ctx, sbx); err != nil {
		return fmt.Errorf("failed to lock sandbox when scaling down: %s", err)
	}
	if err = r.Delete(ctx, sbx); err != nil {
		log.Error(err, "failed to delete sandbox")
		return err
	}
	log.Info("sandbox locked and deleted")
	r.Recorder.Eventf(sbx, corev1.EventTypeNormal, EventSandboxScaledDown, "Sandbox %s locked and deleted", klog.KObj(sbx))
	return nil
}

// deleteDeadSandboxes does not need to use ScaleExpectation, because this is a garbage collection logic that does not
// require maintaining replica counts (or rather, only needs to maintain the dead group's replica count at 0), so just
// delete all dead sandboxes.
func (r *Reconciler) deleteDeadSandboxes(ctx context.Context, dead []*agentsv1alpha1.Sandbox) error {
	log := logf.FromContext(ctx).V(consts.DebugLogLevel)
	failNum := 0
	for _, sbx := range dead {
		if sbx.DeletionTimestamp != nil {
			continue
		}
		if err := r.Delete(ctx, sbx); err != nil {
			log.Error(err, "failed to delete sandbox")
			failNum++
		}
		log.Info("sandbox deleted", "sandbox", klog.KObj(sbx))
		r.Recorder.Eventf(sbx, corev1.EventTypeNormal, EventFailedSandboxDeleted, "Sandbox %s deleted", klog.KObj(sbx))
	}
	if failNum > 0 {
		return fmt.Errorf("failed to delete %d sandboxes", failNum)
	}
	return nil
}

func (r *Reconciler) updateSandboxSetStatus(ctx context.Context, newStatus agentsv1alpha1.SandboxSetStatus, sbs *agentsv1alpha1.SandboxSet) error {
	log := logf.FromContext(ctx).V(consts.DebugLogLevel)
	clone := sbs.DeepCopy()
	if err := r.Get(ctx, client.ObjectKey{Namespace: sbs.Namespace, Name: sbs.Name}, clone); err != nil {
		log.Error(err, "failed to get updated sandboxset from client")
		return client.IgnoreNotFound(err)
	}
	if reflect.DeepEqual(clone.Status, newStatus) {
		return nil
	}
	clone.Status = newStatus
	err := r.Status().Update(ctx, clone)
	if err == nil {
		log.Info("update sandboxset status success", "status", utils.DumpJson(newStatus))
	} else {
		log.Error(err, "update sandboxset status failed")
	}
	return err
}

func (r *Reconciler) groupAllSandboxes(ctx context.Context, sbs *agentsv1alpha1.SandboxSet) (GroupedSandboxes, error) {
	log := logf.FromContext(ctx)
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
		debugLog := log.V(consts.DebugLogLevel).WithValues("sandbox", sbx.Name)
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
		debugLog.Info("sandbox is grouped", "state", state, "reason", reason)
	}
	log.Info("sandbox group done", "total", len(sandboxList.Items), "creating", len(groups.Creating),
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
