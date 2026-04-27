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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"github.com/openkruise/agents/pkg/utils/inplaceupdate"
	"github.com/openkruise/agents/pkg/utils/sidecarutils"
)

const CommonControlName = "common"

// Container waiting reasons defined by kubelet (not exported as public constants in K8s API).
const (
	// WaitingReasonPodInitializing indicates init containers are still running.
	WaitingReasonPodInitializing = "PodInitializing"
	// WaitingReasonContainerCreating indicates the container is being created (image pull, volume mount, etc.).
	WaitingReasonContainerCreating = "ContainerCreating"
)

type commonControl struct {
	client.Client
	recorder             record.EventRecorder
	inplaceUpdateControl *inplaceupdate.InPlaceUpdateControl
	rateLimiter          *RateLimiter
	lifecycleHookFunc    LifecycleHookFunc
}

func NewCommonControl(c client.Client, recorder record.EventRecorder, rl *RateLimiter) SandboxControl {
	control := &commonControl{
		Client:               c,
		recorder:             recorder,
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(c, inplaceupdate.DefaultGeneratePatchBodyFunc),
		rateLimiter:          rl,
		lifecycleHookFunc:    ExecuteLifecycleHook,
	}
	return control
}

func (r *commonControl) EnsureSandboxRunning(ctx context.Context, args EnsureFuncArgs) (time.Duration, error) {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	// If the Pod does not exist, it must first be created.
	if pod == nil {
		if requeueAfter, shouldReturn := r.rateLimiter.getRateLimitDuration(ctx, pod, box); shouldReturn {
			return requeueAfter, nil
		}
		_, err := r.createPod(ctx, box, newStatus)
		return 0, err
	}

	// pod status running
	if pod.Status.Phase == corev1.PodRunning {
		newStatus.Phase = agentsv1alpha1.SandboxRunning
		syncSandboxStatusFromPod(pod, newStatus)
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
	// For non-Recreate upgrade policy (e.g., sandbox-manager triggered inplace update via annotation),
	// perform inplace update directly without entering the full upgrade lifecycle (PreUpgrade -> UpgradePod -> PostUpgrade).
	if box.Spec.UpgradePolicy == nil || box.Spec.UpgradePolicy.Type != agentsv1alpha1.SandboxUpgradePolicyRecreate {
		done, err := r.handleInplaceUpdateSandbox(ctx, args)
		if err != nil {
			return err
		} else if !done {
			return nil
		}
	}
	syncSandboxStatusFromPod(pod, newStatus)
	return nil
}

// syncSandboxStatusFromPod updates sandbox status from pod info and syncs the Ready condition
// with container startup failure detection.
func syncSandboxStatusFromPod(pod *corev1.Pod, newStatus *agentsv1alpha1.SandboxStatus) {
	newStatus.NodeName = pod.Spec.NodeName
	newStatus.SandboxIp = pod.Status.PodIP
	newStatus.PodInfo = agentsv1alpha1.PodInfo{
		PodIP:    pod.Status.PodIP,
		NodeName: pod.Spec.NodeName,
		PodUID:   pod.UID,
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
			Reason:             agentsv1alpha1.SandboxPausedReasonDeletePod,
			LastTransitionTime: metav1.Now(),
		}
		utils.SetSandboxCondition(newStatus, *cond)
		klog.InfoS("Paused condition initialized", "sandbox", klog.KObj(box))
	} else if cond.Status == metav1.ConditionTrue {
		klog.InfoS("Paused condition is already true", "sandbox", klog.KObj(box))
		return nil
	}

	// The paused phase sets condition ready to false.
	if rCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady)); rCond != nil && rCond.Status == metav1.ConditionTrue {
		rCond.Status = metav1.ConditionFalse
		rCond.LastTransitionTime = metav1.Now()
		utils.SetSandboxCondition(newStatus, *rCond)
		klog.InfoS("The paused phase sets condition ready to false", "sandbox", klog.KObj(box))
	}

	// Pod deletion completed, paused completed
	// cond.Status == metav1.ConditionFalse just for sure
	if pod == nil && cond.Status == metav1.ConditionFalse {
		cond.Status = metav1.ConditionTrue
		cond.LastTransitionTime = metav1.Now()
		utils.SetSandboxCondition(newStatus, *cond)
		klog.InfoS("Pod deletion completed, pause phase completed", "sandbox", klog.KObj(box))
		return nil
	}
	// Pod deletion incomplete, waiting
	if pod != nil && !pod.DeletionTimestamp.IsZero() {
		klog.InfoS("Sandbox wait pod paused", "sandbox", klog.KObj(box))
		return nil
	}
	err := client.IgnoreNotFound(r.Delete(ctx, pod, &client.DeleteOptions{GracePeriodSeconds: ptr.To(int64(5))}))
	if err != nil {
		klog.ErrorS(err, "Delete pod failed", "sandbox", klog.KObj(box))
		return err
	}
	klog.InfoS("Delete pod success", "sandbox", klog.KObj(box))
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
		_, err = r.createPod(ctx, box, newStatus)
		return err
	}

	// create pod success, set resumed condition to true
	if resumedCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionResumed)); resumedCond != nil && resumedCond.Status == metav1.ConditionFalse {
		resumedCond.Status = metav1.ConditionTrue
		resumedCond.LastTransitionTime = metav1.Now()
		utils.SetSandboxCondition(newStatus, *resumedCond)
	}

	// when pod is ready, sandbox status from resuming to running
	pCond := utils.GetPodCondition(&pod.Status, corev1.PodReady)
	if pod.Status.Phase == corev1.PodRunning && pCond != nil && pCond.Status == corev1.ConditionTrue {
		newStatus.Phase = agentsv1alpha1.SandboxRunning
		rCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady))
		rCond.Status = metav1.ConditionStatus(pCond.Status)
		rCond.LastTransitionTime = pCond.LastTransitionTime
		utils.SetSandboxCondition(newStatus, *rCond)
	}
	return nil
}

// hasUpgradeAction checks if the sandbox has a non-empty upgrade action configured.
// If pre is true, checks PreUpgrade; otherwise checks PostUpgrade.
func hasUpgradeAction(box *agentsv1alpha1.Sandbox, pre bool) bool {
	if box.Spec.Lifecycle == nil {
		return false
	}
	var action *agentsv1alpha1.UpgradeAction
	if pre {
		action = box.Spec.Lifecycle.PreUpgrade
	} else {
		action = box.Spec.Lifecycle.PostUpgrade
	}
	if action == nil || action.Exec == nil || len(action.Exec.Command) == 0 {
		return false
	}
	return true
}

func (r *commonControl) EnsureSandboxUpgraded(ctx context.Context, args EnsureFuncArgs) (retErr error) {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus

	// Set Ready=False during upgrade
	utils.SetSandboxCondition(newStatus, metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxConditionReady),
		Status:             metav1.ConditionFalse,
		Reason:             agentsv1alpha1.SandboxReadyReasonUpgrading,
		Message:            "sandbox is upgrading",
		LastTransitionTime: metav1.Now(),
	})
	upgradeCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
	// Phase 1: First entry - execute preUpgrade and initialize
	if upgradeCond == nil {
		upgradeCond = &metav1.Condition{
			Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
			Status:             metav1.ConditionFalse,
			Reason:             agentsv1alpha1.SandboxUpgradingReasonPreUpgrade,
			LastTransitionTime: metav1.Now(),
		}
		utils.SetSandboxCondition(newStatus, *upgradeCond)
	}

	switch upgradeCond.Reason {
	case agentsv1alpha1.SandboxUpgradingReasonPreUpgrade, agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed:
		// Execute preUpgrade if configured
		if hasUpgradeAction(box, true) {
			var action *agentsv1alpha1.UpgradeAction
			if box.Spec.Lifecycle != nil {
				action = box.Spec.Lifecycle.PreUpgrade
			}
			result := r.executeUpgradeAction(ctx, pod, box, action)
			klog.InfoS("preUpgrade result", "sandbox", klog.KObj(box), "succeeded", result.Succeeded, "message", result.Message)
			if !result.Succeeded {
				// Record preUpgrade action failed
				upgradeCond.Message = result.Message
				upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed
				utils.SetSandboxCondition(newStatus, *upgradeCond)
				return nil
			}
		}

		// transfer to UpgradePod
		klog.InfoS("preUpgrade completed, transitioning to UpgradePod", "sandbox", klog.KObj(box))
		upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonUpgradePod
		upgradeCond.Message = ""
		utils.SetSandboxCondition(newStatus, *upgradeCond)
		fallthrough
	case agentsv1alpha1.SandboxUpgradingReasonUpgradePod, agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed:
		done, err := r.performRecreateUpgrade(ctx, args)
		if err != nil {
			klog.ErrorS(err, "UpgradePod step failed", "sandbox", klog.KObj(box))
			return err
		} else if !done {
			klog.InfoS("UpgradePod step in progress", "sandbox", klog.KObj(box))
			return nil // upgrade in progress
		}

		klog.InfoS("UpgradePod step completed, transitioning to PostUpgrade", "sandbox", klog.KObj(box))

		// Re-fetch the Pod after recreate upgrade, since the old pod object is stale (deleted and replaced).
		var freshPod corev1.Pod
		if err := r.Get(ctx, types.NamespacedName{Namespace: box.Namespace, Name: box.Name}, &freshPod); err != nil {
			klog.ErrorS(err, "Failed to re-fetch pod after recreate upgrade", "sandbox", klog.KObj(box))
			return err
		}
		pod = &freshPod

		// UpgradePod step completed
		upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonPostUpgrade
		upgradeCond.Message = ""
		utils.SetSandboxCondition(newStatus, *upgradeCond)
		fallthrough
	case agentsv1alpha1.SandboxUpgradingReasonPostUpgrade, agentsv1alpha1.SandboxUpgradingReasonPostUpgradeFailed:
		// Execute postUpgrade if configured
		if hasUpgradeAction(box, false) {
			var action *agentsv1alpha1.UpgradeAction
			if box.Spec.Lifecycle != nil {
				action = box.Spec.Lifecycle.PostUpgrade
			}
			result := r.executeUpgradeAction(ctx, pod, box, action)
			klog.InfoS("postUpgrade result", "sandbox", klog.KObj(box), "succeeded", result.Succeeded, "message", result.Message)
			if !result.Succeeded {
				// Record postUpgrade action failed
				upgradeCond.Message = result.Message
				upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonPostUpgradeFailed
				utils.SetSandboxCondition(newStatus, *upgradeCond)
				return nil
			}
		}

		klog.InfoS("postUpgrade completed, transitioning to Succeeded", "sandbox", klog.KObj(box))
		upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonSucceeded
		upgradeCond.Status = metav1.ConditionTrue
		upgradeCond.Message = ""
		upgradeCond.LastTransitionTime = metav1.Now()
		utils.SetSandboxCondition(newStatus, *upgradeCond)
		newStatus.Phase = agentsv1alpha1.SandboxRunning
		utils.SetSandboxCondition(newStatus, metav1.Condition{
			Type:               string(agentsv1alpha1.SandboxConditionReady),
			Status:             metav1.ConditionTrue,
			Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
			Message:            "",
			LastTransitionTime: metav1.Now(),
		})
	}

	return nil
}

func (r *commonControl) EnsureSandboxTerminated(ctx context.Context, args EnsureFuncArgs) error {
	pod, box, _ := args.Pod, args.Box, args.NewStatus
	var err error
	if pod == nil {
		_, err = utils.PatchFinalizer(ctx, r.Client, box, utils.RemoveFinalizerOpType, utils.SandboxFinalizer)
		if err != nil {
			klog.ErrorS(err, "update sandbox finalizer failed", "sandbox", klog.KObj(box))
			return err
		}
		klog.InfoS("remove sandbox finalizer success", "sandbox", klog.KObj(box))
		return nil
	} else if !pod.DeletionTimestamp.IsZero() {
		klog.InfoS("Pod is deleting, and wait a moment", "sandbox", klog.KObj(box))
		return nil
	}

	err = client.IgnoreNotFound(r.Delete(ctx, pod))
	if err != nil {
		klog.ErrorS(err, "delete pod failed", "sandbox", klog.KObj(box))
		return err
	}
	klog.InfoS("delete pod success", "sandbox", klog.KObj(box))
	return nil
}

func (r *commonControl) createPod(ctx context.Context, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus) (*corev1.Pod, error) {
	pod, err := GeneratePodFromSandbox(ctx, r.Client, box, newStatus.UpdateRevision)
	if err != nil {
		return nil, err
	}

	// to avoid the performance issue, using the controller to inject csi containers
	// fetch the configmap and parse the configuration based on the controller runtime
	podTemplate := &pod.Spec
	injectErr := sidecarutils.InjectPodTemplateCSIAndRuntimeSidecar(ctx, box, podTemplate, r.Client)
	if injectErr != nil {
		klog.ErrorS(injectErr, "failed to inject pod template with csi sidecar or runtime sidecar", "sandbox", klog.KObj(box))
		return nil, injectErr
	}

	ScaleExpectation.ExpectScale(GetControllerKey(box), expectations.Create, box.Name)
	err = r.Create(ctx, pod)
	if err != nil {
		ScaleExpectation.ObserveScale(GetControllerKey(box), expectations.Create, box.Name)
		if !errors.IsAlreadyExists(err) {
			klog.ErrorS(err, "create pod failed", "sandbox", klog.KObj(box))
			return nil, err
		}
	}
	klog.InfoS("Create pod success", "sandbox", klog.KObj(box), "Body", utils.DumpJson(pod))
	return pod, nil
}

func (r *commonControl) handleInplaceUpdateSandbox(ctx context.Context, args EnsureFuncArgs) (bool, error) {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	handler := &CommonInPlaceUpdateHandler{
		control:  r.inplaceUpdateControl,
		recorder: r.recorder,
	}
	return handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)
}

// performRecreateUpgrade handles the Recreate upgrade step (delete old pod + create new pod).
func (r *commonControl) performRecreateUpgrade(ctx context.Context, args EnsureFuncArgs) (bool, error) {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus

	// Step 1: Delete old Pod (has old template hash)
	if pod != nil && pod.Labels[agentsv1alpha1.PodLabelTemplateHash] != newStatus.UpdateRevision {
		if !pod.DeletionTimestamp.IsZero() {
			klog.InfoS("Waiting for pod deletion to complete", "sandbox", klog.KObj(box))
			return false, nil
		}
		// Delete pod
		ScaleExpectation.ExpectScale(GetControllerKey(box), expectations.Delete, box.Name)
		if err := r.Delete(ctx, pod); err != nil {
			ScaleExpectation.ObserveScale(GetControllerKey(box), expectations.Delete, box.Name)
			if !errors.IsNotFound(err) {
				klog.ErrorS(err, "Failed to delete pod for upgrade", "sandbox", klog.KObj(box))
				return false, err
			}
		}
		klog.InfoS("Deleted old pod for upgrade", "sandbox", klog.KObj(box))
		return false, nil
	}

	// Step 2: Create new Pod (old pod deleted)
	if pod == nil {
		klog.InfoS("Creating new pod for upgrade", "sandbox", klog.KObj(box))
		newPod, err := r.createPod(ctx, box, newStatus)
		if err != nil {
			klog.ErrorS(err, "Failed to create new pod for upgrade", "sandbox", klog.KObj(box))
			return false, err
		}
		if newPod != nil {
			klog.InfoS("New pod created for upgrade", "sandbox", klog.KObj(box), "pod", klog.KObj(newPod))
		}
		return false, nil
	}

	if pod.Status.PodIP != "" && pod.Spec.NodeName != "" {
		newStatus.NodeName = pod.Spec.NodeName
		newStatus.SandboxIp = pod.Status.PodIP
		newStatus.PodInfo = agentsv1alpha1.PodInfo{
			PodIP:    pod.Status.PodIP,
			NodeName: pod.Spec.NodeName,
			PodUID:   pod.UID,
		}
	}

	// Step 3: Wait for new Pod to be running and ready
	pCond := utils.GetPodCondition(&pod.Status, corev1.PodReady)
	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
	if pCond == nil || pCond.Status != corev1.ConditionTrue {
		klog.InfoS("Waiting for new pod to be ready", "sandbox", klog.KObj(box))
		for _, cStatus := range pod.Status.ContainerStatuses {
			if cStatus.State.Waiting != nil {
				reason := cStatus.State.Waiting.Reason
				// PodInitializing and ContainerCreating are normal transient states during startup, skip them
				if reason == WaitingReasonPodInitializing || reason == WaitingReasonContainerCreating {
					continue
				}
				// Other waiting reasons indicate container startup failure
				klog.InfoS("container waiting with abnormal reason", "sandbox", klog.KObj(box),
					"container", cStatus.Name, "reason", reason, "message", cStatus.State.Waiting.Message)
				cond.Reason = agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed
				cond.Message = fmt.Sprintf("container %s: %s - %s", cStatus.Name, reason, cStatus.State.Waiting.Message)
				utils.SetSandboxCondition(newStatus, *cond)
			} else if cStatus.State.Terminated != nil {
				klog.InfoS("container terminated unexpectedly", "sandbox", klog.KObj(box),
					"container", cStatus.Name, "reason", cStatus.State.Terminated.Reason,
					"exitCode", cStatus.State.Terminated.ExitCode, "message", cStatus.State.Terminated.Message)
				cond.Reason = agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed
				cond.Message = fmt.Sprintf("container %s: terminated with exit code %d - %s",
					cStatus.Name, cStatus.State.Terminated.ExitCode, cStatus.State.Terminated.Reason)
				utils.SetSandboxCondition(newStatus, *cond)
			}
		}
		return false, nil
	}
	return true, nil
}

// upgradeActionResult represents the result of executing an upgrade hook.
type upgradeActionResult struct {
	Succeeded bool
	Message   string
}

// executeUpgradeAction executes an upgrade action and returns the result.
// If action is nil (not configured), it returns success directly.
// If pod is nil and action is configured, it returns failure.
func (r *commonControl) executeUpgradeAction(ctx context.Context, pod *corev1.Pod, box *agentsv1alpha1.Sandbox, action *agentsv1alpha1.UpgradeAction) upgradeActionResult {
	if action == nil {
		return upgradeActionResult{Succeeded: true, Message: "no hook configured, skipped"}
	}
	if pod == nil {
		return upgradeActionResult{Succeeded: false, Message: "pod not found, cannot execute hook"}
	}

	exitCode, stdout, stderr, err := r.lifecycleHookFunc(ctx, box, action)
	if err != nil {
		return upgradeActionResult{Succeeded: false, Message: fmt.Sprintf("hook execution error: %v", err)}
	}
	if exitCode != 0 {
		return upgradeActionResult{Succeeded: false, Message: fmt.Sprintf("hook failed with exit code %d, stdout: %s, stderr: %s", exitCode, stdout, stderr)}
	}
	return upgradeActionResult{Succeeded: true, Message: fmt.Sprintf("hook succeeded, stdout: %s", stdout)}
}
