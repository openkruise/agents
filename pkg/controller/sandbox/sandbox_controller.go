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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/sandbox/core"
	"github.com/openkruise/agents/pkg/discovery"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/pausedretention"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
	timeoututils "github.com/openkruise/agents/pkg/utils/timeout"
)

func init() {
	flag.IntVar(&concurrentReconciles, "sandbox-workers", concurrentReconciles, "Max concurrent reconciles for Sandbox controller.")
	flag.DurationVar(&recycleTimeout, "recycle-timeout", recycleTimeout, "Timeout for sandbox recycle operations.")
	flag.DurationVar(&recycleGracePeriod, "recycle-grace-period", recycleGracePeriod, "Grace period after recycle before sandbox returns to pool.")
	flag.DurationVar(&recycleFailureShutdownGrace, "recycle-failure-shutdown-grace", recycleFailureShutdownGrace, "Grace period before shutting down a sandbox after recycle failure.")
	flag.StringVar(&csiResetSignalDir, "csi-reset-signal-dir", csiResetSignalDir,
		"Directory inside the sandbox where a reset signal file is written before recycle when the sandbox carries CSI mounts, "+
			"so a stopping csi-sidecar can unmount stale volumes during prestop/SIGTERM. Empty disables the behavior.")
	flag.StringVar(&csiResetSignalFileName, "csi-reset-signal-file", csiResetSignalFileName,
		"Name of the reset signal file written into --csi-reset-signal-dir before recycle.")
}

var (
	concurrentReconciles        = 500
	sandboxControllerKind       = agentsv1alpha1.GroupVersion.WithKind("Sandbox")
	recycleTimeout              = 60 * time.Second
	recycleGracePeriod          = 10 * time.Second
	recycleFailureShutdownGrace = 5 * time.Minute
	csiResetSignalDir           = ""
	csiResetSignalFileName      = "reset"
)

const (
	eventReasonSandboxTerminating = "SandboxTerminating"
)

type sandboxPhaseTransition struct {
	oldPhase agentsv1alpha1.SandboxPhase
	newPhase agentsv1alpha1.SandboxPhase
}

var sandboxPhaseTransitionMessages = map[sandboxPhaseTransition]string{
	{oldPhase: "", newPhase: agentsv1alpha1.SandboxPending}:                              "Start sandbox",
	{oldPhase: agentsv1alpha1.SandboxPending, newPhase: agentsv1alpha1.SandboxRunning}:   "Sandbox started",
	{oldPhase: agentsv1alpha1.SandboxRunning, newPhase: agentsv1alpha1.SandboxPaused}:    "Pause sandbox",
	{oldPhase: agentsv1alpha1.SandboxPaused, newPhase: agentsv1alpha1.SandboxResuming}:   "Resume sandbox",
	{oldPhase: agentsv1alpha1.SandboxPaused, newPhase: agentsv1alpha1.SandboxRunning}:    "Resume sandbox",
	{oldPhase: agentsv1alpha1.SandboxResuming, newPhase: agentsv1alpha1.SandboxRunning}:  "Sandbox resumed",
	{oldPhase: agentsv1alpha1.SandboxRunning, newPhase: agentsv1alpha1.SandboxUpgrading}: "Upgrade sandbox",
	{oldPhase: agentsv1alpha1.SandboxUpgrading, newPhase: agentsv1alpha1.SandboxRunning}: "Sandbox upgraded",
	{oldPhase: agentsv1alpha1.SandboxRunning, newPhase: agentsv1alpha1.SandboxRecycling}: "Recycle sandbox",
	{oldPhase: agentsv1alpha1.SandboxRecycling, newPhase: agentsv1alpha1.SandboxRunning}: "Sandbox recycled",
}

var sandboxPhaseTargetMessages = map[agentsv1alpha1.SandboxPhase]string{
	agentsv1alpha1.SandboxSucceeded: "Sandbox succeeded",
	agentsv1alpha1.SandboxFailed:    "Sandbox failed",
}

// Enqueuer is the contract the Sandbox controller depends on for async
// metric cleanup. sandboxmetricsgc.Reconciler satisfies it.
type Enqueuer interface {
	Enqueue(namespace, name string)
}

func Add(mgr manager.Manager, metricsCleanup Enqueuer) error {
	if !utilfeature.DefaultFeatureGate.Enabled(features.SandboxGate) || !discovery.DiscoverGVK(sandboxControllerKind) {
		return nil
	}
	if metricsCleanup == nil {
		return fmt.Errorf("sandbox: metricsCleanup enqueuer is required")
	}

	rateLimiter := core.NewRateLimiter()
	recorder := mgr.GetEventRecorderFor("sandbox")
	checkpointControl := core.NewCheckpointControl(mgr.GetClient(), recorder)
	podControl := core.NewPodControl(mgr.GetClient(), recorder, core.GeneratePodFromSandbox)
	err := (&SandboxReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		checkpointControl: checkpointControl,
		controls: core.NewSandboxControl(core.SandboxControlArgs{
			Client:            mgr.GetClient(),
			APIReader:         mgr.GetAPIReader(),
			Recorder:          recorder,
			RateLimiter:       rateLimiter,
			CheckpointControl: checkpointControl,
			PodControl:        podControl,
			RecycleConfig: core.SandboxRecycleConfig{
				Timeout:                recycleTimeout,
				GracePeriod:            recycleGracePeriod,
				FailureShutdownGrace:   recycleFailureShutdownGrace,
				CSIResetSignalDir:      csiResetSignalDir,
				CSIResetSignalFileName: csiResetSignalFileName,
			},
		}),
		rateLimiter:    rateLimiter,
		metricsCleanup: metricsCleanup,
		recorder:       recorder,
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
	Scheme            *runtime.Scheme
	controls          map[string]core.SandboxControl
	rateLimiter       *core.RateLimiter
	checkpointControl *core.CheckpointControl
	metricsCleanup    Enqueuer
	recorder          record.EventRecorder
}

// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxtemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=checkpoints,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;update;patch
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch

//nolint:gocyclo // This function handles multiple reconciliation scenarios which require branching logic
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
			r.metricsCleanup.Enqueue(req.NamespacedName.Namespace, req.NamespacedName.Name)
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	// Record sandbox lifecycle metrics on every reconcile
	recordSandboxMetrics(box, pod)

	if box.Spec.Template == nil && box.Spec.TemplateRef == nil {
		if !box.DeletionTimestamp.IsZero() {
			newStatus := box.Status.DeepCopy()
			args := core.EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus}
			return r.handleTerminating(ctx, args)
		}
		klog.InfoS("sandbox template is nil, and ignore", "sandbox", klog.KObj(box))
		return reconcile.Result{}, nil
	}

	klog.InfoS("Began to process Sandbox for reconcile", "sandbox", klog.KObj(box))
	if pod != nil {
		core.ScaleExpectation.ObserveScale(utils.GetControllerKey(box), expectations.Create, pod.Name)
	}
	if isSatisfied, unsatisfiedDuration, _ := core.ScaleExpectation.SatisfiedExpectations(utils.GetControllerKey(box)); !isSatisfied {
		if unsatisfiedDuration < expectations.ExpectationTimeout {
			klog.InfoS("Not satisfied ScaleExpectation for Sandbox, wait for cache event", "sandbox", klog.KObj(box))
			return reconcile.Result{RequeueAfter: expectations.ExpectationTimeout - unsatisfiedDuration}, nil
		}
		klog.InfoS("ScaleExpectation unsatisfied overtime for Sandbox, wait for cache event timeout", "timeout", unsatisfiedDuration)
		core.ScaleExpectation.DeleteExpectations(utils.GetControllerKey(box))
	}
	// If resourceVersion expectations have not satisfied yet, just skip this reconcile
	core.ResourceVersionExpectations.Observe(box)
	if isSatisfied, unsatisfiedDuration := core.ResourceVersionExpectations.IsSatisfied(box); !isSatisfied {
		if unsatisfiedDuration < expectations.ExpectationTimeout {
			klog.InfoS("Not satisfied resourceVersion for Sandbox, wait for cache event", "sandbox", klog.KObj(box))
			return reconcile.Result{RequeueAfter: expectations.ExpectationTimeout - unsatisfiedDuration}, nil
		}
		klog.InfoS("ResourceVersionExpectations unsatisfied overtime for Sandbox, wait for cache event timeout", "timeout", unsatisfiedDuration)
		core.ResourceVersionExpectations.Delete(box)
	}

	defer func() {
		if !utilfeature.DefaultFeatureGate.Enabled(features.SandboxCreatePodRateLimitGate) ||
			!core.IsHighPrioritySandbox(ctx, box) || err != nil {
			return
		}

		// At this point, the sandbox status may have changed, so we need to process it
		if inCreatingTrack := r.rateLimiter.UpdateRateLimiter(box); inCreatingTrack {
			requeueDuration := box.CreationTimestamp.Time.Add(time.Duration(core.MaxSandboxCreateDelay()) * time.Second).Sub(time.Now())
			crl = ctrl.Result{RequeueAfter: requeueDuration}
		}
	}()

	newStatus := box.Status.DeepCopy()
	if box.Annotations == nil {
		box.Annotations = map[string]string{}
	}

	// Process VolumeClaimTemplates for persistent data recovery during sleep/wake operations
	if err := r.ensureVolumeClaimTemplates(ctx, box); err != nil {
		klog.ErrorS(err, "failed to ensure volume claim templates", "sandbox", klog.KObj(box))
		return reconcile.Result{}, err
	}

	args := core.EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus}

	// ensure sandbox terminating
	if !box.DeletionTimestamp.IsZero() {
		if box.Status.Phase != agentsv1alpha1.SandboxFailed && box.Status.Phase != agentsv1alpha1.SandboxSucceeded {
			klog.InfoS("Sandbox Delete started", "sandbox", klog.KObj(box), "previousPhase", string(box.Status.Phase))
		}
		result, termErr := r.handleTerminating(ctx, args)
		if termErr == nil {
			klog.InfoS("Sandbox Delete finished", "sandbox", klog.KObj(box))
		}
		return result, termErr
	}

	// if sandbox phase = Failed, Success
	if isSandboxCompletedPhase(box.Status.Phase) {
		return ctrl.Result{}, nil
	}

	// add finalizer
	if box, err = r.addSandboxFinalizerAndHash(ctx, box); err != nil {
		return reconcile.Result{}, err
	}

	// Check ShutdownTime and PauseTime.
	result, done, timerErr := r.checkTimers(ctx, box, metav1.Now())
	if done {
		return result, timerErr
	}
	requeueAfter := result.RequeueAfter

	// calculate sandbox status
	var shouldRequeue bool
	newStatus, shouldRequeue = r.calculateStatus(ctx, args)
	if shouldRequeue {
		return reconcile.Result{RequeueAfter: requeueAfter}, r.updateSandboxStatus(ctx, *newStatus, box)
	}

	if box.Status.Phase != newStatus.Phase {
		klog.InfoS("Sandbox phase started", "sandbox", klog.KObj(box), "phase", string(newStatus.Phase), "previousPhase", string(box.Status.Phase))
	}

	phaseBefore := newStatus.Phase
	switch newStatus.Phase {
	case agentsv1alpha1.SandboxPending:
		requeueAfter, err = r.getControl(args.Pod).EnsureSandboxRunning(ctx, args)
	case agentsv1alpha1.SandboxRunning:
		err = r.getControl(args.Pod).EnsureSandboxUpdated(ctx, args)
	case agentsv1alpha1.SandboxPaused:
		err = r.EnsureSandboxPaused(ctx, args)
	case agentsv1alpha1.SandboxResuming:
		err = r.getControl(args.Pod).EnsureSandboxResumed(ctx, args)
	case agentsv1alpha1.SandboxUpgrading:
		err = r.getControl(args.Pod).EnsureSandboxUpgraded(ctx, args)
	case agentsv1alpha1.SandboxRecycling:
		requeueAfter, err = r.getControl(args.Pod).EnsureSandboxRecycled(ctx, args)
	default:
		klog.InfoS("sandbox status phase is invalid", "sandbox", klog.KObj(box), "phase", box.Status.Phase)
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	if err != nil {
		if retErr := r.updateSandboxStatus(ctx, *newStatus, box); retErr != nil {
			klog.ErrorS(retErr, "failed to persist upgrade status on error", "sandbox", klog.KObj(box))
		}
		return reconcile.Result{}, err
	}
	if newStatus.Phase != phaseBefore {
		klog.InfoS("Sandbox phase finished", "sandbox", klog.KObj(box), "phase", string(phaseBefore), "nextPhase", string(newStatus.Phase))
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, r.updateSandboxStatus(ctx, *newStatus, box)
}

func (r *SandboxReconciler) EnsureSandboxPaused(ctx context.Context, args core.EnsureFuncArgs) error {
	return r.getControl(args.Pod).EnsureSandboxPaused(ctx, args)
}

func (r *SandboxReconciler) handleTerminating(ctx context.Context, args core.EnsureFuncArgs) (ctrl.Result, error) {
	pod, box, _ := args.Pod, args.Box, args.NewStatus
	recordTerminatingEvent := pod != nil && pod.DeletionTimestamp.IsZero()
	if err := r.getControl(pod).EnsureSandboxTerminated(ctx, args); err != nil {
		return ctrl.Result{}, err
	}
	if recordTerminatingEvent {
		r.recordSandboxTerminatingEvent(box)
	}
	return ctrl.Result{}, nil
}

func isSandboxCompletedPhase(phase agentsv1alpha1.SandboxPhase) bool {
	return phase == agentsv1alpha1.SandboxFailed || phase == agentsv1alpha1.SandboxSucceeded
}

func pauseTimeReached(pauseTime *metav1.Time, now metav1.Time) bool {
	return pauseTime != nil && !pauseTime.After(now.Time)
}

func (r *SandboxReconciler) addSandboxFinalizerAndHash(ctx context.Context, box *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
	if !box.DeletionTimestamp.IsZero() || controllerutil.ContainsFinalizer(box, core.SandboxFinalizer) {
		return box, nil
	}

	originObj := box.DeepCopy()
	patch := client.MergeFrom(box)
	controllerutil.AddFinalizer(originObj, core.SandboxFinalizer)
	if originObj.Annotations == nil {
		originObj.Annotations = make(map[string]string)
	}
	_, hashImmutablePart := core.HashSandbox(box)
	originObj.Annotations[agentsv1alpha1.SandboxHashImmutablePart] = hashImmutablePart
	if err := client.IgnoreNotFound(r.Patch(ctx, originObj, patch)); err != nil {
		klog.ErrorS(err, "failed to patch sandbox finalizer and hash", "sandbox", klog.KObj(box))
		return nil, fmt.Errorf("failed to patch finalizer: %w", err)
	}
	klog.InfoS("patch sandbox hash annotations and finalizer success", "sandbox", klog.KObj(box))
	return originObj, nil
}

func (r *SandboxReconciler) updateSandboxStatus(ctx context.Context, newStatus agentsv1alpha1.SandboxStatus, box *agentsv1alpha1.Sandbox) error {
	if reflect.DeepEqual(box.Status, newStatus) {
		return nil
	}
	oldPhase := box.Status.Phase

	by, _ := json.Marshal(newStatus)
	patchStatus := fmt.Sprintf(`{"status":%s}`, string(by))
	rcvObject := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: box.Namespace, Name: box.Name}}
	err := client.IgnoreNotFound(r.Status().Patch(ctx, rcvObject, client.RawPatch(types.MergePatchType, []byte(patchStatus))))
	if err != nil {
		klog.ErrorS(err, "update sandbox status failed", "sandbox", klog.KObj(box), "patchStatus", patchStatus)
		return err
	}
	core.ResourceVersionExpectations.Expect(rcvObject)
	klog.InfoS("update sandbox status success", "sandbox", klog.KObj(box), "status", utils.DumpJson(newStatus))
	box.Status = newStatus
	r.recordSandboxPhaseEvent(box, oldPhase, newStatus)
	// Update metrics after status change (pod=nil: container metrics already recorded in Reconcile)
	recordSandboxMetrics(box, nil)
	return nil
}

func (r *SandboxReconciler) recordSandboxPhaseEvent(box *agentsv1alpha1.Sandbox, oldPhase agentsv1alpha1.SandboxPhase, newStatus agentsv1alpha1.SandboxStatus) {
	if r.recorder == nil || oldPhase == newStatus.Phase || newStatus.Phase == "" {
		return
	}

	eventType := corev1.EventTypeNormal
	if newStatus.Phase == agentsv1alpha1.SandboxFailed {
		eventType = corev1.EventTypeWarning
	}
	message := sandboxPhaseEventMessage(oldPhase, newStatus.Phase)
	if newStatus.Message != "" {
		message = fmt.Sprintf("%s: %s", message, newStatus.Message)
	}
	r.recorder.Event(box, eventType, sandboxPhaseEventReason(newStatus.Phase), message)
}

func (r *SandboxReconciler) recordSandboxTerminatingEvent(box *agentsv1alpha1.Sandbox) {
	if r.recorder == nil || box == nil || box.DeletionTimestamp.IsZero() {
		return
	}
	r.recorder.Eventf(box, corev1.EventTypeNormal, eventReasonSandboxTerminating,
		"Sandbox deletion is in progress from phase %s", phaseForEvent(box.Status.Phase))
}

func sandboxPhaseEventMessage(oldPhase, newPhase agentsv1alpha1.SandboxPhase) string {
	if message, ok := sandboxPhaseTransitionMessages[sandboxPhaseTransition{oldPhase: oldPhase, newPhase: newPhase}]; ok {
		return message
	}
	if message, ok := sandboxPhaseTargetMessages[newPhase]; ok {
		return message
	}
	return fmt.Sprintf("Sandbox phase changed from %s to %s", phaseForEvent(oldPhase), newPhase)
}

func sandboxPhaseEventReason(phase agentsv1alpha1.SandboxPhase) string {
	if phase == "" {
		return "SandboxPhaseChanged"
	}
	return "Sandbox" + string(phase)
}

func phaseForEvent(phase agentsv1alpha1.SandboxPhase) string {
	if phase == "" {
		return "<empty>"
	}
	return string(phase)
}

func (r *SandboxReconciler) calculateStatus(ctx context.Context, args core.EnsureFuncArgs) (*agentsv1alpha1.SandboxStatus, bool) {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus

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
		// Recycle trigger takes priority over Pod terminal detection.
		// If recycle is requested, always enter Recycling regardless of Pod state —
		// doRecycle's terminal phase check properly handles dead Pods via
		// handleRecycleFailed (which deletes the sandbox and cleans up metadata).
		// This prevents the sandbox from getting stuck in Failed with dirty
		// claim metadata when the Pod dies between claim-release and the next reconcile.
		if isRecycleTriggered(box) {
			if hasPVCVolumes(box) {
				r.rejectRecycle(box, newStatus, "recycle is not supported for sandboxes with persistent volume claims")
			} else {
				klog.InfoS("Detected recycle trigger", "sandbox", klog.KObj(box))
				newStatus.Phase = agentsv1alpha1.SandboxRecycling
				utils.RemoveSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionRecycling))
				break
			}
		}

		// Pod terminal detection (only when recycle is NOT triggered)
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
			// Check for upgrade: if template has changed (hash mismatch), transition to Upgrading phase
		} else if pod != nil && pod.Labels[agentsv1alpha1.PodLabelTemplateHash] != newStatus.UpdateRevision &&
			box.Spec.UpgradePolicy != nil && box.Spec.UpgradePolicy.Type == agentsv1alpha1.SandboxUpgradePolicyRecreate {
			klog.InfoS("Detected upgrade trigger", "sandbox", klog.KObj(box),
				"podRevision", pod.Labels[agentsv1alpha1.PodLabelTemplateHash],
				"sandboxRevision", newStatus.UpdateRevision)
			newStatus.Phase = agentsv1alpha1.SandboxUpgrading
			utils.RemoveSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
		}

	case agentsv1alpha1.SandboxPaused:
		// Paused state does not support recycle; reject immediately.
		if isRecycleTriggered(box) {
			r.rejectRecycle(box, newStatus, "recycle is not supported in Paused state")
		}

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
			klog.InfoS("sandbox pause not completed, cannot enter resume state temporarily", "sandbox", klog.KObj(box))
		}

	case agentsv1alpha1.SandboxRecycling:
		// Recycle lifecycle (progress checking, grace period, success/failure
		// transitions) is handled entirely by EnsureSandboxRecycled.

	case agentsv1alpha1.SandboxUpgrading:
		// This indicates the podTemplate has changed again during an ongoing upgrade.
		// Determine the resume step after the desired template changes during an ongoing upgrade.
		if newStatus.UpdateRevision != box.Status.UpdateRevision {
			upgradeCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
			if upgradeCond != nil {
				resumeReason := determineUpgradeResumeReason(pod, newStatus, upgradeCond)
				klog.InfoS("podTemplate changed during upgrade, resetting condition Upgrading reason",
					"sandbox", klog.KObj(box),
					"previousReason", upgradeCond.Reason,
					"oldRevision", box.Status.UpdateRevision,
					"newRevision", newStatus.UpdateRevision,
					"resumeReason", resumeReason)
				upgradeCond.Reason = resumeReason
				upgradeCond.Message = ""
				utils.SetSandboxCondition(newStatus, *upgradeCond)
			}
		}
	}
	return newStatus, false
}

// isRecycleTriggered returns true when the recycle annotation and the recycle-enabled
// annotation are both set to "true" on the sandbox.
func isRecycleTriggered(box *agentsv1alpha1.Sandbox) bool {
	return box.Annotations[agentsv1alpha1.AnnotationCleanup] == agentsv1alpha1.True &&
		box.Annotations[agentsv1alpha1.AnnotationCleanupEnabled] == agentsv1alpha1.True
}

// hasPVCVolumes returns true if the sandbox has VolumeClaimTemplates or its pod
// template references any PersistentVolumeClaim volumes.
func hasPVCVolumes(box *agentsv1alpha1.Sandbox) bool {
	if len(box.Spec.VolumeClaimTemplates) > 0 {
		return true
	}
	if box.Spec.Template != nil {
		for _, vol := range box.Spec.Template.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil {
				return true
			}
		}
	}
	return false
}

// rejectRecycle sets a Recycling condition with reason RecycleRejected and records a
// Warning event. The sandbox stays in its current phase (Running or Paused).
// The msg parameter provides the specific rejection reason.
func (r *SandboxReconciler) rejectRecycle(box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus, msg string) {
	// Avoid duplicate events if the condition is already set to RecycleRejected
	// with the same message.
	if existing := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionRecycling)); existing != nil &&
		existing.Reason == agentsv1alpha1.SandboxRecyclingReasonRejected && existing.Message == msg {
		return
	}

	utils.SetSandboxCondition(newStatus, metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxConditionRecycling),
		Status:             metav1.ConditionFalse,
		Reason:             agentsv1alpha1.SandboxRecyclingReasonRejected,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
	})

	if r.recorder != nil {
		r.recorder.Event(box, corev1.EventTypeWarning, agentsv1alpha1.SandboxRecyclingReasonRejected, msg)
	}

	klog.InfoS("Recycling rejected", "sandbox", klog.KObj(box), "reason", msg)
}

func determineUpgradeResumeReason(
	pod *corev1.Pod,
	newStatus *agentsv1alpha1.SandboxStatus,
	upgradeCond *metav1.Condition,
) string {
	if upgradeCond == nil || !utilfeature.DefaultFeatureGate.Enabled(features.SandboxUpgradeResumeFromFailedStepGate) {
		return agentsv1alpha1.SandboxUpgradingReasonPreUpgrade
	}

	switch upgradeCond.Reason {
	case agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed:
		return agentsv1alpha1.SandboxUpgradingReasonPreUpgrade
	case agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed:
		return agentsv1alpha1.SandboxUpgradingReasonUpgradePod
	case agentsv1alpha1.SandboxUpgradingReasonPostUpgradeFailed:
		if pod != nil && pod.Labels[agentsv1alpha1.PodLabelTemplateHash] == newStatus.UpdateRevision {
			return agentsv1alpha1.SandboxUpgradingReasonPostUpgrade
		}
		return agentsv1alpha1.SandboxUpgradingReasonUpgradePod
	default:
		return agentsv1alpha1.SandboxUpgradingReasonPreUpgrade
	}
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
		Watches(&agentsv1alpha1.Sandbox{}, &handler.EnqueueRequestForObject{}).
		Watches(&corev1.Pod{}, &SandboxPodEventHandler{}).
		Watches(&agentsv1alpha1.Checkpoint{}, &CheckpointEventHandler{}).
		Complete(r)
}

// ensureVolumeClaimTemplates creates and ensures PVCs exist for persistent data recovery during sleep/wake operations
func (r *SandboxReconciler) ensureVolumeClaimTemplates(ctx context.Context, box *agentsv1alpha1.Sandbox) error {
	if len(box.Spec.VolumeClaimTemplates) == 0 {
		return nil
	}

	for _, template := range box.Spec.VolumeClaimTemplates {
		// Generate PVC name based on template name and sandbox name
		pvcName, err := core.GeneratePVCName(template.Name, box.Name)
		if err != nil {
			klog.ErrorS(err, "failed to generate PVC name", "sandbox", klog.KObj(box), "template", template.Name)
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
			klog.ErrorS(err, "failed to set sandbox as owner of PVC", "sandbox", klog.KObj(box), "pvc", pvcName)
			return err
		}

		// Check if PVC already exists
		existingPVC := &corev1.PersistentVolumeClaim{}
		err = r.Get(ctx, client.ObjectKey{Namespace: box.Namespace, Name: pvcName}, existingPVC)

		if err == nil {
			klog.InfoS("PVC already exists for persistent data recovery", "sandbox", klog.KObj(box), "pvc", pvcName)
			continue
		}

		if !errors.IsNotFound(err) {
			klog.ErrorS(err, "failed to get PVC", "sandbox", klog.KObj(box), "pvc", pvcName)
			return err
		}

		if err = r.Create(ctx, pvc); err == nil {
			klog.InfoS("created PVC for persistent data recovery", "sandbox", klog.KObj(box), "pvc", pvcName)
			continue
		}

		if !errors.IsAlreadyExists(err) {
			klog.ErrorS(err, "failed to create PVC", "sandbox", klog.KObj(box), "pvc", pvcName)
			return err
		}
		klog.InfoS("PVC already exists after create attempt", "sandbox", klog.KObj(box), "pvc", pvcName)
	}

	return nil
}

func (r *SandboxReconciler) checkTimers(ctx context.Context, box *agentsv1alpha1.Sandbox, now metav1.Time) (ctrl.Result, bool, error) {
	// Skip timers during reuse unless the reuse has reached a terminal
	// failure state. Only then can ShutdownTime set by reuse failure be handled.
	// handleRecycleFailed be processed. Using != avoids needing to update
	// this check when new in-progress reasons are added in the future.
	if box.Status.Phase == agentsv1alpha1.SandboxRecycling {
		recycleCond := utils.GetSandboxCondition(&box.Status, string(agentsv1alpha1.SandboxConditionRecycling))
		if recycleCond == nil ||
			(recycleCond.Reason != agentsv1alpha1.SandboxRecyclingReasonFailed &&
				recycleCond.Reason != agentsv1alpha1.SandboxRecyclingReasonTimeout) {
			return ctrl.Result{}, false, nil
		}
	}

	if done, err := r.handleShutdownTimeout(ctx, box, now); done {
		return ctrl.Result{}, true, err
	}
	if result, done, err := r.handlePauseTimeout(ctx, box, now); done {
		return result, true, err
	}
	return ctrl.Result{RequeueAfter: r.calcTimeoutRequeue(box, now)}, false, nil
}

// handlePauseTimeout triggers auto-pause when PauseTime has been reached.
// It returns (result, true, err) when the caller should return immediately,
// or (_, false, nil) when reconciliation should continue.
func (r *SandboxReconciler) handlePauseTimeout(ctx context.Context, box *agentsv1alpha1.Sandbox, now metav1.Time) (ctrl.Result, bool, error) {
	if box.Spec.PauseTime == nil || box.Spec.Paused {
		return ctrl.Result{}, false, nil
	}
	if !pauseTimeReached(box.Spec.PauseTime, now) {
		return ctrl.Result{}, false, nil
	}

	klog.InfoS("sandbox pause time reached", "sandbox", klog.KObj(box))
	modified := box.DeepCopy()
	// Optimistic-lock so concurrent writers surface as 409 instead of
	// silently winning a last-writer race.
	patch := client.MergeFromWithOptions(box, client.MergeFromWithOptimisticLock{})
	modified.Spec.Paused = true

	// If the sandbox has a paused-retention policy, extend ShutdownTime so the
	// sandbox is preserved for the configured duration after being paused.
	if retention, managed := r.resolveRetentionAnnotationOrDefault(box); managed {
		if box.Spec.ShutdownTime != nil {
			newShutdown := metav1.NewTime(pausedretention.PausedShutdownTime(now.Time, retention))
			modified.Spec.ShutdownTime = &newShutdown
			// Keep PauseTime aligned so the next connect/resume can preserve auto-pause mode.
			modified.Spec.PauseTime = &newShutdown
		}
	}

	if err := r.Patch(ctx, modified, patch); err != nil {
		if errors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, true, nil
		}
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{}, true, nil
}

// handleShutdownTimeout deletes the sandbox when ShutdownTime has been reached.
// When a paused-retention annotation is present, the shutdown is deferred until
// after pause has had a chance to execute (pause extends ShutdownTime).
func (r *SandboxReconciler) handleShutdownTimeout(ctx context.Context, box *agentsv1alpha1.Sandbox, now metav1.Time) (bool, error) {
	if box.Spec.ShutdownTime == nil || !box.DeletionTimestamp.IsZero() {
		return false, nil
	}
	if !box.Spec.ShutdownTime.Before(&now) {
		return false, nil
	}

	// When the paused-retention annotation is present, the sandbox has not
	// yet paused, AND PauseTime has already been reached, skip deletion:
	// handlePauseTimeout will fire in this same reconcile, pause the sandbox,
	// and extend ShutdownTime.
	// We only skip when pauseTimeReached so that handlePauseTimeout can
	// actually act in the same loop. If PauseTime is nil or still in the
	// future, we must proceed with deletion.
	if _, hasRetention := box.Annotations[agentsv1alpha1.AnnotationReservePausedSandboxDuration]; hasRetention &&
		!box.Spec.Paused &&
		pauseTimeReached(box.Spec.PauseTime, now) {
		return false, nil
	}

	klog.InfoS("sandbox shutdown time reached, deleting", "sandbox", klog.KObj(box), "shutdownTime", box.Spec.ShutdownTime)
	return true, r.Delete(ctx, box)
}

// calcTimeoutRequeue returns the nearest requeue duration based on pending
// PauseTime and ShutdownTime that have not yet been reached.
func (r *SandboxReconciler) calcTimeoutRequeue(box *agentsv1alpha1.Sandbox, now metav1.Time) time.Duration {
	var requeueAfter time.Duration
	if box.Spec.PauseTime != nil && !box.Spec.Paused {
		if delta := box.Spec.PauseTime.Sub(now.Time); delta > 0 {
			requeueAfter = delta
		}
	}
	if box.Spec.ShutdownTime != nil && box.DeletionTimestamp.IsZero() {
		if delta := box.Spec.ShutdownTime.Sub(now.Time); delta > 0 && (requeueAfter == 0 || delta < requeueAfter) {
			requeueAfter = delta
		}
	}
	return requeueAfter
}

// resolveRetentionAnnotationOrDefault parses the paused-retention annotation value.
// On parse failure, it logs a warning and returns the default retention duration
// without mutating the annotation.
func (r *SandboxReconciler) resolveRetentionAnnotationOrDefault(box *agentsv1alpha1.Sandbox) (time.Duration, bool) {
	retention, managed, err := pausedretention.ResolveReservePausedSandboxDurationAnnotation(box.Annotations)
	if err == nil {
		return retention, managed
	}
	raw := box.Annotations[agentsv1alpha1.AnnotationReservePausedSandboxDuration]

	klog.ErrorS(err, "invalid reserve paused sandbox annotation, using default",
		"sandbox", klog.KObj(box),
		"annotation", agentsv1alpha1.AnnotationReservePausedSandboxDuration,
		"value", raw)
	return timeoututils.ForeverReservePausedSandboxDuration, true
}
