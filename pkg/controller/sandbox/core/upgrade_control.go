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

package core

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

// Event reasons for upgrade lifecycle transitions.
const (
	EventUpgradeResuming          = "UpgradeResuming"
	EventUpgradeResumed           = "UpgradeResumed"
	EventUpgradePreUpgradeFailed  = "PreUpgradeFailed"
	EventUpgradePodReplaced       = "UpgradePodReplaced"
	EventUpgradePodFailed         = "UpgradePodFailed"
	EventUpgradePostUpgradeFailed = "PostUpgradeFailed"
	EventUpgradeSucceeded         = "UpgradeSucceeded"
)

// UpgradeControl manages the sandbox upgrade lifecycle state machine.
// It orchestrates the full upgrade flow: PreUpgrade → (Checkpointing) →
// UpgradePod → PostUpgrade → Succeeded, delegating pod operations to
// PodControl and checkpoint operations to CheckpointControl.
type UpgradeControl struct {
	client.Client
	checkpointControl *CheckpointControl
	podControl        *PodControl
	recorder          record.EventRecorder
	lifecycleHookFunc LifecycleHookFunc
	initializer       SandboxInitializer
	syncStatusFromPod func(pod *corev1.Pod, newStatus *agentsv1alpha1.SandboxStatus, syncReadyCondition bool)
	resumeFunc        ResumeFunc
}

// NewUpgradeControl creates a new UpgradeControl.
//
// podControl is passed in as a parameter so that callers can inject a
// customised PodControl (e.g. with a different PodGenerateFunc) when needed.
func NewUpgradeControl(
	cli client.Client,
	checkpointControl *CheckpointControl,
	podControl *PodControl,
	recorder record.EventRecorder,
	lifecycleHookFunc LifecycleHookFunc,
	initializer SandboxInitializer,
	syncStatusFromPod func(pod *corev1.Pod, newStatus *agentsv1alpha1.SandboxStatus, syncReadyCondition bool),
	resumeFunc ResumeFunc,
) *UpgradeControl {
	return &UpgradeControl{
		Client:            cli,
		checkpointControl: checkpointControl,
		podControl:        podControl,
		recorder:          recorder,
		lifecycleHookFunc: lifecycleHookFunc,
		initializer:       initializer,
		syncStatusFromPod: syncStatusFromPod,
		resumeFunc:        resumeFunc,
	}
}

// recordUpgradeEvent emits an event on the sandbox object. It is safe to call
// when the recorder is nil (e.g. in unit tests that do not assert events).
func (r *UpgradeControl) recordUpgradeEvent(box *agentsv1alpha1.Sandbox, eventType, reason, messageFmt string, args ...any) {
	if r.recorder == nil {
		return
	}
	r.recorder.Eventf(box, eventType, reason, messageFmt, args...)
}

// RequiresPodReplacementUpgrade returns true when the sandbox's upgrade policy
// requires pod replacement (Recreate or CheckpointRestore). These policies enter
// the full upgrade lifecycle (PreUpgrade → Checkpointing → UpgradePod → PostUpgrade).
func RequiresPodReplacementUpgrade(box *agentsv1alpha1.Sandbox) bool {
	return box.Spec.UpgradePolicy != nil &&
		(box.Spec.UpgradePolicy.Type == agentsv1alpha1.SandboxUpgradePolicyRecreate ||
			box.Spec.UpgradePolicy.Type == agentsv1alpha1.SandboxUpgradePolicyCheckpointRestore)
}

// EnsureSandboxUpgraded drives the sandbox upgrade state machine.
//
// The state transitions are:
//
//	Resuming → ResumeSucceed → PreUpgrade → Checkpointing → UpgradePod → PostUpgrade → Succeeded
//
// Each reconcile cycle processes exactly one state and returns nil so the
// controller can persist the updated condition before re-entering the next
// state. Failed states (PreUpgradeFailed, CheckpointFailed, UpgradePodFailed,
// PostUpgradeFailed) are retried on subsequent reconciles.
func (r *UpgradeControl) EnsureSandboxUpgraded(ctx context.Context, args EnsureFuncArgs) (retErr error) {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	isCheckpointRestore := box.Spec.UpgradePolicy != nil &&
		box.Spec.UpgradePolicy.Type == agentsv1alpha1.SandboxUpgradePolicyCheckpointRestore

	// Set Ready=False during upgrade
	utils.SetSandboxCondition(newStatus, metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxConditionReady),
		Status:             metav1.ConditionFalse,
		Reason:             agentsv1alpha1.SandboxReadyReasonUpgrading,
		Message:            "sandbox is upgrading",
		LastTransitionTime: metav1.Now(),
	})
	upgradeCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
	// First entry — if the sandbox was paused, start with Resuming to ensure it is
	// woken up before proceeding with the upgrade lifecycle. Otherwise start at
	// PreUpgrade directly.
	if upgradeCond == nil {
		initialReason := agentsv1alpha1.SandboxUpgradingReasonPreUpgrade
		if pausedCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused)); pausedCond != nil {
			initialReason = agentsv1alpha1.SandboxUpgradingReasonResuming
		}
		upgradeCond = &metav1.Condition{
			Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
			Status:             metav1.ConditionFalse,
			Reason:             initialReason,
			LastTransitionTime: metav1.Now(),
		}
		utils.SetSandboxCondition(newStatus, *upgradeCond)
	}

	switch upgradeCond.Reason {
	// When upgrading a paused Sandbox, it must first be resumed successfully,
	// then upgraded, and finally paused again.
	// Since spec.paused=true at this point, two scenarios are possible during
	// the upgrade:
	// 1. spec.paused remains true throughout the upgrade. After the upgrade
	//    succeeds, the Sandbox enters Running state, and since spec.paused=true,
	//    the pause logic is triggered.
	// 2. If the user triggers a resume (spec.paused=false) during the upgrade,
	//    the Sandbox enters Running state with spec.paused=false after the
	//    upgrade, so no pause logic is triggered and it stays Running.
	//
	// Could this conflict with pauseTime? If PauseTime is set, will pause be
	// triggered again during the upgrade?
	// No. pauseTime takes effect by the sandbox controller setting spec.paused=true.
	// However, the current logic never changes spec.paused during the upgrade flow.
	// The Sandbox is in the Upgrading phase — the resume logic is implemented
	// inline here, without transitioning to Resuming/Running. Therefore, even if
	// pauseTime fires and sets spec.paused=true, it does not matter: the pause
	// will only be re-triggered after the upgrade succeeds and the Sandbox
	// enters Running state.
	case agentsv1alpha1.SandboxUpgradingReasonResuming:
		return r.handleResuming(ctx, args, upgradeCond, newStatus)
	// ResumeSucceed is a transient waiting state. When the template is
	// patched, calculateStatus detects the hash change and calls
	// determineUpgradeResumeReason to transition to PreUpgrade.
	//
	// Abandonment: if the resume trigger annotation has been removed (e.g.,
	// ops was deleted during the phase-1→phase-2 window) and the template
	// was not patched (UpdateRevision unchanged), abandon the upgrade and
	// return to Running. The sandbox already resumed successfully — it just
	// never received the template patch. With no ops to drive phase 2,
	// staying in ResumeSucceed would block forever.
	case agentsv1alpha1.SandboxUpgradingReasonResumeSucceed:
		if box.Annotations[agentsv1alpha1.AnnotationUpgradeResumeTrigger] != agentsv1alpha1.True &&
			newStatus.UpdateRevision == box.Status.UpdateRevision {
			klog.InfoS("Resume trigger annotation removed before template patch, abandoning upgrade", "sandbox", klog.KObj(box))
			utils.RemoveSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
			newStatus.Phase = agentsv1alpha1.SandboxRunning
			return nil
		}
		klog.InfoS("Waiting for template patch after resume", "sandbox", klog.KObj(box))
		return nil
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
				r.recordUpgradeEvent(box, corev1.EventTypeWarning, EventUpgradePreUpgradeFailed, "PreUpgrade failed: %s", result.Message)
				return nil
			}
		}

		// Always transition to Checkpointing; EnsureCheckpointForUpgrade will
		// short-circuit and return done=true if CheckpointRestore is not enabled.
		klog.InfoS("preUpgrade completed, transitioning to Checkpointing", "sandbox", klog.KObj(box))
		upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonCheckpointing
		upgradeCond.Message = ""
		utils.SetSandboxCondition(newStatus, *upgradeCond)
		fallthrough
	case agentsv1alpha1.SandboxUpgradingReasonCheckpointing, agentsv1alpha1.SandboxUpgradingReasonCheckpointFailed:
		checkpointDone, cpName, err := r.checkpointControl.EnsureCheckpointForUpgrade(ctx, box)
		if err != nil {
			klog.ErrorS(err, "Checkpoint failed during upgrade", "sandbox", klog.KObj(box), "checkpoint", cpName)
			upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonCheckpointFailed
			upgradeCond.Message = err.Error()
			utils.SetSandboxCondition(newStatus, *upgradeCond)
			return err
		}
		if !checkpointDone {
			klog.InfoS("Waiting for checkpoint to complete", "sandbox", klog.KObj(box), "checkpoint", cpName)
			upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonCheckpointing
			upgradeCond.Message = fmt.Sprintf("Waiting for checkpoint %s to complete before pod deletion", cpName)
			utils.SetSandboxCondition(newStatus, *upgradeCond)
			return nil
		}
		klog.InfoS("Checkpoint succeeded, transitioning to UpgradePod", "sandbox", klog.KObj(box))
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
		r.recordUpgradeEvent(box, corev1.EventTypeNormal, EventUpgradePodReplaced, "Pod replaced successfully, proceeding to PostUpgrade")

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
			// Use a copy of box with the latest status so that GetRuntimeURL
			// resolves the correct PodIP for the PostUpgrade hook.
			boxForHook := box.DeepCopy()
			boxForHook.Status = *newStatus
			result := r.executeUpgradeAction(ctx, pod, boxForHook, action)
			klog.InfoS("postUpgrade result", "sandbox", klog.KObj(box), "succeeded", result.Succeeded, "message", result.Message)
			if !result.Succeeded {
				// Record postUpgrade action failed
				upgradeCond.Message = result.Message
				upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonPostUpgradeFailed
				utils.SetSandboxCondition(newStatus, *upgradeCond)
				r.recordUpgradeEvent(box, corev1.EventTypeWarning, EventUpgradePostUpgradeFailed, "PostUpgrade failed: %s", result.Message)
				return nil
			}
		}

		// For CheckpointRestore, clean up checkpoint CRs after the entire upgrade succeeds.
		if isCheckpointRestore {
			r.checkpointControl.CleanupForUpgrade(ctx, box)
		}

		klog.InfoS("postUpgrade completed, transitioning to Succeeded", "sandbox", klog.KObj(box))
		r.recordUpgradeEvent(box, corev1.EventTypeNormal, EventUpgradeSucceeded, "Upgrade completed successfully")
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

// handleResuming processes the Resuming state of the upgrade lifecycle.
// It resumes the paused sandbox, waits for pod readiness, and transitions
// to ResumeSucceed when the resume is complete.
// Returns nil while waiting and an error if resume or initialize fails.
func (r *UpgradeControl) handleResuming(ctx context.Context, args EnsureFuncArgs, upgradeCond *metav1.Condition, newStatus *agentsv1alpha1.SandboxStatus) error {
	pod, box := args.Pod, args.Box
	// The sandbox was paused — resume it before proceeding with the upgrade.
	pausedCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
	if pausedCond == nil || pausedCond.Status == metav1.ConditionFalse {
		// Sandbox is still pausing (or pause condition missing), wait for it to complete.
		klog.InfoS("Sandbox is still pausing, waiting before upgrade", "sandbox", klog.KObj(box))
		return nil
	}
	// Record the resume event only on the first entry into Resuming;
	// subsequent reconciles see a non-empty Message and skip the
	// duplicate event while waiting for pod readiness.
	if upgradeCond.Message == "" {
		r.recordUpgradeEvent(box, corev1.EventTypeNormal, EventUpgradeResuming, "Resuming paused sandbox for upgrade")
		upgradeCond.Message = "Resume triggered, waiting for pod readiness"
		utils.SetSandboxCondition(newStatus, *upgradeCond)
	}
	if err := r.resumeFunc(ctx, args); err != nil {
		return err
	}
	// Check if resume succeeded by looking at the Resumed condition.
	resumedCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionResumed))
	if resumedCond == nil || resumedCond.Status != metav1.ConditionTrue {
		klog.InfoS("Sandbox resume in progress, waiting before upgrade", "sandbox", klog.KObj(box))
		return nil
	}
	// pod may be nil here if resumeFunc just created it but the local
	// args.Pod copy is stale. Re-fetch is not needed — the controller will
	// re-reconcile with the updated pod. Just wait for the next cycle.
	if pod == nil {
		klog.InfoS("Pod not yet available after resume, waiting for next reconcile", "sandbox", klog.KObj(box))
		return nil
	}
	pCond := utils.GetPodCondition(&pod.Status, corev1.PodReady)
	if pCond == nil || pCond.Status != corev1.ConditionTrue {
		klog.InfoS("Waiting for pod ready before initialization", "sandbox", klog.KObj(box))
		return nil
	}
	// Only initialize the old pod when a PreUpgrade hook is configured.
	// The Initialize call (runtime re-init, security token, CSI re-mount)
	// prepares the old pod for PreUpgrade execution. If no PreUpgrade hook
	// is configured, the old pod is about to be deleted in the UpgradePod
	// step, so initializing it would be wasted work. The new pod created
	// in performRecreateUpgrade is always initialized regardless.
	if hasUpgradeAction(box, true) {
		if err := r.initializer.Initialize(ctx, box, newStatus); err != nil {
			return err
		}
	}
	r.syncStatusFromPod(pod, newStatus, false)
	// Resume succeeded. Transition to ResumeSucceed and wait for
	// SandboxUpdateOps to patch the template before proceeding.
	klog.InfoS("Sandbox resumed successfully, waiting for template patch", "sandbox", klog.KObj(box))
	r.recordUpgradeEvent(box, corev1.EventTypeNormal, EventUpgradeResumed, "Sandbox resumed, waiting for template patch")
	upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonResumeSucceed
	upgradeCond.Message = ""
	utils.SetSandboxCondition(newStatus, *upgradeCond)
	utils.RemoveSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
	return nil
}

// performRecreateUpgrade handles the Recreate upgrade step (delete old pod + create new pod).
// For CheckpointRestore policy, it restores the PodTemplateDelta when creating the new pod.
func (r *UpgradeControl) performRecreateUpgrade(ctx context.Context, args EnsureFuncArgs) (bool, error) {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus

	isCheckpointRestore := box.Spec.UpgradePolicy != nil &&
		box.Spec.UpgradePolicy.Type == agentsv1alpha1.SandboxUpgradePolicyCheckpointRestore

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
		// TODO: The virtual kubelet may have status reporting delays after
		// pod deletion. Previously a blocking time.Sleep(1s) was used here
		// to mitigate this. It is removed to avoid blocking the reconcile
		// loop. If VK status issues resurface, consider adding a requeue
		// delay or an annotation-based gate before pod creation.
		klog.InfoS("Creating new pod for upgrade", "sandbox", klog.KObj(box))
		// Both Recreate and CheckpointRestore render the new pod from the current
		// template, so the runtime HTTPS capability of the new pod matches the
		// current injection configuration and the stamp is accurate.
		createArgs := CreatePodArgs{Box: box, NewStatus: newStatus, AdvertiseRuntimeTLS: true}
		// For CheckpointRestore, set the checkpoint ID annotation so the
		// checkpoint controller can restore the pod's writable layer.
		if isCheckpointRestore {
			createArgs.CheckpointID = r.checkpointControl.GetCheckpointIDForUpgrade(ctx, box)
		}
		newPod, err := r.podControl.CreatePod(ctx, createArgs)
		if err != nil {
			klog.ErrorS(err, "Failed to create new pod for upgrade", "sandbox", klog.KObj(box))
			return false, err
		}
		if newPod != nil {
			klog.InfoS("New pod created for upgrade", "sandbox", klog.KObj(box), "pod", klog.KObj(newPod))
		}
		return false, nil
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
				r.recordUpgradeEvent(box, corev1.EventTypeWarning, EventUpgradePodFailed,
					"Container %s waiting: %s - %s", cStatus.Name, reason, cStatus.State.Waiting.Message)
			} else if cStatus.State.Terminated != nil {
				klog.InfoS("container terminated unexpectedly", "sandbox", klog.KObj(box),
					"container", cStatus.Name, "reason", cStatus.State.Terminated.Reason,
					"exitCode", cStatus.State.Terminated.ExitCode, "message", cStatus.State.Terminated.Message)
				cond.Reason = agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed
				cond.Message = fmt.Sprintf("container %s: terminated with exit code %d - %s",
					cStatus.Name, cStatus.State.Terminated.ExitCode, cStatus.State.Terminated.Reason)
				utils.SetSandboxCondition(newStatus, *cond)
				r.recordUpgradeEvent(box, corev1.EventTypeWarning, EventUpgradePodFailed,
					"Container %s terminated with exit code %d - %s",
					cStatus.Name, cStatus.State.Terminated.ExitCode, cStatus.State.Terminated.Reason)
			}
		}
		return false, nil
	}

	r.syncStatusFromPod(pod, newStatus, false)

	// Step 4: Perform post-recreate-upgrade initialization (re-init runtime, re-mount CSI).
	if err := r.initializer.Initialize(ctx, box, newStatus); err != nil {
		return false, err
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
func (r *UpgradeControl) executeUpgradeAction(ctx context.Context, pod *corev1.Pod, box *agentsv1alpha1.Sandbox, action *agentsv1alpha1.UpgradeAction) upgradeActionResult {
	if action == nil {
		return upgradeActionResult{Succeeded: true, Message: "no hook configured, skipped"}
	}
	if pod == nil {
		return upgradeActionResult{Succeeded: false, Message: "pod not found, cannot execute hook"}
	}

	exitCode, stdout, stderr, err := r.lifecycleHookFunc(ctx, box, action)
	if err != nil {
		msg := fmt.Sprintf("hook execution error: %v, stderr: %s, stdout: %s", err, stderr, stdout)
		return upgradeActionResult{Succeeded: false, Message: utils.TruncateConditionMessage(msg)}
	}
	if exitCode != 0 {
		msg := fmt.Sprintf("hook failed with exit code %d, stderr: %s, stdout: %s", exitCode, stderr, stdout)
		return upgradeActionResult{Succeeded: false, Message: utils.TruncateConditionMessage(msg)}
	}
	return upgradeActionResult{Succeeded: true, Message: fmt.Sprintf("hook succeeded, stdout: %s", stdout)}
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
