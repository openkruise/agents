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
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"
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
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

func init() {
	flag.IntVar(&concurrentReconciles, "sandbox-workers", concurrentReconciles, "Max concurrent reconciles for Sandbox controller.")
	flag.IntVar(&highCreatingThreshold, "high-creating-threshold", highCreatingThreshold, "Max number of high-priority sandboxes being created before rate limiting normal sandboxes.")
	flag.IntVar(&sandboxCreatingTimeout, "sandbox-creating-timeout", sandboxCreatingTimeout, "Timeout in seconds for a high-priority sandbox to be considered stuck in creating state.")
}

var (
	concurrentReconciles   = 500
	sandboxControllerKind  = agentsv1alpha1.GroupVersion.WithKind("Sandbox")
	highCreatingThreshold  = 100
	sandboxCreatingTimeout = 60

	AddSandboxTrackAction    = "add"
	DeleteSandboxTrackAction = "delete"
)

type SandboxTrack struct {
	Namespace string
	Name      string
}

func Add(mgr manager.Manager) error {
	if !utilfeature.DefaultFeatureGate.Enabled(features.SandboxGate) || !discovery.DiscoverGVK(sandboxControllerKind) {
		return nil
	}
	err := (&SandboxReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		controls:            core.NewSandboxControl(mgr.GetClient(), mgr.GetEventRecorderFor("sandbox")),
		highCreatingSandbox: map[string]*SandboxTrack{},
	}).SetupWithManager(mgr)
	if err != nil {
		return err
	}
	klog.Infof("Started SandboxReconciler successfully")
	return nil
}

// SandboxReconciler reconciles a Sandbox object
type SandboxReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	controls map[string]core.SandboxControl

	mu                       sync.RWMutex
	highCreatingSandbox      map[string]*SandboxTrack // key: "namespace/name"
	highCreatingSandboxCount int
}

// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;update;patch
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete

func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (crl ctrl.Result, err error) {
	// fetch pod
	pod := &corev1.Pod{}
	err = r.Get(ctx, req.NamespacedName, pod)
	if client.IgnoreNotFound(err) != nil {
		return reconcile.Result{}, err
	} else if errors.IsNotFound(err) {
		pod = nil
	}

	// Fetch the sandbox instance
	box := &agentsv1alpha1.Sandbox{}
	err = r.Get(ctx, req.NamespacedName, box)
	if err != nil {
		if errors.IsNotFound(err) {
			box.Namespace = req.NamespacedName.Namespace
			box.Name = req.NamespacedName.Name
			core.ResourceVersionExpectations.Delete(box)
			core.ScaleExpectation.DeleteExpectations(utils.GetControllerKey(box))
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))

	if box.Spec.Template == nil {
		logger.Info("sandbox template is nil, and ignore")
		return reconcile.Result{}, nil
	}

	logger.Info("Began to process Sandbox for reconcile")
	if pod != nil {
		core.ScaleExpectation.ObserveScale(utils.GetControllerKey(box), expectations.Create, pod.Name)
	}
	if isSatisfied, unsatisfiedDuration, _ := core.ScaleExpectation.SatisfiedExpectations(utils.GetControllerKey(box)); !isSatisfied {
		if unsatisfiedDuration < expectations.ExpectationTimeout {
			logger.Info("Not satisfied ScaleExpectation for Sandbox, wait for cache event")
			return reconcile.Result{RequeueAfter: expectations.ExpectationTimeout - unsatisfiedDuration}, nil
		}
		klog.InfoS("ScaleExpectation unsatisfied overtime for Sandbox, wait for cache event timeout", "timeout", unsatisfiedDuration)
		core.ScaleExpectation.DeleteExpectations(utils.GetControllerKey(box))
	}
	// If resourceVersion expectations have not satisfied yet, just skip this reconcile
	core.ResourceVersionExpectations.Observe(box)
	if isSatisfied, unsatisfiedDuration := core.ResourceVersionExpectations.IsSatisfied(box); !isSatisfied {
		if unsatisfiedDuration < expectations.ExpectationTimeout {
			logger.Info("Not satisfied resourceVersion for Sandbox, wait for cache event")
			return reconcile.Result{RequeueAfter: expectations.ExpectationTimeout - unsatisfiedDuration}, nil
		}
		klog.InfoS("ResourceVersionExpectations unsatisfied overtime for Sandbox, wait for cache event timeout", "timeout", unsatisfiedDuration)
		core.ResourceVersionExpectations.Delete(box)
	}

	defer func() {
		if !utilfeature.DefaultFeatureGate.Enabled(features.SandboxCreatePodRateLimitGate) ||
			!isHighPrioritySandbox(ctx, box) {
			return
		}

		// At this point, the sandbox status may have changed, so we need to process it
		if inCreatingTrack := r.updateHighCreatingSandbox(box); inCreatingTrack {
			requeueDuration := box.CreationTimestamp.Time.Add(time.Duration(sandboxCreatingTimeout) * time.Second).Sub(time.Now())
			crl = ctrl.Result{RequeueAfter: requeueDuration}
		}
	}()

	if requeueAfter, shouldReturn := r.handlerSandboxCreatePodRateLimit(ctx, pod, box, logger); shouldReturn {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	newStatus := box.Status.DeepCopy()
	if box.Annotations == nil {
		box.Annotations = map[string]string{}
	}

	// Process VolumeClaimTemplates for persistent data recovery during sleep/wake operations
	if err := r.ensureVolumeClaimTemplates(ctx, box); err != nil {
		logger.Error(err, "failed to ensure volume claim templates")
		return reconcile.Result{}, err
	}

	args := core.EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus}

	// ensure sandbox terminating
	if !box.DeletionTimestamp.IsZero() {
		return r.handleTerminating(ctx, args)
	}

	// if sandbox phase = Failed, Success
	if isSandboxCompletedPhase(box.Status.Phase) {
		return ctrl.Result{}, nil
	}

	// add finalizer
	if box, err = r.addSandboxFinalizerAndHash(ctx, box); err != nil {
		return reconcile.Result{}, err
	}

	// Check ShutdownTime and PauseTime
	now := metav1.Now()
	var requeueAfter time.Duration
	if box.Spec.ShutdownTime != nil && box.DeletionTimestamp == nil {
		if box.Spec.ShutdownTime.Before(&now) {
			logger.Info("sandbox shutdown time reached, will be deleted", "shutdownTime", box.Spec.ShutdownTime)
			return ctrl.Result{}, r.Delete(ctx, box)
		}
		requeueAfter = box.Spec.ShutdownTime.Sub(now.Time)
	}
	if box.Spec.PauseTime != nil && !box.Spec.Paused {
		if box.Spec.PauseTime.Before(&now) {
			logger.Info("sandbox pause time reached, will be paused")
			modified := box.DeepCopy()
			patch := client.MergeFrom(box)
			modified.Spec.Paused = true
			return ctrl.Result{}, r.Patch(ctx, modified, patch)
		}
		requeueAfter = min(requeueAfter, box.Spec.PauseTime.Sub(now.Time))
	}

	// calculate sandbox status
	var shouldRequeue bool
	newStatus, shouldRequeue = calculateStatus(args)
	if shouldRequeue {
		return reconcile.Result{RequeueAfter: requeueAfter}, r.updateSandboxStatus(ctx, *newStatus, box)
	}

	switch newStatus.Phase {
	case agentsv1alpha1.SandboxPending:
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
	pod, _, _ := args.Pod, args.Box, args.NewStatus
	return ctrl.Result{}, r.getControl(pod).EnsureSandboxTerminated(ctx, args)
}

func isSandboxCompletedPhase(phase agentsv1alpha1.SandboxPhase) bool {
	return phase == agentsv1alpha1.SandboxFailed || phase == agentsv1alpha1.SandboxSucceeded
}

// handlerSandboxCreatePodRateLimit applies rate limiting for normal sandboxes when high-priority sandboxes are being created.
// Returns (requeueAfter, true) if rate limiting is applied and reconciliation should stop.
func (r *SandboxReconciler) handlerSandboxCreatePodRateLimit(ctx context.Context, pod *corev1.Pod, box *agentsv1alpha1.Sandbox, logger logr.Logger) (time.Duration, bool) {
	if !utilfeature.DefaultFeatureGate.Enabled(features.SandboxCreatePodRateLimitGate) {
		return 0, false
	}

	// Process the scenario where sandbox enters for the first time
	if isHighPrioritySandbox(ctx, box) {
		_ = r.updateHighCreatingSandbox(box)
		// Only rate-limit normal sandbox Pod creation
		return 0, false
	}

	if (box.Status.Phase == "" || box.Status.Phase == agentsv1alpha1.SandboxPending) && pod == nil {
		count := r.getHighCreatingSandboxCount()
		if count > highCreatingThreshold {
			logger.Info("high creating sandbox count exceed threshold, and wait",
				"current creating count", count, "highCreatingThreshold", highCreatingThreshold)
			return time.Second * 3, true
		}
	}

	return 0, false
}

func (r *SandboxReconciler) addSandboxFinalizerAndHash(ctx context.Context, box *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
	if !box.DeletionTimestamp.IsZero() || controllerutil.ContainsFinalizer(box, utils.SandboxFinalizer) {
		return box, nil
	}

	originObj := box.DeepCopy()
	patch := client.MergeFrom(box)
	controllerutil.AddFinalizer(originObj, utils.SandboxFinalizer)
	if originObj.Annotations == nil {
		originObj.Annotations = make(map[string]string)
	}
	_, hashWithoutImageAndResource := core.HashSandbox(box)
	originObj.Annotations[agentsv1alpha1.SandboxHashWithoutImageAndResources] = hashWithoutImageAndResource
	if err := client.IgnoreNotFound(r.Patch(ctx, originObj, patch)); err != nil {
		logger.Error(err, "failed to patch sandbox finalizer and hash")
		return nil, fmt.Errorf("failed to patch finalizer: %w", err)
	}
	logger.Info("patch sandbox hash annotations and finalizer success")
	return originObj, nil
}

func (r *SandboxReconciler) updateSandboxStatus(ctx context.Context, newStatus agentsv1alpha1.SandboxStatus, box *agentsv1alpha1.Sandbox) error {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
	if reflect.DeepEqual(box.Status, newStatus) || newStatus.Phase == agentsv1alpha1.SandboxPending {
		return nil
	}

	by, _ := json.Marshal(newStatus)
	patchStatus := fmt.Sprintf(`{"status":%s}`, string(by))
	rcvObject := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: box.Namespace, Name: box.Name}}
	err := client.IgnoreNotFound(r.Status().Patch(ctx, rcvObject, client.RawPatch(types.MergePatchType, []byte(patchStatus))))
	if err != nil {
		logger.Error(err, "update sandbox status failed", "patchStatus", patchStatus)
		return err
	}
	core.ResourceVersionExpectations.Expect(rcvObject)
	logger.Info("update sandbox status success", "status", utils.DumpJson(newStatus))
	box.Status = newStatus
	return nil
}

func calculateStatus(args core.EnsureFuncArgs) (*agentsv1alpha1.SandboxStatus, bool) {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	logger := logf.FromContext(context.TODO()).WithValues("sandbox", klog.KObj(box))

	hash, _ := core.HashSandbox(box)
	newStatus.ObservedGeneration = box.Generation
	newStatus.UpdateRevision = hash
	if newStatus.Phase == "" {
		newStatus.Phase = agentsv1alpha1.SandboxPending
	}

	switch newStatus.Phase {
	case agentsv1alpha1.SandboxPending:
		updateStatusIfPodCompleted(pod, newStatus)
		if isSandboxCompletedPhase(newStatus.Phase) {
			return newStatus, true
		}
	case agentsv1alpha1.SandboxRunning:
		// At this stage, if the Pod does not exist, it can only be that the Pod was deleted externally, and the sandbox should enter the Failed state
		if pod == nil || !pod.DeletionTimestamp.IsZero() {
			newStatus.Phase = agentsv1alpha1.SandboxFailed
			newStatus.Message = "Pod Not Found"
		} else {
			updateStatusIfPodCompleted(pod, newStatus)
		}
		if isSandboxCompletedPhase(newStatus.Phase) {
			return newStatus, true
		}

		// If it is paused, first set the sandbox to the Paused state.
		// To prevent loss of state information, the state immediately before Paused must currently be Running.
		if box.Spec.Paused {
			// The paused and resumed condition are exclusive
			utils.RemoveSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionResumed))
			newStatus.Phase = agentsv1alpha1.SandboxPaused
		}

	case agentsv1alpha1.SandboxPaused:
		cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
		// sandbox will only enter the resuming state after successful paused
		if cond.Status == metav1.ConditionTrue && !box.Spec.Paused {
			// delete paused condition
			utils.RemoveSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
			newStatus.Phase = agentsv1alpha1.SandboxResuming
			rCond := metav1.Condition{
				Type:               string(agentsv1alpha1.SandboxConditionResumed),
				Status:             metav1.ConditionFalse,
				Reason:             agentsv1alpha1.SandboxResumeReasonCreatePod,
				LastTransitionTime: metav1.Now(),
			}
			utils.SetSandboxCondition(newStatus, rCond)
		} else if !box.Spec.Paused && cond.Status == metav1.ConditionFalse {
			logger.Info("sandbox pause not completed, cannot enter resume state temporarily")
		}
	}
	return newStatus, false
}

func updateStatusIfPodCompleted(pod *corev1.Pod, newStatus *agentsv1alpha1.SandboxStatus) {
	if pod == nil || !pod.DeletionTimestamp.IsZero() {
		return
	}
	if pod.Status.Phase == corev1.PodSucceeded {
		newStatus.Phase = agentsv1alpha1.SandboxSucceeded
		newStatus.Message = "Pod status phase is Succeeded"
	} else if pod.Status.Phase == corev1.PodFailed {
		newStatus.Phase = agentsv1alpha1.SandboxFailed
		newStatus.Message = "Pod status phase is Failed"
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentReconciles}).
		For(&agentsv1alpha1.Sandbox{}).
		Named("sandbox-controller").
		Watches(&agentsv1alpha1.Sandbox{}, &handler.EnqueueRequestForObject{}).Watches(&corev1.Pod{}, &SandboxPodEventHandler{}).
		Complete(r)
}

// ensureVolumeClaimTemplates creates and ensures PVCs exist for persistent data recovery during sleep/wake operations
func (r *SandboxReconciler) ensureVolumeClaimTemplates(ctx context.Context, box *agentsv1alpha1.Sandbox) error {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))

	if len(box.Spec.VolumeClaimTemplates) == 0 {
		return nil
	}

	for _, template := range box.Spec.VolumeClaimTemplates {
		// Generate PVC name based on template name and sandbox name
		pvcName, err := core.GeneratePVCName(template.Name, box.Name)
		if err != nil {
			logger.Error(err, "failed to generate PVC name", "template", template.Name, "sandbox", box.Name)
			return err
		}

		// Create PVC object based on the template
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pvcName,
				Namespace: box.Namespace,
			},
			Spec: template.Spec,
		}

		// Set the sandbox as the owner of the PVC to align their lifecycles
		if err = ctrl.SetControllerReference(box, pvc, r.Scheme); err != nil {
			logger.Error(err, "failed to set sandbox as owner of PVC", "pvc", pvcName)
			return err
		}

		// Check if PVC already exists
		existingPVC := &corev1.PersistentVolumeClaim{}
		err = r.Get(ctx, client.ObjectKey{Namespace: box.Namespace, Name: pvcName}, existingPVC)

		if err == nil {
			logger.Info("PVC already exists for persistent data recovery", "pvc", pvcName)
			continue
		}

		if !errors.IsNotFound(err) {
			logger.Error(err, "failed to get PVC", "pvc", pvcName)
			return err
		}

		if err = r.Create(ctx, pvc); err == nil {
			logger.Info("created PVC for persistent data recovery", "pvc", pvcName)
			continue
		}

		if !errors.IsAlreadyExists(err) {
			logger.Error(err, "failed to create PVC", "pvc", pvcName)
			return err
		}
		logger.Info("PVC already exists after create attempt", "pvc", pvcName)
	}

	return nil
}

func (r *SandboxReconciler) updateHighCreatingSandbox(box *agentsv1alpha1.Sandbox) bool {
	isCreating := isCreatingSandbox(box)
	key := fmt.Sprintf("%s/%s", box.Namespace, box.Name)
	var isCreatingTimeout bool
	if isCreating {
		isCreatingTimeout = metav1.Now().Sub(box.CreationTimestamp.Time) > (time.Duration(sandboxCreatingTimeout) * time.Second)
	}

	action := AddSandboxTrackAction
	if !isCreating || isCreatingTimeout {
		action = DeleteSandboxTrackAction
	}

	r.mu.RLock()
	_, inCreatingTrack := r.highCreatingSandbox[key]
	r.mu.RUnlock()

	switch action {
	case AddSandboxTrackAction:
		if inCreatingTrack {
			return true
		}
		r.mu.Lock()
		track := SandboxTrack{
			Namespace: box.Namespace,
			Name:      box.Name,
		}
		r.highCreatingSandbox[key] = &track
		r.highCreatingSandboxCount++
		inCreatingTrack = true
		r.mu.Unlock()
		// delete
	default:
		if !inCreatingTrack {
			return false
		}
		r.mu.Lock()
		delete(r.highCreatingSandbox, key)
		r.highCreatingSandboxCount--
		inCreatingTrack = false
		r.mu.Unlock()
	}
	return inCreatingTrack
}

func (r *SandboxReconciler) getHighCreatingSandboxCount() int {
	r.mu.RLock()
	count := r.highCreatingSandboxCount
	r.mu.RUnlock()
	return count
}

func isHighPrioritySandbox(ctx context.Context, box *agentsv1alpha1.Sandbox) bool {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
	value, ok := box.Annotations[agentsv1alpha1.SandboxAnnotationPriority]
	if !ok || value == "" {
		return false
	}

	priority, err := strconv.Atoi(value)
	if err != nil {
		logger.Error(err, "parse annotations failed", agentsv1alpha1.SandboxAnnotationPriority, value)
		return false
	}
	return priority > 0
}

func isCreatingSandbox(box *agentsv1alpha1.Sandbox) bool {
	if !box.DeletionTimestamp.IsZero() {
		return false
	}
	if box.Status.Phase == agentsv1alpha1.SandboxPaused || box.Status.Phase == agentsv1alpha1.SandboxResuming ||
		box.Status.Phase == agentsv1alpha1.SandboxSucceeded || box.Status.Phase == agentsv1alpha1.SandboxFailed {
		return false
	}
	cond := utils.GetSandboxCondition(&box.Status, string(agentsv1alpha1.SandboxConditionReady))
	if box.Status.Phase == agentsv1alpha1.SandboxRunning && cond != nil && cond.Status == metav1.ConditionTrue {
		return false
	}
	return true
}
