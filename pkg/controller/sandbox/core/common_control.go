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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/distribution/reference"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/tracing"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/inplaceupdate"
	"go.opentelemetry.io/otel/attribute"
)

const CommonControlName = "common"

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
	lifecycleHookFunc    LifecycleHookFunc
	initializer          SandboxInitializer
	recycleControl       *SandboxRecycleControl
	upgradeControl       *UpgradeControl
	syncStatusFromPod    func(pod *corev1.Pod, newStatus *agentsv1alpha1.SandboxStatus, syncReadyCondition bool)
}

func NewCommonControl(args SandboxControlArgs) SandboxControl {
	initializer := &defaultSandboxInitializer{
		client:          args.Client,
		apiReader:       args.APIReader,
		storageRegistry: storages.NewStorageProvider(),
		recorder:        args.Recorder,
	}
	control := &commonControl{
		Client:               args.Client,
		recorder:             args.Recorder,
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(args.Client, inplaceupdate.DefaultGeneratePatchBodyFunc),
		rateLimiter:          args.RateLimiter,
		checkpointControl:    args.CheckpointControl,
		podControl:           args.PodControl,
		lifecycleHookFunc:    ExecuteLifecycleHook,
		initializer:          initializer,
		recycleControl:       NewSandboxRecycleControl(args.Client, args.Recorder, args.RecycleConfig),
		syncStatusFromPod:    defaultSyncStatusFromPod,
	}
	control.upgradeControl = NewUpgradeControl(args.Client, args.CheckpointControl, args.PodControl, ExecuteLifecycleHook, initializer, control.syncStatusFromPod)
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
		_, err := r.podControl.CreatePod(ctx, CreatePodArgs{Box: box, NewStatus: newStatus})
		return 0, err
	}

	// pod status running
	if pod.Status.Phase == corev1.PodRunning {
		newStatus.Phase = agentsv1alpha1.SandboxRunning
		r.syncStatusFromPod(pod, newStatus, true)
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
		if err := r.initializer.Initialize(ctx, box, newStatus); err != nil {
			return err
		}
	}

	// For upgrade policies that do not require pod replacement (e.g.,
	// sandbox-manager triggered inplace update via annotation), perform
	// inplace update directly without entering the full upgrade lifecycle
	// (PreUpgrade -> UpgradePod -> PostUpgrade). Recreate and CheckpointRestore
	// are excluded here because they require the full lifecycle.
	if !RequiresPodReplacementUpgrade(box) {
		done, wrote, err := r.handleInplaceUpdateSandbox(ctx, args)
		if err != nil {
			return err
		}
		if wrote {
			// handleInplaceUpdateSandbox performed an actual write (e.g. patched
			// the Pod or set an InplaceUpdate condition). Mark the Reconcile write
			// flag so the enclosing Reconcile span is retained.
			tracing.MarkWrite(ctx)
		}
		if !done {
			// In-place update still in progress: early-return so that
			// syncStatusFromPod does not overwrite the transient
			// Ready=False/InplaceUpdate conditions set during the update.
			return nil
		}
	}
	r.syncStatusFromPod(pod, newStatus, true)
	return nil
}

// defaultSyncStatusFromPod is the default implementation of syncStatusFromPod.
// It syncs sandbox status from pod info and, when syncReadyCondition is true, also
// syncs the Ready condition and detects container startup failures.
func defaultSyncStatusFromPod(pod *corev1.Pod, newStatus *agentsv1alpha1.SandboxStatus, syncReadyCondition bool) {
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
		cond.Reason = agentsv1alpha1.SandboxReadyReasonPodReady
		cond.Message = ""
	}
	for _, cStatus := range pod.Status.ContainerStatuses {
		// indicating container startup failure
		if cond.Status == metav1.ConditionFalse && cStatus.State.Waiting != nil {
			cond.Reason = agentsv1alpha1.SandboxReadyReasonStartContainerFailed
			cond.Message = cStatus.State.Waiting.Message
		}
	}
	utils.SetSandboxCondition(newStatus, *cond)
}

func (r *commonControl) EnsureSandboxPaused(ctx context.Context, args EnsureFuncArgs) error {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
	if cond == nil {
		cond = &metav1.Condition{
			Type:               string(agentsv1alpha1.SandboxConditionPaused),
			Status:             metav1.ConditionFalse,
			Reason:             agentsv1alpha1.SandboxPausedReasonPausing,
			LastTransitionTime: metav1.Now(),
		}
		utils.SetSandboxCondition(newStatus, *cond)
		klog.FromContext(ctx).Info("Paused condition initialized", "sandbox", klog.KObj(box))
		// Clean up checkpoint info on first entry into paused state
		// Fallback: normally no checkpoint delta should exist at this point
		r.checkpointControl.Cleanup(ctx, box)
	} else if cond.Status == metav1.ConditionTrue {
		klog.FromContext(ctx).Info("Paused condition is already true", "sandbox", klog.KObj(box))
		return nil
	}

	// The paused phase sets condition ready to false.
	if rCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady)); rCond != nil && rCond.Status == metav1.ConditionTrue {
		rCond.Status = metav1.ConditionFalse
		rCond.LastTransitionTime = metav1.Now()
		utils.SetSandboxCondition(newStatus, *rCond)
		klog.FromContext(ctx).Info("The paused phase sets condition ready to false", "sandbox", klog.KObj(box))
	}

	// Pod deletion completed, paused completed
	// cond.Status == metav1.ConditionFalse just for sure
	if pod == nil && cond.Status == metav1.ConditionFalse {
		cond.Status = metav1.ConditionTrue
		cond.Reason = agentsv1alpha1.SandboxPausedReasonDeletePod
		cond.LastTransitionTime = metav1.Now()
		utils.SetSandboxCondition(newStatus, *cond)
		klog.FromContext(ctx).Info("Pod deletion completed, pause phase completed", "sandbox", klog.KObj(box))
		return nil
	}
	// Pod deletion incomplete, waiting
	if pod != nil && !pod.DeletionTimestamp.IsZero() {
		klog.FromContext(ctx).Info("Sandbox wait pod paused", "sandbox", klog.KObj(box))
		return nil
	}

	// Validate images and create pod-info checkpoint before deletion
	if rejected := r.checkpointControl.AssumePodCheckpointed(ctx, pod, box, newStatus, cond); rejected {
		return nil
	}

	ctx, deleteSpan := tracing.StartControllerSpan(ctx, tracing.SpanControllerDeletePod,
		attribute.String(tracing.AttrPodName, pod.Name))
	err := client.IgnoreNotFound(r.Delete(ctx, pod, &client.DeleteOptions{GracePeriodSeconds: ptr.To(int64(5))}))
	tracing.EndSpan(ctx, deleteSpan, err)
	if err != nil {
		klog.FromContext(ctx).Error(err, "Delete pod failed", "sandbox", klog.KObj(box))
		return err
	}
	klog.FromContext(ctx).Info("Delete pod success", "sandbox", klog.KObj(box))
	return nil
}

func (r *commonControl) EnsureSandboxResumed(ctx context.Context, args EnsureFuncArgs) error {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus

	// Consider the scenario where a pod is paused and immediately resumed,
	// pod phase may be Running, but the actual state could be Terminating.
	if pod != nil && !pod.DeletionTimestamp.IsZero() {
		return fmt.Errorf("the pods created in the previous stage are still in the terminating state")
	}

	// first create pod
	var err error
	if pod == nil {
		delta := r.checkpointControl.GetPodTemplateDelta(ctx, box)
		_, err = r.podControl.CreatePod(ctx, CreatePodArgs{Box: box, NewStatus: newStatus, PodTemplateDelta: delta})
		return err
	}

	// when pod is running, transition sandbox from resuming to running
	if pod.Status.Phase == corev1.PodRunning && isContainersConsistent(ctx, pod, box) {
		newStatus.Phase = agentsv1alpha1.SandboxRunning
		newStatus.NodeName = pod.Spec.NodeName
		newStatus.SandboxIp = pod.Status.PodIP
		newStatus.PodInfo = agentsv1alpha1.PodInfo{
			PodIP:    pod.Status.PodIP,
			NodeName: pod.Spec.NodeName,
			PodUID:   pod.UID,
		}

		r.checkpointControl.Cleanup(ctx, box)

		// set resumed condition to true after pod is running
		if resumedCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionResumed)); resumedCond != nil &&
			resumedCond.Status == metav1.ConditionFalse {
			resumedCond.Status = metav1.ConditionTrue
			resumedCond.LastTransitionTime = metav1.Now()
			utils.SetSandboxCondition(newStatus, *resumedCond)
		}

		// Every resume cycle needs fresh runtime re-init and CSI re-mount.
		// Unconditionally set Pending so EnsureSandboxUpdated will run Initialize
		// after Pod Ready, regardless of any stale Succeeded from a prior cycle.
		utils.SetSandboxCondition(newStatus, metav1.Condition{
			Type:               string(agentsv1alpha1.RuntimeInitialized),
			Status:             metav1.ConditionFalse,
			Reason:             agentsv1alpha1.SandboxConditionRuntimeInitReasonPending,
			Message:            "Waiting for pod ready before initialization",
			LastTransitionTime: metav1.Now(),
		})
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
// state machine: PreUpgrade → (Checkpointing) → UpgradePod → PostUpgrade → Succeeded.
func (r *commonControl) EnsureSandboxUpgraded(ctx context.Context, args EnsureFuncArgs) error {
	return r.upgradeControl.EnsureSandboxUpgraded(ctx, args)
}

func (r *commonControl) EnsureSandboxTerminated(ctx context.Context, args EnsureFuncArgs) error {
	pod, box, _ := args.Pod, args.Box, args.NewStatus
	var err error
	if pod == nil {
		ctx, span := tracing.StartControllerSpan(ctx, tracing.SpanControllerRemoveFinalizer)
		_, err = utils.PatchFinalizer(ctx, r.Client, box, utils.RemoveFinalizerOpType, SandboxFinalizer)
		tracing.EndSpan(ctx, span, err)
		if err != nil {
			klog.FromContext(ctx).Error(err, "update sandbox finalizer failed", "sandbox", klog.KObj(box))
			return err
		}
		klog.FromContext(ctx).Info("remove sandbox finalizer success", "sandbox", klog.KObj(box))
		return nil
	} else if !pod.DeletionTimestamp.IsZero() {
		klog.FromContext(ctx).Info("Pod is deleting, and wait a moment", "sandbox", klog.KObj(box))
		return nil
	}

	ctx, deleteSpan := tracing.StartControllerSpan(ctx, tracing.SpanControllerDeletePod,
		attribute.String(tracing.AttrPodName, pod.Name))
	err = client.IgnoreNotFound(r.Delete(ctx, pod))
	tracing.EndSpan(ctx, deleteSpan, err)
	if err != nil {
		klog.FromContext(ctx).Error(err, "delete pod failed", "sandbox", klog.KObj(box))
		return err
	}
	klog.FromContext(ctx).Info("delete pod success", "sandbox", klog.KObj(box))
	return nil
}

func (r *commonControl) handleInplaceUpdateSandbox(ctx context.Context, args EnsureFuncArgs) (done bool, wrote bool, err error) {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	handler := &CommonInPlaceUpdateHandler{
		control:  r.inplaceUpdateControl,
		recorder: r.recorder,
	}
	return handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)
}
