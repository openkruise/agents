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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
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
	EventUpgradeInstalling        = "UpgradeInstalling"
	EventUpgradeInstallingFailed  = "UpgradeInstallingFailed"
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

// RequiresInstallerUpgrade returns true when the sandbox's upgrade policy is
// InstallerUpdate, which injects an ephemeral installer container into the
// running Pod instead of replacing it. These upgrades enter the lifecycle
// chain PreUpgrade → Installing → PostUpgrade. It requires the
// SandboxInstallerUpdate feature gate: when the gate is disabled the policy is
// inert, so the sandbox is treated as having no upgrade policy at all.
func RequiresInstallerUpgrade(box *agentsv1alpha1.Sandbox) bool {
	return box.Spec.UpgradePolicy != nil &&
		box.Spec.UpgradePolicy.Type == agentsv1alpha1.SandboxUpgradePolicyInstallerUpdate &&
		utilfeature.DefaultFeatureGate.Enabled(features.SandboxInstallerUpdateGate)
}

// EnsureSandboxUpgraded keeps installer failures terminal while replacement stages can fall through and retry.
func (r *UpgradeControl) EnsureSandboxUpgraded(ctx context.Context, args EnsureFuncArgs) (retErr error) {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	logger := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
	isCheckpointRestore := box.Spec.UpgradePolicy != nil &&
		box.Spec.UpgradePolicy.Type == agentsv1alpha1.SandboxUpgradePolicyCheckpointRestore
	isInstallerUpdate := RequiresInstallerUpgrade(box)

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
		upgradeCond = utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
	}

	if isInstallerUpdate && upgradeCond.Status == metav1.ConditionFalse {
		switch upgradeCond.Reason {
		case agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed,
			agentsv1alpha1.SandboxUpgradingReasonInstallingFailed,
			agentsv1alpha1.SandboxUpgradingReasonPostUpgradeFailed:
			// A timed-out installer can exit successfully later without making the failed attempt safe to accept.
			return nil
		}
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
		return r.handleResuming(ctx, args, upgradeCond)
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
		// InstallerUpdate has no phase-2 template patch to wait for: the full
		// policy was written before the resume started. Abandon only when the
		// install for the current patch version already completed; otherwise
		// proceed straight to PreUpgrade.
		if isInstallerUpdate {
			if box.Spec.UpgradePolicy.PatchVersion == box.Status.CompletedPatchVersion {
				logger.Info("Install already completed, abandoning upgrade after resume")
				utils.RemoveSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
				newStatus.Phase = agentsv1alpha1.SandboxRunning
				return nil
			}
			logger.Info("Resume completed for installer upgrade, proceeding to PreUpgrade")
			upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonPreUpgrade
			upgradeCond.Message = ""
			utils.SetSandboxCondition(newStatus, *upgradeCond)
			return nil
		}
		if box.Annotations[agentsv1alpha1.AnnotationUpgradeResumeTrigger] != agentsv1alpha1.True &&
			newStatus.UpdateRevision == box.Status.UpdateRevision {
			logger.Info("Resume trigger annotation removed before template patch, abandoning upgrade")
			utils.RemoveSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
			newStatus.Phase = agentsv1alpha1.SandboxRunning
			return nil
		}
		logger.Info("Waiting for template patch after resume")
		return nil
	case agentsv1alpha1.SandboxUpgradingReasonInstalling:
		// The installer container performs the file mutation; every non-zero
		// outcome is terminal for this patch version. Re-evaluation is
		// idempotent: once injected, the container name is a deterministic
		// function of the patch version, so no second container is ever added.
		done, err := r.performInstallerUpgrade(ctx, args, upgradeCond)
		if err != nil {
			logger.Error(err, "Installing step failed")
			return err
		}
		if !done {
			return nil // install in progress or failed
		}

		logger.Info("Installing step completed, transitioning to PostUpgrade")

		// Record completion so the same patch version is never re-run and the
		// upgrade trigger does not re-fire before SandboxUpdateOps clears the policy.
		newStatus.CompletedPatchVersion = box.Spec.UpgradePolicy.PatchVersion
		upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonPostUpgrade
		upgradeCond.Message = ""
		utils.SetSandboxCondition(newStatus, *upgradeCond)
		return nil
	case agentsv1alpha1.SandboxUpgradingReasonPreUpgrade, agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed:
		// Execute preUpgrade if configured
		if hasUpgradeAction(box, true) {
			var action *agentsv1alpha1.UpgradeAction
			if box.Spec.Lifecycle != nil {
				action = box.Spec.Lifecycle.PreUpgrade
			}
			result := r.executeUpgradeAction(ctx, pod, box, action)
			logger.Info("preUpgrade result", "succeeded", result.Succeeded, "message", result.Message)
			if !result.Succeeded {
				// Record preUpgrade action failed
				upgradeCond.Message = result.Message
				upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed
				utils.SetSandboxCondition(newStatus, *upgradeCond)
				r.recordUpgradeEvent(box, corev1.EventTypeWarning, EventUpgradePreUpgradeFailed, "PreUpgrade failed: %s", result.Message)
				return nil
			}
		}

		// InstallerUpdate skips checkpointing and pod replacement entirely: the
		// install is driven by the injected ephemeral container. The condition's
		// LastTransitionTime is refreshed here because it anchors the install
		// timeout, and SetSandboxCondition does not refresh it on a
		// reason-only change.
		if isInstallerUpdate {
			logger.Info("preUpgrade completed, transitioning to Installing")
			upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonInstalling
			upgradeCond.Message = ""
			upgradeCond.LastTransitionTime = metav1.Now()
			utils.SetSandboxCondition(newStatus, *upgradeCond)
			return nil
		}

		// Always transition to Checkpointing; EnsureCheckpointForUpgrade will
		// short-circuit and return done=true if CheckpointRestore is not enabled.
		logger.Info("preUpgrade completed, transitioning to Checkpointing")
		upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonCheckpointing
		upgradeCond.Message = ""
		utils.SetSandboxCondition(newStatus, *upgradeCond)
		fallthrough
	case agentsv1alpha1.SandboxUpgradingReasonCheckpointing, agentsv1alpha1.SandboxUpgradingReasonCheckpointFailed:
		checkpointDone, cpName, err := r.checkpointControl.EnsureCheckpointForUpgrade(ctx, box)
		if err != nil {
			logger.Error(err, "Checkpoint failed during upgrade", "checkpoint", cpName)
			upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonCheckpointFailed
			upgradeCond.Message = err.Error()
			utils.SetSandboxCondition(newStatus, *upgradeCond)
			return err
		}
		if !checkpointDone {
			logger.Info("Waiting for checkpoint to complete", "checkpoint", cpName)
			upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonCheckpointing
			upgradeCond.Message = fmt.Sprintf("Waiting for checkpoint %s to complete before pod deletion", cpName)
			utils.SetSandboxCondition(newStatus, *upgradeCond)
			return nil
		}
		logger.Info("Checkpoint succeeded, transitioning to UpgradePod")
		upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonUpgradePod
		upgradeCond.Message = ""
		utils.SetSandboxCondition(newStatus, *upgradeCond)
		fallthrough
	case agentsv1alpha1.SandboxUpgradingReasonUpgradePod, agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed:
		done, err := r.performRecreateUpgrade(ctx, args)
		if err != nil {
			logger.Error(err, "UpgradePod step failed")
			return err
		} else if !done {
			logger.Info("UpgradePod step in progress")
			return nil // upgrade in progress
		}

		logger.Info("UpgradePod step completed, transitioning to PostUpgrade")
		r.recordUpgradeEvent(box, corev1.EventTypeNormal, EventUpgradePodReplaced, "Pod replaced successfully, proceeding to PostUpgrade")

		// Re-fetch the Pod after recreate upgrade, since the old pod object is stale (deleted and replaced).
		var freshPod corev1.Pod
		if err := r.Get(ctx, types.NamespacedName{Namespace: box.Namespace, Name: box.Name}, &freshPod); err != nil {
			logger.Error(err, "Failed to re-fetch pod after recreate upgrade")
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
			logger.Info("postUpgrade result", "succeeded", result.Succeeded, "message", result.Message)
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
			r.checkpointControl.CleanupCheckpoints(ctx, box)
		}

		logger.Info("postUpgrade completed, transitioning to Succeeded")
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
func (r *UpgradeControl) handleResuming(ctx context.Context, args EnsureFuncArgs, upgradeCond *metav1.Condition) error {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	logger := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
	// The sandbox was paused — resume it before proceeding with the upgrade.
	pausedCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
	if pausedCond == nil || pausedCond.Status == metav1.ConditionFalse {
		// Sandbox is still pausing (or pause condition missing), wait for it to complete.
		logger.Info("Sandbox is still pausing, waiting before upgrade")
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
		logger.Info("Sandbox resume in progress, waiting before upgrade")
		return nil
	}
	// pod may be nil here if resumeFunc just created it but the local
	// args.Pod copy is stale. Re-fetch is not needed — the controller will
	// re-reconcile with the updated pod. Just wait for the next cycle.
	if pod == nil {
		logger.Info("Pod not yet available after resume, waiting for next reconcile")
		return nil
	}
	pCond := utils.GetPodCondition(&pod.Status, corev1.PodReady)
	if pCond == nil || pCond.Status != corev1.ConditionTrue {
		logger.Info("Waiting for pod ready before initialization")
		return nil
	}
	// Sync status from the resumed pod before Initialize so that runtime
	// re-init targets the current Pod IP/UID. The resumed pod may differ
	// from the paused one, and using stale status would leave the upgrade
	// stuck in Resuming.
	r.syncStatusFromPod(pod, newStatus, false)
	// Installer PostUpgrade hooks use the resumed Pod rather than an initialized replacement.
	isInstallerUpdate := box.Spec.UpgradePolicy != nil &&
		box.Spec.UpgradePolicy.Type == agentsv1alpha1.SandboxUpgradePolicyInstallerUpdate
	if hasUpgradeAction(box, true) || (isInstallerUpdate && hasUpgradeAction(box, false)) {
		if err := r.initializer.Initialize(ctx, box, newStatus); err != nil {
			return err
		}
	}
	// ResumeSucceed advances installer upgrades directly; replacement upgrades wait for a template patch.
	logger.Info("Sandbox resumed successfully, advancing upgrade lifecycle")
	r.recordUpgradeEvent(box, corev1.EventTypeNormal, EventUpgradeResumed, "Sandbox resumed, advancing upgrade lifecycle")
	upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonResumeSucceed
	upgradeCond.Message = ""
	utils.SetSandboxCondition(newStatus, *upgradeCond)
	// Resume succeeded: drop the Paused condition, kept through Resuming.
	// The direct resume path cleans it up in the controller's
	// finalizeResumePhase; the upgrade path calls resumeFunc directly, so it
	// cleans up here.
	utils.RemoveSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
	return nil
}

// performRecreateUpgrade handles the Recreate upgrade step (delete old pod + create new pod).
// For CheckpointRestore policy, it passes the checkpoint ID so the new pod
// restores its writable layer from the checkpoint.
func (r *UpgradeControl) performRecreateUpgrade(ctx context.Context, args EnsureFuncArgs) (bool, error) {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	logger := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(box))

	isCheckpointRestore := box.Spec.UpgradePolicy != nil &&
		box.Spec.UpgradePolicy.Type == agentsv1alpha1.SandboxUpgradePolicyCheckpointRestore

	// Step 1: Delete old Pod (has old template hash)
	if pod != nil && pod.Labels[agentsv1alpha1.PodLabelTemplateHash] != newStatus.UpdateRevision {
		if !pod.DeletionTimestamp.IsZero() {
			logger.Info("Waiting for pod deletion to complete")
			return false, nil
		}
		// Delete pod
		ScaleExpectation.ExpectScale(GetControllerKey(box), expectations.Delete, box.Name)
		if err := r.Delete(ctx, pod); err != nil {
			ScaleExpectation.ObserveScale(GetControllerKey(box), expectations.Delete, box.Name)
			if !errors.IsNotFound(err) {
				logger.Error(err, "Failed to delete pod for upgrade")
				return false, err
			}
		}
		logger.Info("Deleted old pod for upgrade")
		return false, nil
	}

	// Step 2: Create new Pod (old pod deleted)
	if pod == nil {
		// TODO: The virtual kubelet may have status reporting delays after
		// pod deletion. Previously a blocking time.Sleep(1s) was used here
		// to mitigate this. It is removed to avoid blocking the reconcile
		// loop. If VK status issues resurface, consider adding a requeue
		// delay or an annotation-based gate before pod creation.
		logger.Info("Creating new pod for upgrade")
		// Both Recreate and CheckpointRestore render the new pod from the current
		// template, so the runtime HTTPS capability of the new pod matches the
		// current injection configuration and the stamp is accurate.
		createArgs := CreatePodArgs{Box: box, NewStatus: newStatus, AdvertiseRuntimeTLS: true}
		// For CheckpointRestore, set the checkpoint ID annotation so the
		// checkpoint controller can restore the pod's writable layer.
		if isCheckpointRestore {
			_, checkpointID := r.checkpointControl.GetCheckpointResumeData(ctx, box)
			if checkpointID == "" {
				// Should never happen: the checkpoint succeeded before the
				// upgrade reached this step, so it must carry an ID. Block
				// the upgrade and surface the inconsistency instead of
				// creating a pod that cannot restore its writable layer.
				err := fmt.Errorf("checkpoint ID not found for CheckpointRestore upgrade")
				logger.Error(err, "CheckpointRestore upgrade blocked: checkpoint has no ID")
				return false, err
			}
			createArgs.CheckpointID = checkpointID
		}
		newPod, err := r.podControl.CreatePod(ctx, createArgs)
		if err != nil {
			logger.Error(err, "Failed to create new pod for upgrade")
			return false, err
		}
		if newPod != nil {
			logger.Info("New pod created for upgrade", "pod", klog.KObj(newPod))
		}
		return false, nil
	}

	// Step 3: Wait for new Pod to be running and ready
	pCond := utils.GetPodCondition(&pod.Status, corev1.PodReady)
	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
	if pCond == nil || pCond.Status != corev1.ConditionTrue {
		logger.Info("Waiting for new pod to be ready")
		for _, cStatus := range pod.Status.ContainerStatuses {
			if cStatus.State.Waiting != nil {
				reason := cStatus.State.Waiting.Reason
				// PodInitializing and ContainerCreating are normal transient states during startup, skip them
				if reason == WaitingReasonPodInitializing || reason == WaitingReasonContainerCreating {
					continue
				}
				// Other waiting reasons indicate container startup failure
				logger.Info("container waiting with abnormal reason",
					"container", cStatus.Name, "reason", reason, "message", cStatus.State.Waiting.Message)
				cond.Reason = agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed
				cond.Message = fmt.Sprintf("container %s: %s - %s", cStatus.Name, reason, cStatus.State.Waiting.Message)
				utils.SetSandboxCondition(newStatus, *cond)
				r.recordUpgradeEvent(box, corev1.EventTypeWarning, EventUpgradePodFailed,
					"Container %s waiting: %s - %s", cStatus.Name, reason, cStatus.State.Waiting.Message)
			} else if cStatus.State.Terminated != nil {
				logger.Info("container terminated unexpectedly",
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

// Reserved installer exit codes, part of the installer contract:
// 125 means the container runtime does not implement namespace targeting;
// 126 means the installer baked an absolute /proc/1/root path into the target.
const (
	installerExitCodeNoNamespaceTargeting = 125
	installerExitCodeNonRelocatableResult = 126
)

// defaultInstallerTimeoutSeconds bounds an install when the operator did not
// specify installer.timeoutSeconds.
const defaultInstallerTimeoutSeconds = 600

// InstallerUpgradeDeadline keeps timeout evaluation and scheduled reconciliation on the same fixed deadline.
func InstallerUpgradeDeadline(box *agentsv1alpha1.Sandbox, status *agentsv1alpha1.SandboxStatus) time.Time {
	policy := box.Spec.UpgradePolicy
	if status.Phase != agentsv1alpha1.SandboxUpgrading || policy == nil ||
		policy.Type != agentsv1alpha1.SandboxUpgradePolicyInstallerUpdate || policy.Installer == nil {
		return time.Time{}
	}
	cond := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionUpgrading))
	if cond == nil || cond.Reason != agentsv1alpha1.SandboxUpgradingReasonInstalling ||
		cond.Status != metav1.ConditionFalse || cond.LastTransitionTime.IsZero() {
		return time.Time{}
	}
	timeoutSeconds := int32(defaultInstallerTimeoutSeconds)
	if policy.Installer.TimeoutSeconds != nil {
		timeoutSeconds = *policy.Installer.TimeoutSeconds
	}
	return cond.LastTransitionTime.Add(time.Duration(timeoutSeconds) * time.Second)
}

// installerTargetRootEnv points the installer command at the target
// container's filesystem through the PID-1 magic symlink.
const installerTargetRootEnv = "/proc/1/root"

// installerContainerName derives the ephemeral container name from the patch
// version. The name is a deterministic function of the patch version, so a
// requeue after a partial write finds the entry already present and skips
// injection — that guard is what bounds the install to one container per
// patch version.
func installerContainerName(patchVersion string) string {
	sum := sha256.Sum256([]byte(patchVersion))
	return fmt.Sprintf("installer-%s", hex.EncodeToString(sum[:4]))
}

// performInstallerUpgrade drives the Installing step of an InstallerUpdate
// upgrade. It resolves the target container, guards PID-namespace ownership,
// injects the installer ephemeral container through the pods/ephemeralcontainers
// subresource at most once per patch version, and evaluates its termination.
// It returns done=true when the installer exited successfully and the upgrade
// may proceed to PostUpgrade.
func (r *UpgradeControl) performInstallerUpgrade(ctx context.Context, args EnsureFuncArgs, upgradeCond *metav1.Condition) (bool, error) {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	logger := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
	policy := box.Spec.UpgradePolicy
	if policy == nil || policy.Installer == nil {
		// Cannot happen for a policy admitted through SandboxUpdateOps; block
		// rather than loop on an unsatisfiable configuration.
		err := fmt.Errorf("installer upgrade policy is missing the installer configuration")
		logger.Error(err, "Installer upgrade blocked")
		r.setInstallingFailed(upgradeCond, newStatus, box, err.Error())
		return false, nil
	}
	installer := policy.Installer

	// The Upgrading phase has no pod-nil terminal detection upstream, so a
	// Pod deleted mid-install (eviction, node failure) reaches this point
	// with pod == nil. The writable layer the install wrote into is gone, so
	// the patch version is terminally failed, not retried.
	if pod == nil {
		r.setInstallingFailed(upgradeCond, newStatus, box,
			"sandbox pod not found during install; the writable layer is gone")
		return false, nil
	}

	// A terminating Pod is still Running during graceful shutdown, but its
	// writable layer is about to disappear and ephemeral containers cannot be
	// added to it. Failing here also avoids burning the full timeoutSeconds
	// on an install that can never complete.
	if !pod.DeletionTimestamp.IsZero() {
		r.setInstallingFailed(upgradeCond, newStatus, box,
			"sandbox pod is terminating; the install cannot complete")
		return false, nil
	}

	// Step 1: resolve the target container. The selector may match sandboxes
	// that did not exist at admission time, so this is resolved here rather
	// than in the webhook.
	targetName := installer.TargetContainerName
	if targetName == "" && box.Spec.Template != nil && len(box.Spec.Template.Spec.Containers) > 0 {
		targetName = box.Spec.Template.Spec.Containers[0].Name
	}
	var target *corev1.Container
	if targetName != "" {
		for i := range pod.Spec.Containers {
			if pod.Spec.Containers[i].Name == targetName {
				target = &pod.Spec.Containers[i]
				break
			}
		}
	}
	if target == nil {
		r.setInstallingFailed(upgradeCond, newStatus, box, fmt.Sprintf(
			"target container %q not found in pod", targetName))
		return false, nil
	}

	// Step 2: the target container must own its PID namespace. Under
	// shareProcessNamespace or hostPID, PID 1 is the pause container or the
	// node's init, so /proc/1/root names the wrong rootfs and the installer's
	// own targeting self-check cannot detect it.
	if pod.Spec.ShareProcessNamespace != nil && *pod.Spec.ShareProcessNamespace {
		r.setInstallingFailed(upgradeCond, newStatus, box,
			"pod sets shareProcessNamespace: true, so PID 1 is not the target container and /proc/1/root names the wrong rootfs")
		return false, nil
	}
	if pod.Spec.HostPID {
		r.setInstallingFailed(upgradeCond, newStatus, box,
			"pod sets hostPID: true, so PID 1 is not the target container and /proc/1/root names the wrong rootfs")
		return false, nil
	}

	timeoutSeconds := int32(defaultInstallerTimeoutSeconds)
	if installer.TimeoutSeconds != nil {
		timeoutSeconds = *installer.TimeoutSeconds
	}
	deadline := InstallerUpgradeDeadline(box, newStatus)
	ecName := installerContainerName(policy.PatchVersion)
	for i := range pod.Spec.EphemeralContainers {
		if pod.Spec.EphemeralContainers[i].Name == ecName {
			return r.evaluateInstallerStatus(ctx, box, pod, upgradeCond, newStatus, ecName, timeoutSeconds, deadline), nil
		}
	}
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		r.setInstallingFailed(upgradeCond, newStatus, box, fmt.Sprintf(
			"installer container %s could not be injected within timeoutSeconds=%d", ecName, timeoutSeconds))
		return false, nil
	}

	// A transient target restart may delay injection, but cannot extend the install deadline.
	if pod.Status.Phase != corev1.PodRunning {
		logger.Info("Waiting for pod running before injecting installer")
		return false, nil
	}
	targetRunning := false
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Name == target.Name && cs.State.Running != nil {
			targetRunning = true
			break
		}
	}
	if !targetRunning {
		logger.Info("Waiting for target container running before injecting installer", "container", target.Name)
		return false, nil
	}

	logger = logger.WithValues("container", ecName)
	ec := buildInstallerEphemeralContainer(ecName, installer, policy.PatchVersion, target, box)
	ecJSON, err := json.Marshal(ec)
	if err != nil {
		return false, err
	}
	patchBody := fmt.Sprintf(`{"spec":{"ephemeralContainers":[%s]}}`, ecJSON)
	if err := r.SubResource("ephemeralcontainers").Patch(ctx, pod,
		client.RawPatch(types.StrategicMergePatchType, []byte(patchBody))); err != nil {
		logger.Error(err, "Failed to inject installer ephemeral container")
		if errors.IsForbidden(err) || errors.IsInvalid(err) {
			r.setInstallingFailed(upgradeCond, newStatus, box, fmt.Sprintf(
				"failed to inject installer container %s: %v", ecName, err))
			return false, nil
		}
		return false, err
	}
	logger.Info("Injected installer ephemeral container", "image", installer.Image)
	r.recordUpgradeEvent(box, corev1.EventTypeNormal, EventUpgradeInstalling,
		"Installer container %s injected from image %s", ecName, installer.Image)
	return false, nil
}

// evaluateInstallerStatus maps the injected installer container's status to an
// upgrade outcome. Every non-zero exit is terminal for this patch version: an
// ephemeral container cannot be removed, restarted or replaced, so a second
// injection could not undo the first.
func (r *UpgradeControl) evaluateInstallerStatus(
	ctx context.Context,
	box *agentsv1alpha1.Sandbox,
	pod *corev1.Pod,
	upgradeCond *metav1.Condition,
	newStatus *agentsv1alpha1.SandboxStatus,
	ecName string,
	timeoutSeconds int32,
	deadline time.Time,
) bool {
	logger := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(box), "container", ecName)
	var ecStatus *corev1.ContainerStatus
	for i := range pod.Status.EphemeralContainerStatuses {
		if pod.Status.EphemeralContainerStatuses[i].Name == ecName {
			ecStatus = &pod.Status.EphemeralContainerStatuses[i]
			break
		}
	}

	deadlineExceeded := func() bool {
		return !deadline.IsZero() && !time.Now().Before(deadline)
	}

	if ecStatus == nil || ecStatus.State.Waiting != nil {
		if ecStatus != nil {
			switch ecStatus.State.Waiting.Reason {
			case "ImagePullBackOff", "ErrImagePull":
				// The kubelet retries the pull on its own backoff; bounded by
				// the same timeout deadline.
				logger.Info("Installer image pull in progress", "reason", ecStatus.State.Waiting.Reason)
			default:
				logger.Info("Installer container waiting",
					"reason", ecStatus.State.Waiting.Reason, "message", ecStatus.State.Waiting.Message)
			}
		}
		if deadlineExceeded() {
			r.setInstallingFailed(upgradeCond, newStatus, box, fmt.Sprintf(
				"installer container %s did not start within timeoutSeconds=%d; the ephemeral container cannot be terminated and keeps running until the Pod dies",
				ecName, timeoutSeconds))
			return false
		}
		return false
	}

	if term := ecStatus.State.Terminated; term != nil {
		// A delayed reconcile may observe a timely completion after the deadline has elapsed.
		if !deadline.IsZero() && term.FinishedAt.After(deadline) {
			r.setInstallingFailed(upgradeCond, newStatus, box, fmt.Sprintf(
				"installer container %s finished after timeoutSeconds=%d", ecName, timeoutSeconds))
			return false
		}
		switch term.ExitCode {
		case 0:
			return true
		case installerExitCodeNoNamespaceTargeting:
			r.setInstallingFailed(upgradeCond, newStatus, box,
				"container runtime does not support namespace targeting (installer exited 125)")
			return false
		case installerExitCodeNonRelocatableResult:
			r.setInstallingFailed(upgradeCond, newStatus, box,
				"installer baked an absolute /proc/1/root path into the target (installer exited 126)")
			return false
		default:
			msg := fmt.Sprintf("installer container %s exited with code %d", ecName, term.ExitCode)
			if term.Message != "" {
				msg += ": " + term.Message
			}
			r.setInstallingFailed(upgradeCond, newStatus, box, msg)
			return false
		}
	}

	// Still running.
	if deadlineExceeded() {
		r.setInstallingFailed(upgradeCond, newStatus, box, fmt.Sprintf(
			"installer container %s exceeded timeoutSeconds=%d and was marked failed; the ephemeral container cannot be terminated and keeps running until the Pod dies",
			ecName, timeoutSeconds))
		return false
	}
	logger.Info("Installer container running, waiting for termination")
	return false
}

// setInstallingFailed records the InstallingFailed terminal condition and a
// warning event.
func (r *UpgradeControl) setInstallingFailed(upgradeCond *metav1.Condition, newStatus *agentsv1alpha1.SandboxStatus, box *agentsv1alpha1.Sandbox, message string) {
	upgradeCond.Reason = agentsv1alpha1.SandboxUpgradingReasonInstallingFailed
	upgradeCond.Message = utils.TruncateConditionMessage(message)
	utils.SetSandboxCondition(newStatus, *upgradeCond)
	r.recordUpgradeEvent(box, corev1.EventTypeWarning, EventUpgradeInstallingFailed, "Installer failed: %s", message)
}

// buildInstallerEphemeralContainer assembles the ephemeral container for an
// install, including the SANDBOX_INSTALLER_* contract environment and the
// default SYS_PTRACE capability.
func buildInstallerEphemeralContainer(
	name string,
	installer *agentsv1alpha1.InstallerSpec,
	patchVersion string,
	target *corev1.Container,
	box *agentsv1alpha1.Sandbox,
) corev1.EphemeralContainer {
	env := []corev1.EnvVar{
		{Name: "SANDBOX_INSTALLER_TARGET_ROOT", Value: installerTargetRootEnv},
		{Name: "SANDBOX_INSTALLER_TARGET_CONTAINER", Value: target.Name},
		{Name: "SANDBOX_INSTALLER_PATCH_VERSION", Value: patchVersion},
		{Name: "SANDBOX_NAME", Value: box.Name},
		{Name: "SANDBOX_NAMESPACE", Value: box.Namespace},
		{Name: "SANDBOX_UID", Value: string(box.UID)},
	}
	env = append(env, installer.Env...)

	// securityContext is a full override: unset means the controller defaults
	// to SYS_PTRACE (needed to read /proc/1/root) and inherits the target's
	// runAsUser when set.
	securityContext := installer.SecurityContext
	if securityContext == nil {
		securityContext = &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{"SYS_PTRACE"},
			},
		}
		if target.SecurityContext != nil && target.SecurityContext.RunAsUser != nil {
			securityContext.RunAsUser = target.SecurityContext.RunAsUser
		}
	}

	imagePullPolicy := installer.ImagePullPolicy
	if imagePullPolicy == "" {
		imagePullPolicy = corev1.PullIfNotPresent
	}

	return corev1.EphemeralContainer{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:                     name,
			Image:                    installer.Image,
			Command:                  installer.Command,
			Env:                      env,
			SecurityContext:          securityContext,
			ImagePullPolicy:          imagePullPolicy,
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		},
		TargetContainerName: target.Name,
	}
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
