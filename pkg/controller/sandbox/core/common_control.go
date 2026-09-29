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

package core

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/distribution/reference"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/tracing"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/inplaceupdate"
)

const CommonControlName = "common"

// eventReasonPodOwnerMismatch is the event reason emitted when an existing
// pod is owned by a previous sandbox generation with the same name.
const eventReasonPodOwnerMismatch = "PodOwnerMismatch"

// Container waiting reasons defined by kubelet (not exported as public constants in K8s API).
const (
	// WaitingReasonPodInitializing indicates init containers are still running.
	WaitingReasonPodInitializing = "PodInitializing"
	// WaitingReasonContainerCreating indicates the container is being created (image pull, volume mount, etc.).
	WaitingReasonContainerCreating = "ContainerCreating"

	SandboxFinalizer = "agents.kruise.io/sandbox"

	PodConditionContainersPaused  = "ContainersPaused"
	PodConditionContainersResumed = "ContainersResumed"
	PodConditionResetComplete     = "ResetComplete"

	PodConditionResetReasonSucceeded = "ResetSucceeded"
	PodConditionResetReasonFailed    = "ResetFailed"
	PodConditionResetReasonTimeout   = "ResetTimeout"
)

type commonControl struct {
	client.Client
	recorder             record.EventRecorder
	inplaceUpdateControl *inplaceupdate.InPlaceUpdateControl
	rateLimiter          *RateLimiter
	checkpointControl    *CheckpointControl
	podControl           *PodControl
	probeManager         *PodProbeManager
	lifecycleHookFunc    LifecycleHookFunc
	initializer          SandboxInitializer
	recycleControl       *SandboxRecycleControl
	upgradeControl       *UpgradeControl
	syncStatusFromPod    func(pod *corev1.Pod, newStatus *agentsv1alpha1.SandboxStatus, syncReadyCondition bool)
}

// ResumeFunc resumes a paused sandbox: creates the pod if missing and sets
// the Resumed condition when the pod is running. It does NOT change the
// sandbox phase — that responsibility stays with EnsureSandboxResumed, which
// is why the upgrade path can reuse it.
type ResumeFunc func(ctx context.Context, args EnsureFuncArgs) error

func NewCommonControl(args SandboxControlArgs) SandboxControl {
	lifecycleHookFunc := NewLifecycleHookFunc(args.RuntimeTLSBundle)

	initializer := &defaultSandboxInitializer{
		client:          args.Client,
		apiReader:       args.APIReader,
		storageRegistry: storages.NewStorageProvider(),
		recorder:        args.Recorder,
		tlsBundle:       args.RuntimeTLSBundle,
	}
	control := &commonControl{
		Client:               args.Client,
		recorder:             args.Recorder,
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(args.Client, inplaceupdate.DefaultGeneratePatchBodyFunc),
		rateLimiter:          args.RateLimiter,
		checkpointControl:    args.CheckpointControl,
		podControl:           args.PodControl,
		probeManager:         NewPodProbeManager(args.Client, args.Recorder),
		lifecycleHookFunc:    lifecycleHookFunc,
		initializer:          initializer,
		recycleControl:       NewSandboxRecycleControl(args.Client, args.Recorder, args.RecycleConfig),
		syncStatusFromPod: func(pod *corev1.Pod, newStatus *agentsv1alpha1.SandboxStatus, syncReadyCondition bool) {
			defaultSyncStatusFromPod(pod, newStatus, syncReadyCondition, classifyStartupFailure)
		},
	}
	control.upgradeControl = NewUpgradeControl(args.Client, args.CheckpointControl, args.PodControl, args.Recorder, lifecycleHookFunc, initializer, control.syncStatusFromPod, control.handleResume, control.inplaceUpdateControl)
	return control
}

func (r *commonControl) EnsureSandboxRecycled(ctx context.Context, args EnsureFuncArgs) (time.Duration, error) {
	return r.recycleControl.ensureSandboxRecycled(ctx, args)
}

func (r *commonControl) EnsureSandboxRunning(ctx context.Context, args EnsureFuncArgs) (time.Duration, error) {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	// If the Pod does not exist, it must first be created.
	if pod == nil {
		if requeueAfter, shouldReturn := r.rateLimiter.getRateLimitDuration(ctx, pod, box); shouldReturn {
			return requeueAfter, nil
		}
		_, err := r.podControl.CreatePod(ctx, CreatePodArgs{Box: box, NewStatus: newStatus, AdvertiseRuntimeTLS: true})
		return 0, err
	}

	// A pod owned by a previous sandbox generation with the same name must not
	// be adopted (issue #756): stay Pending, surface an event, and wait for the
	// GC controller to reclaim it. The pod watch retriggers the reconcile once
	// it is gone, then a fresh pod is created.
	if staleOwner, stale := StaleSandboxPodOwner(pod, box); stale {
		klog.FromContext(ctx).Info("existing pod is owned by a previous sandbox generation, waiting for GC to delete it",
			"sandbox", klog.KObj(box), "pod", klog.KObj(pod),
			"podOwnerUID", string(staleOwner), "sandboxUID", string(box.UID))
		r.recorder.Eventf(box, corev1.EventTypeWarning, eventReasonPodOwnerMismatch,
			"pod %s is owned by sandbox uid %s, not the current sandbox uid %s; waiting for GC to delete it",
			pod.Name, staleOwner, box.UID)
		return 0, nil
	}

	r.syncStatusFromPod(pod, newStatus, true)
	if pod.Status.Phase == corev1.PodRunning {
		newStatus.Phase = agentsv1alpha1.SandboxRunning
		return 0, nil
	}

	return 0, nil
}

func (r *commonControl) EnsureSandboxUpdated(ctx context.Context, args EnsureFuncArgs) error {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	// If a Pod is no longer present in the Running state, it should be considered an abnormal situation.
	if pod == nil {
		newStatus.Phase = agentsv1alpha1.SandboxFailed
		newStatus.Message = "Sandbox Pod Not Found"
		return nil
	}

	// If RuntimeInitialized is pending (set during resume), wait for Pod Ready then run Initialize
	initCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.RuntimeInitialized))
	if initCond != nil && initCond.Status != metav1.ConditionTrue {
		pCond := utils.GetPodCondition(&pod.Status, corev1.PodReady)
		if pCond == nil || pCond.Status != corev1.ConditionTrue {
			klog.FromContext(ctx).Info("Waiting for pod ready before initialization", "sandbox", klog.KObj(box))
			return nil
		}
		// Trace the initialization so its latency is observable in Jaeger;
		// the writes it performs are tracked by the write-tracking client.
		ctx, span := tracing.StartControllerSpan(ctx, tracing.SpanControllerAgentRuntimeInit)
		err := r.initializer.Initialize(ctx, box, newStatus)
		tracing.EndSpan(ctx, span, err)
		if err != nil {
			return err
		}
	}

	// Sandboxes without an upgrade policy apply template changes in place
	// directly from the Running phase, without entering the upgrade lifecycle
	// (PreUpgrade -> UpgradePod -> PostUpgrade). This is the SandboxClaim
	// delivery path: the sandbox must stay Running so the claim can be served.
	// Every explicit upgrade policy — including InplaceUpdate — is excluded here
	// because it runs through the Upgrading phase instead.
	if !RequiresUpgradeSandbox(box) {
		_, err := r.handleInplaceUpdateSandbox(ctx, args)
		// done covers both success and terminal failure, so it must not be used to
		// override the Ready set by the adapter.
		// args.Pod may be the pre-patch object; only sync status info here, and the
		// Pod watch will trigger later observations.
		r.syncStatusFromPod(pod, newStatus, false)
		return err
	}
	// Ensure probe configurations are valid and take effect on the pod.
	if err := r.probeManager.EnsureProbe(ctx, box, pod, newStatus); err != nil {
		klog.ErrorS(err, "failed to ensure pod probe", "sandbox", klog.KObj(box))
		return err
	}

	r.syncStatusFromPod(pod, newStatus, true)
	return nil
}

// defaultSyncStatusFromPod is the default implementation of syncStatusFromPod.
// It synchronizes Pod identity and, when requested, normalizes Ready failures
// through the controller-specific startup failure normalizer.
func defaultSyncStatusFromPod(
	pod *corev1.Pod,
	newStatus *agentsv1alpha1.SandboxStatus,
	syncReadyCondition bool,
	normalizePodStartupFailure func(*corev1.Pod) (reason, message string, failed bool),
) {
	newStatus.NodeName = pod.Spec.NodeName
	newStatus.SandboxIp = pod.Status.PodIP
	newStatus.PodInfo = agentsv1alpha1.PodInfo{
		PodIP:    pod.Status.PodIP,
		NodeName: pod.Spec.NodeName,
		PodUID:   pod.UID,
	}
	if !syncReadyCondition {
		return
	}
	pCond := utils.GetPodCondition(&pod.Status, corev1.PodReady)
	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady))
	reason, message, failed := "", "", false
	if normalizePodStartupFailure != nil {
		reason, message, failed = normalizePodStartupFailure(pod)
	}
	// Keep ordinary Pending Pods condition-free. Unclassified startup delays
	// remain governed by the SandboxSet ResourcePending timeout.
	if cond == nil && pCond == nil && !failed {
		return
	}
	if cond == nil {
		cond = &metav1.Condition{
			Type:               string(agentsv1alpha1.SandboxConditionReady),
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
		}
	}
	if pCond != nil && string(pCond.Status) != string(cond.Status) {
		cond.Status = metav1.ConditionStatus(pCond.Status)
		cond.LastTransitionTime = pCond.LastTransitionTime
		if cond.Status == metav1.ConditionTrue {
			// Flipping to Ready clears any prior failure classification.
			cond.Reason = agentsv1alpha1.SandboxReadyReasonPodReady
			cond.Message = ""
		} else {
			// Preserve non-startup failure reasons until they are superseded by
			// the normalizer or the Pod becomes Ready.
			cond.Message = pCond.Message
		}
	}
	if failed && cond.Status != metav1.ConditionFalse {
		cond.Status = metav1.ConditionFalse
		cond.LastTransitionTime = metav1.Now()
	}
	if cond.Status == metav1.ConditionFalse {
		if failed {
			cond.Reason = reason
			cond.Message = message
		} else if cond.Reason == agentsv1alpha1.SandboxReadyReasonStartContainerFailed ||
			cond.Reason == agentsv1alpha1.SandboxReadyReasonUnschedulable {
			// Only normalizer-owned startup failures are cleared on recovery.
			cond.Reason = agentsv1alpha1.SandboxReadyReasonPodReady
			cond.Message = ""
		}
	}
	utils.SetSandboxCondition(newStatus, *cond)
}

func (r *commonControl) EnsureSandboxPaused(ctx context.Context, args EnsureFuncArgs) error {
	// commonControl only supports the Stop pause strategy.
	return ensureStopPaused(ctx, r.Client, args, agentsv1alpha1.SandboxPausedReasonStopPauseSucceed)
}

func (r *commonControl) EnsureSandboxResumed(ctx context.Context, args EnsureFuncArgs) error {
	if err := r.handleResume(ctx, args); err != nil {
		return err
	}
	pod, _, newStatus := args.Pod, args.Box, args.NewStatus
	resumedCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionResumed))
	if pod != nil && resumedCond != nil && resumedCond.Status == metav1.ConditionTrue {
		newStatus.Phase = agentsv1alpha1.SandboxRunning
		r.syncStatusFromPod(pod, newStatus, false)
	}
	return nil
}

// handleResume handles the core resume logic: creating the pod if missing and
// setting conditions for resume completion and runtime re-initialization. It
// does not change the sandbox phase — that is the caller's responsibility.
// commonControl only supports the Stop pause strategy, so no checkpoint data
// is involved. Paused-condition and finalizer cleanup happen in the
// controller's finalizeResumePhase once Resumed=True.
func (r *commonControl) handleResume(ctx context.Context, args EnsureFuncArgs) error {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	// Consider the scenario where a pod is paused and immediately resumed,
	// pod phase may be Running, but the actual state could be Terminating.
	if pod != nil && !pod.DeletionTimestamp.IsZero() {
		return fmt.Errorf("the pods created in the previous stage are still in the terminating state")
	}

	// first create pod
	if pod == nil {
		_, err := r.podControl.CreatePod(ctx, CreatePodArgs{Box: box, NewStatus: newStatus, IsResume: true})
		return err
	}

	// when pod is running, transition sandbox from resuming to running
	if pod.Status.Phase == corev1.PodRunning && isContainersConsistent(ctx, pod, box) {
		// Unconditionally set Resumed=True (instead of flipping an existing
		// False) so the upgrade Resuming stage works even when Resumed was
		// not pre-seeded.
		markResumeSucceeded(newStatus)
	}
	return nil
}

// isContainersConsistent verifies that every init container's image in pod.Spec
// matches the corresponding image reported in pod.Status. Returns false if any mismatch or
// missing status is found, indicating the caller should wait for the status to converge.
func isContainersConsistent(ctx context.Context, pod *corev1.Pod, box *agentsv1alpha1.Sandbox) bool {
	initStatusImages := make(map[string]string, len(pod.Status.InitContainerStatuses))
	for _, initStatus := range pod.Status.InitContainerStatuses {
		initStatusImages[initStatus.Name] = initStatus.Image
	}
	for _, initContainer := range pod.Spec.InitContainers {
		statusImage, found := initStatusImages[initContainer.Name]
		if !found {
			klog.FromContext(ctx).Info("init container status not found, waiting",
				"sandbox", klog.KObj(box),
				"container", initContainer.Name)
			return false
		}
		if !imageRefsEqual(initContainer.Image, statusImage) {
			klog.FromContext(ctx).Info("init container image mismatch between spec and status, waiting",
				"sandbox", klog.KObj(box),
				"container", initContainer.Name,
				"specImage", initContainer.Image,
				"statusImage", statusImage)
			return false
		}
	}
	return true
}

// imageRefsEqual compares two image references accounting for registry normalization.
// Container runtimes may expand short names (e.g. "img:latest" → "docker.io/library/img:latest").
func imageRefsEqual(a, b string) bool {
	if a == b {
		return true
	}
	return normalizeImageRef(a) == normalizeImageRef(b)
}

func normalizeImageRef(img string) string {
	named, err := reference.ParseNormalizedNamed(img)
	if err != nil {
		return img
	}
	return reference.TagNameOnly(named).String()
}

// EnsureSandboxUpgraded delegates to UpgradeControl which manages the full upgrade
// state machine: Resuming → PreUpgrade → (Checkpointing) → UpgradePod → PostUpgrade → Succeeded.
func (r *commonControl) EnsureSandboxUpgraded(ctx context.Context, args EnsureFuncArgs) error {
	return r.upgradeControl.EnsureSandboxUpgraded(ctx, args)
}

func (r *commonControl) EnsureSandboxTerminated(ctx context.Context, args EnsureFuncArgs) error {
	pod, box, _ := args.Pod, args.Box, args.NewStatus
	var err error
	if pod == nil {
		if controllerutil.ContainsFinalizer(box, SandboxFinalizer) {
			ctx, span := tracing.StartControllerSpan(ctx, tracing.SpanControllerRemoveFinalizer)
			_, err = utils.PatchFinalizer(ctx, r.Client, box, utils.RemoveFinalizerOpType, SandboxFinalizer)
			tracing.EndSpan(ctx, span, err)
			if err != nil {
				klog.FromContext(ctx).Error(err, "update sandbox finalizer failed", "sandbox", klog.KObj(box))
				return err
			}
			klog.FromContext(ctx).Info("remove sandbox finalizer success", "sandbox", klog.KObj(box))
		}
		return nil
	} else if !pod.DeletionTimestamp.IsZero() {
		klog.FromContext(ctx).Info("Pod is deleting, and wait a moment", "sandbox", klog.KObj(box))
		return nil
	}

	ctx, deleteSpan := tracing.StartControllerSpan(ctx, tracing.SpanControllerDeletePod)
	err = client.IgnoreNotFound(r.Delete(ctx, pod))
	tracing.EndSpan(ctx, deleteSpan, err)
	if err != nil {
		klog.FromContext(ctx).Error(err, "delete pod failed", "sandbox", klog.KObj(box))
		return err
	}
	klog.FromContext(ctx).Info("delete pod success", "sandbox", klog.KObj(box))
	return nil
}

// CommonInPlaceUpdateHandler implements the inplace update handler for common controller
type CommonInPlaceUpdateHandler struct {
	control  *inplaceupdate.InPlaceUpdateControl
	recorder record.EventRecorder
}

func (h *CommonInPlaceUpdateHandler) GetInPlaceUpdateControl() *inplaceupdate.InPlaceUpdateControl {
	return h.control
}

func (h *CommonInPlaceUpdateHandler) GetRecorder() record.EventRecorder {
	return h.recorder
}

func podIsReady(pod *corev1.Pod) bool {
	if pod == nil || !pod.DeletionTimestamp.IsZero() {
		return false
	}
	cond := utils.GetPodCondition(&pod.Status, corev1.PodReady)
	return cond != nil && cond.Status == corev1.ConditionTrue
}

func setUpdateReady(status *agentsv1alpha1.SandboxStatus, ready bool, reason, msg string) {
	value := metav1.ConditionFalse
	if ready {
		value, reason, msg = metav1.ConditionTrue, agentsv1alpha1.SandboxReadyReasonPodReady, ""
	}
	utils.SetSandboxCondition(status, metav1.Condition{Type: string(agentsv1alpha1.SandboxConditionReady),
		Status: value, Reason: reason, Message: utils.TruncateConditionMessage(msg), LastTransitionTime: metav1.Now()})
}

func inplaceWaitMessage(pod *corev1.Pod) string {
	if pod == nil {
		return "waiting for Pod"
	}
	if msg := describeInplaceWaitReason(pod); msg != "" {
		return utils.TruncateConditionMessage(msg)
	}
	for _, cond := range pod.Status.Conditions {
		if (cond.Type == corev1.PodResizePending || cond.Type == corev1.PodResizeInProgress) && cond.Status == corev1.ConditionTrue {
			return utils.TruncateConditionMessage(fmt.Sprintf("waiting for resource resize: %s: %s", cond.Reason, cond.Message))
		}
	}
	return "waiting for target configuration and Pod readiness"
}

func isTerminalInplaceUpdateReason(reason string) bool {
	return reason == agentsv1alpha1.SandboxInplaceUpdateReasonFailed ||
		reason == agentsv1alpha1.SandboxInplaceUpdateReasonUnsupportedResize
}

// isInplaceUpdateTerminal returns true if the InplaceUpdate condition has already
// reached a terminal state that should not be re-evaluated.
func isInplaceUpdateTerminal(newStatus *agentsv1alpha1.SandboxStatus) bool {
	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
	if cond == nil {
		return false
	}
	switch cond.Reason {
	case agentsv1alpha1.SandboxInplaceUpdateReasonFailed,
		agentsv1alpha1.SandboxInplaceUpdateReasonUnsupportedResize,
		agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded:
		return true
	}
	return false
}

func (r *commonControl) handleInplaceUpdateSandbox(ctx context.Context, args EnsureFuncArgs) (done bool, err error) {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	handler := &CommonInPlaceUpdateHandler{
		control:  r.inplaceUpdateControl,
		recorder: r.recorder,
	}
	previous := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
	if previous != nil && isTerminalInplaceUpdateReason(previous.Reason) &&
		previous.ObservedGeneration == box.Generation && box.Status.UpdateRevision == newStatus.UpdateRevision {
		// 无副作用的终态预检查拒绝仍可反映 Pod 健康状态；写入失败和记录损坏不放开 Ready。
		_, validationErr := validateInplaceUpdate(pod, box)
		ready := validationErr != nil && classifyInplaceError(validationErr) != inplaceClassStateCorrupted && podIsReady(pod)
		setUpdateReady(newStatus, ready, agentsv1alpha1.SandboxReadyReasonInplaceUpdating, previous.Message)
		return true, nil
	}
	result, err := handleInPlaceUpdateCommon(ctx, handler.GetInPlaceUpdateControl(), pod, box, newStatus.UpdateRevision)
	hasErr := err != nil
	ready := podIsReady(pod)
	msg := inplaceWaitMessage(pod)
	reason := agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating
	status := metav1.ConditionFalse

	switch {
	case hasErr:
		msg = utils.TruncateConditionMessage(err.Error())
		if isTerminalInplaceError(err) {
			reason = agentsv1alpha1.SandboxInplaceUpdateReasonFailed
			if isUnsupportedResizeError(err) {
				reason = agentsv1alpha1.SandboxInplaceUpdateReasonUnsupportedResize
			}
			done = true

			// 只有无副作用的预检查拒绝可以沿用 Pod 健康状态。
			// 已尝试写入或更新记录损坏时，即使缓存中的 Pod 仍 Ready，也关闭门禁。
			if result == inplaceUpdateStepPatchDelivered || classifyInplaceError(err) == inplaceClassStateCorrupted {
				ready = false
			}
			if previous == nil || previous.Reason != reason || previous.Message != msg || previous.ObservedGeneration != box.Generation {
				if recorder := handler.GetRecorder(); recorder != nil {
					recorder.Event(box, corev1.EventTypeWarning, "InplaceUpdateFailed", msg)
				}
			}
			err = nil
		} else {
			// 结果未知或可重试时，保留错误交给协调器重试，不暴露缓存中的 Ready=True。
			ready = false
		}

	case result == inplaceUpdateStepSucceeded && ready:
		reason = agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded
		status = metav1.ConditionTrue
		msg = ""
		done = true

	case result == inplaceUpdateStepSucceeded:
		msg = "configuration applied; waiting for Pod Ready: " + msg
	}
	// A memory downscale is silently skipped by the resize path's lenient
	// comparison while the image upgrade proceeds as usual; emit an advisory
	// event on the first observed progress for this generation to remind the
	// user that the target memory was not actually reduced.
	if !hasErr && (result == inplaceUpdateStepPatchDelivered || result == inplaceUpdateStepSucceeded) &&
		(previous == nil || previous.ObservedGeneration != box.Generation) {
		if downscaleErr := inplaceupdate.CheckMemoryDownscale(box, pod); downscaleErr != nil {
			if recorder := handler.GetRecorder(); recorder != nil {
				recorder.Event(box, corev1.EventTypeWarning, "MemoryDownscaleSkipped", downscaleErr.Error())
			}
		}
	}
	// SetSandboxCondition does not refresh ObservedGeneration when it updates an
	// existing Condition, so sync the pointer directly here to ensure cross-round
	// Claim consumers and event dedup judge by the current generation.
	if previous != nil {
		previous.ObservedGeneration = box.Generation
	}
	utils.SetSandboxCondition(newStatus, metav1.Condition{
		Type: string(agentsv1alpha1.SandboxConditionInplaceUpdate), Status: status,
		Reason: reason, Message: utils.TruncateConditionMessage(msg), ObservedGeneration: box.Generation, LastTransitionTime: metav1.Now(),
	})
	setUpdateReady(newStatus, ready, agentsv1alpha1.SandboxReadyReasonInplaceUpdating, msg)
	return done, err
}
