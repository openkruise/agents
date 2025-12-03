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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/google/uuid"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"github.com/openkruise/agents/pkg/utils/fieldindex"
	managerutils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

func init() {
	flag.IntVar(&concurrentReconciles, "sandboxset-workers", concurrentReconciles, "Max concurrent workers for SandboxSet controller.")
	flag.IntVar(&initialBatchSize, "sandboxset-initial-batch-size", initialBatchSize, "The initial batch size to use for the api-server operation")
}

var (
	concurrentReconciles     = 3
	initialBatchSize         = 16
	scaleUpCooldown          = 5 * time.Second
	sandboxSetControllerKind = agentsv1alpha1.GroupVersion.WithKind("SandboxSet")
	scaleUpRecord            sync.Map
)

func Add(mgr manager.Manager) error {
	err := (&Reconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)
	if err != nil {
		return err
	}
	klog.Infof("start SandboxSetReconciler success")
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
	EventSandboxAvailable     = "SandboxAvailable"
	EventSandboxCreated       = "SandboxCreated"
	EventSandboxScaledDown    = "SandboxScaledDown"
	EventSandboxReleased      = "SandboxReleased"
	EventFailedSandboxDeleted = "FailedSandboxDeleted"
)

// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxsets/finalizers,verbs=update

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	totalStart := time.Now()
	log := logf.FromContext(ctx).WithValues("sandboxset", req.NamespacedName)
	ctx = logf.IntoContext(ctx, log)
	sbs := &agentsv1alpha1.SandboxSet{}
	if err := r.Get(ctx, req.NamespacedName, sbs); err != nil {
		if apierrors.IsNotFound(err) {
			scaleExpectation.DeleteExpectations(req.String())
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
	groups, err := r.groupAllSandboxes(ctx, sbs)
	if err != nil {
		log.Error(err, "failed to group sandboxes")
		return ctrl.Result{}, err
	}
	actualReplicas := saveStatusFromGroup(newStatus, groups)

	var allErrors error
	// Step 1: release control of used sandboxes
	start := time.Now()
	if err = r.releaseControlOfUsedSandboxes(ctx, groups.Used, sbs); err != nil {
		log.Error(err, "failed to release control of used sandboxes")
		allErrors = errors.Join(allErrors, err)
	} else {
		log.Info("all used sandboxes released", "cost", time.Since(start))
	}

	// Step 2: process created sandboxes
	start = time.Now()
	if err = r.processCreatedSandboxes(ctx, groups.Creating, sbs); err != nil {
		log.Error(err, "failed to process creating sandboxes")
		allErrors = errors.Join(allErrors, err)
	} else {
		log.Info("all created sandboxes patched as available", "cost", time.Since(start))
	}

	// Step 3: perform scale
	var requeueAfter time.Duration
	controllerKey := GetControllerKey(sbs)
	satisfied, unsatisfiedDuration, dirty := scaleExpectation.SatisfiedExpectations(controllerKey)
	if satisfied {
		start = time.Now()
		newStatus.Replicas, requeueAfter, err = r.performScale(ctx, groups, sbs.Spec.Replicas, actualReplicas, sbs, newStatus.UpdateRevision)
		if err != nil {
			log.Error(err, "failed to perform scale")
			allErrors = errors.Join(allErrors, err)
		} else {
			log.Info("scale finished", "statusReplicas", newStatus.Replicas, "cost", time.Since(start))
		}
	} else {
		if unsatisfiedDuration > expectations.ExpectationTimeout {
			requeueAfter = 10 * time.Second
			scaleExpectation.DeleteExpectations(controllerKey)
			log.Info("expectation unsatisfied overtime, force delete the timeout expectation", "requeueAfter", requeueAfter)
		} else {
			requeueAfter = expectations.ExpectationTimeout - unsatisfiedDuration
			log.Info("skip perform scale to wait for expectations to be satisfied",
				"dirty", dirty, "requeueAfter", requeueAfter)
		}
	}

	// Step 4: delete failed sandboxes
	start = time.Now()
	if err = r.deleteFailedSandboxes(ctx, groups.Failed); err != nil {
		log.Error(err, "failed to perform garbage collection")
		allErrors = errors.Join(allErrors, err)
	} else {
		log.Info("all failed sandboxes deleted", "cost", time.Since(start))
	}
	log.Info("reconcile done", "totalCost", time.Since(totalStart))
	if err = r.updateSandboxSetStatus(ctx, *newStatus, sbs); err != nil {
		log.Error(err, "failed to update sandboxset status")
		allErrors = errors.Join(allErrors, err)
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, allErrors
}

func (r *Reconciler) releaseControlOfUsedSandboxes(ctx context.Context, used []*agentsv1alpha1.Sandbox, sbs *agentsv1alpha1.SandboxSet) error {
	usedSandboxes := make(chan client.ObjectKey, len(used))
	for _, sbx := range used {
		usedSandboxes <- client.ObjectKeyFromObject(sbx)
	}
	_, err := utils.DoItSlowly(len(used), initialBatchSize, func() error {
		key := <-usedSandboxes
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			sbx := &agentsv1alpha1.Sandbox{}
			if err := r.Get(ctx, key, sbx); err != nil {
				return err
			}
			log := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
			for i, ownerReference := range sbx.GetOwnerReferences() {
				if ownerReference.UID == sbs.UID {
					if err := r.removeOwnerReference(ctx, i, sbx); err != nil {
						log.Error(err, "failed to remove owner reference of sandbox")
						return err
					}
					log.Info("sandbox released")
					r.Recorder.Eventf(sbs, corev1.EventTypeNormal, EventSandboxReleased, "Sandbox control %s is released for being used", klog.KObj(sbx))
					break
				}
			}
			return nil
		})
	})
	return err
}

func (r *Reconciler) removeOwnerReference(ctx context.Context, idx int, sbx *agentsv1alpha1.Sandbox) error {
	if idx < 0 || idx >= len(sbx.OwnerReferences) {
		return fmt.Errorf("index out of range: %d", idx)
	}
	// Remove the owner reference at the specified index
	if idx == len(sbx.OwnerReferences)-1 {
		sbx.OwnerReferences = sbx.OwnerReferences[:idx]
	} else {
		sbx.OwnerReferences = append(sbx.OwnerReferences[:idx], sbx.OwnerReferences[idx+1:]...)
	}
	return r.Update(ctx, sbx)
}

func (r *Reconciler) processCreatedSandboxes(ctx context.Context, creating []*agentsv1alpha1.Sandbox, sbs *agentsv1alpha1.SandboxSet) error {
	now := time.Now()
	creatingSandboxes := make(chan client.ObjectKey, len(creating))
	for _, sbx := range creating {
		creatingSandboxes <- client.ObjectKeyFromObject(sbx)
	}
	_, err := utils.DoItSlowly(len(creating), initialBatchSize, func() error {
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			key := <-creatingSandboxes
			sbx := &agentsv1alpha1.Sandbox{}
			if err := r.Get(ctx, key, sbx); err != nil {
				return err
			}
			log := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
			cond := utils.GetSandboxCondition(&sbx.Status, string(agentsv1alpha1.SandboxConditionReady))
			if cond != nil && cond.Status == metav1.ConditionTrue {
				if err := r.initCreatedSandbox(ctx, sbx); err != nil {
					log.Error(err, "failed to patch sandbox")
					return err
				}
				log.Info("sandbox is available",
					"readyCost", cond.LastTransitionTime.Sub(sbx.CreationTimestamp.Time),
					"processedAfterReady", now.Sub(cond.LastTransitionTime.Time))
				r.Recorder.Eventf(sbs, corev1.EventTypeNormal, EventSandboxAvailable, "Sandbox %s is available", klog.KObj(sbx))
			}
			return nil
		})
	})
	return err
}

func (r *Reconciler) performScale(ctx context.Context, groups GroupedSandboxes, expectReplicas, actualReplicas int32,
	sbs *agentsv1alpha1.SandboxSet, revision string) (int32, time.Duration, error) {
	log := logf.FromContext(ctx)
	statusReplicas := actualReplicas
	key := GetControllerKey(sbs)
	if offset := expectReplicas - actualReplicas; offset > 0 {
		log.Info("scale up", "offset", offset)
		successes, err := utils.DoItSlowly(int(offset), initialBatchSize, func() error {
			created, err := r.createSandbox(ctx, sbs, revision)
			if err != nil {
				log.Error(err, "failed to create sandbox")
				return err
			}
			log.V(consts.DebugLogLevel).Info("sandbox created", "sandbox", klog.KObj(created))
			return nil
		})
		scaleUpRecord.Store(key, time.Now())
		return statusReplicas + int32(successes), 0, err
	} else if offset < 0 {
		var lastScaleUp time.Time
		if value, ok := scaleUpRecord.Load(key); ok {
			lastScaleUp = value.(time.Time)
		}
		if time.Since(lastScaleUp) < scaleUpCooldown {
			requeueAfter := scaleUpCooldown - time.Since(lastScaleUp)
			log.Info("skip scale down for just scaled up", "requeueAfter", requeueAfter)
			return statusReplicas, requeueAfter, nil
		} else {
			scaleUpRecord.Delete(key)
			lock := uuid.New().String()
			log.Info("scale down", "offset", offset)
			for _, snapshot := range append(groups.Creating, groups.Available...) {
				if offset >= 0 {
					break
				}
				deleted, err := r.scaleDownSandbox(ctx, client.ObjectKeyFromObject(snapshot), lock)
				if err != nil {
					log.Error(err, "failed to scale down sandbox")
					return statusReplicas, 0, err
				}
				if deleted {
					statusReplicas--
					offset++
				}
			}
		}
	}
	return statusReplicas, 0, nil
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
			Template:           *template,
			PersistentContents: sbs.Spec.PersistentContents,
		},
	}
	sbx.Annotations = clearAndInitInnerKeys(sbx.Annotations)
	sbx.Labels = clearAndInitInnerKeys(sbx.Labels)
	sbx.Labels[agentsv1alpha1.LabelSandboxPool] = sbs.Name
	sbx.Labels[agentsv1alpha1.LabelTemplateHash] = revision
	if err := ctrl.SetControllerReference(sbs, sbx, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, sbx); err != nil {
		return nil, err
	}
	scaleExpectation.ExpectScale(GetControllerKey(sbs), expectations.Create, sbx.Name)
	r.Recorder.Eventf(sbs, corev1.EventTypeNormal, EventSandboxCreated, "Sandbox %s created", klog.KObj(sbx))
	return sbx, nil
}

func (r *Reconciler) scaleDownSandbox(ctx context.Context, key client.ObjectKey, lock string) (deleted bool, err error) {
	log := logf.FromContext(ctx).WithValues("sandbox", key).V(consts.DebugLogLevel)
	sbx := &agentsv1alpha1.Sandbox{}
	log.Info("try to scale down sandbox")
	if err = r.Get(ctx, key, sbx); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if sbx.Annotations[agentsv1alpha1.AnnotationLock] != "" && sbx.Annotations[agentsv1alpha1.AnnotationOwner] != consts.OwnerManager {
		log.Info("sandbox to be scaled down claimed before performed, skip")
		return false, nil
	}
	managerutils.LockSandbox(sbx, lock, consts.OwnerManager)
	if sbx.Labels == nil {
		sbx.Labels = make(map[string]string, 1)
	}
	sbx.Labels[agentsv1alpha1.LabelSandboxState] = agentsv1alpha1.SandboxStateKilling
	if err = r.Update(ctx, sbx); err != nil {
		if apierrors.IsConflict(err) {
			return false, nil // skip
		}
		return false, fmt.Errorf("failed to lock sandbox when scaling down: %s", err)
	}
	log.Info("sandbox locked and set to killing")
	r.Recorder.Eventf(sbx, corev1.EventTypeNormal, EventSandboxScaledDown, "Sandbox %s will be scaled down", klog.KObj(sbx))
	return true, nil
}

func (r *Reconciler) deleteFailedSandboxes(ctx context.Context, failed []*agentsv1alpha1.Sandbox) error {
	log := logf.FromContext(ctx).V(consts.DebugLogLevel)
	failNum := 0
	for _, sbx := range failed {
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
		scaleExpectation.ObserveScale(GetControllerKey(sbs), expectations.Create, sbx.Name)
		debugLog := log.V(consts.DebugLogLevel).WithValues("sandbox", sbx.Name)
		group, reason := findSandboxGroup(sbx)
		switch group {
		case GroupCreating:
			groups.Creating = append(groups.Creating, sbx)
		case GroupAvailable:
			groups.Available = append(groups.Available, sbx)
		case GroupUsed:
			groups.Used = append(groups.Used, sbx)
		case GroupFailed:
			groups.Failed = append(groups.Failed, sbx)
		default: // unknown
			return GroupedSandboxes{}, fmt.Errorf("cannot find group for sandbox %s", sbx.Name)
		}
		debugLog.Info("sandbox is grouped", "group", group, "reason", reason)
	}
	log.Info("sandbox group done", "total", len(sandboxList.Items), "creating", len(groups.Creating),
		"available", len(groups.Available), "used", len(groups.Used), "failed", len(groups.Failed))
	return groups, nil
}

func (r *Reconciler) initCreatedSandbox(ctx context.Context, sbx *agentsv1alpha1.Sandbox) error {
	if sbx.Labels[agentsv1alpha1.LabelSandboxState] == "" || sbx.Labels[agentsv1alpha1.LabelSandboxID] == "" {
		return r.patchSandboxLabel(ctx, sbx, map[string]string{
			agentsv1alpha1.LabelSandboxID:    sbx.GetName(),
			agentsv1alpha1.LabelSandboxState: agentsv1alpha1.SandboxStateAvailable,
		})
	}
	return nil
}

func (r *Reconciler) patchSandboxLabel(ctx context.Context, sbx *agentsv1alpha1.Sandbox, labels map[string]string) error {
	if labels == nil || len(labels) == 0 {
		return nil
	}
	j, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	return r.Patch(ctx, sbx, client.RawPatch(types.MergePatchType,
		[]byte(fmt.Sprintf(`{"metadata":{"labels":%s}}`, string(j)))))
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
