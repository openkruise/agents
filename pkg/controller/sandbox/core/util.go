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
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

// HashSandbox calculates the hash value using sandbox.spec.template
func HashSandbox(box *agentsv1alpha1.Sandbox) (string, string) {
	if box.Spec.Template == nil {
		if box.Spec.TemplateRef == nil {
			return "", ""
		}
		// templateRef mode does not carry inline PodTemplate in Sandbox spec.
		// Use TemplateRef itself as a stable revision key to avoid nil dereference.
		by, _ := json.Marshal(box.Spec.TemplateRef)
		hash := utils.HashData(by)
		return hash, hash
	}

	// hash using sandbox.spec.template
	by, _ := json.Marshal(*box.Spec.Template)
	hash := utils.HashData(by)

	// hash using sandbox.spec.template without image and resources
	tempClone := box.Spec.Template.DeepCopy()
	tempClone.Labels = nil
	tempClone.Annotations = nil
	for i := range tempClone.Spec.Containers {
		container := &tempClone.Spec.Containers[i]
		container.Image = ""
		container.Resources = corev1.ResourceRequirements{}
	}
	for i := range tempClone.Spec.InitContainers {
		container := &tempClone.Spec.InitContainers[i]
		container.Image = ""
		container.Resources = corev1.ResourceRequirements{}
	}
	by, _ = json.Marshal(*tempClone)
	hashImmutablePart := utils.HashData(by)
	return hash, hashImmutablePart
}

// GeneratePVCName generates a persistent volume claim name from template name and sandbox name
func GeneratePVCName(templateName, sandboxName string) (string, error) {
	if templateName == "" || sandboxName == "" {
		return "", fmt.Errorf("template name and sandbox name cannot be empty")
	}

	name := fmt.Sprintf("%s-%s", templateName, sandboxName)

	return name, nil
}

func GetControllerKey(obj client.Object) string {
	return types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}.String()
}

// ensureStopPaused handles the Stop pause strategy: directly delete the pod.
// It marks the pause complete when the pod is gone, waits while the pod is
// terminating, or deletes the pod using cli.
// successReason is set on the Paused condition when deletion is complete.
func ensureStopPaused(
	ctx context.Context,
	cli client.Client,
	args EnsureFuncArgs,
	successReason string,
) error {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
	// Pod deletion completed, pause done
	if pod == nil {
		cond.Status = metav1.ConditionTrue
		cond.Reason = successReason
		cond.LastTransitionTime = metav1.Now()
		utils.SetSandboxCondition(newStatus, *cond)
		klog.FromContext(ctx).Info("Pod deletion completed, pause phase completed", "sandbox", klog.KObj(box))
		return nil
	}
	// Pod deletion in progress, wait
	if !pod.DeletionTimestamp.IsZero() {
		klog.FromContext(ctx).Info("Sandbox wait pod paused", "sandbox", klog.KObj(box))
		return nil
	}
	// Remove the propagated credential while the runtime is still reachable.
	// Under Stop only PVC-backed data survives the pod deletion, so a credential
	// a propagator placed on a mounted volume outlives this pause without it.
	if err := removePropagatedCredential(ctx, box, credentialCleanupReasonPause); err != nil {
		return err
	}
	// Delete the pod
	err := client.IgnoreNotFound(cli.Delete(ctx, pod, &client.DeleteOptions{GracePeriodSeconds: ptr.To(int64(5))}))
	if err != nil {
		klog.FromContext(ctx).Error(err, "Delete pod failed", "sandbox", klog.KObj(box))
		return err
	}
	klog.FromContext(ctx).Info("Delete pod success", "sandbox", klog.KObj(box))
	return nil
}

// ensureCheckpointPaused handles the Checkpoint pause strategy:
// create/validate a checkpoint first, then delete the pod.
// checkpointControl manages the Checkpoint CR lifecycle.
// Once the checkpoint succeeds, the function delegates to ensureStopPaused
// for the actual pod deletion.
func ensureCheckpointPaused(
	ctx context.Context,
	cli client.Client,
	checkpointControl *CheckpointControl,
	args EnsureFuncArgs,
) error {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
	// Remove the propagated credential before anything is dumped. Once
	// AssumePodCheckpointed runs with filesystem or memory in scope, the
	// credential is inside a Checkpoint artifact that outlives both the pod and
	// the pause, and resume mints a fresh token anyway
	// (sandbox_initializer.go reinitSecurityToken), so the dumped copy is stale
	// from the moment it is taken.
	if err := removePropagatedCredential(ctx, box, credentialCleanupReasonPause); err != nil {
		return err
	}
	// Ensure checkpoint is completed before proceeding to pod deletion.
	// The scope is derived from sandbox.spec.persistentContents: requested
	// dump contents (filesystem/memory) are passed through and podInfo is
	// used when no dump content is requested, with image validation.
	scope := CheckpointScope{
		PersistentContents: checkpointContentsForPause(box),
		ValidateImages:     true,
	}
	if rejected := checkpointControl.AssumePodCheckpointed(ctx, pod, box, newStatus, cond, scope); rejected {
		return nil
	}
	// Proceed to delete the pod (same as stop strategy).
	return ensureStopPaused(ctx, cli, args, agentsv1alpha1.SandboxPausedReasonSnapshotPauseSucceed)
}

// markResumeSucceeded marks the resume as succeeded: it unconditionally sets
// Resumed=True (instead of flipping an existing False) so the upgrade Resuming
// stage works even when Resumed was not pre-seeded, and resets
// RuntimeInitialized to Pending so every resume cycle triggers fresh runtime
// re-initialization and CSI re-mount.
func markResumeSucceeded(newStatus *agentsv1alpha1.SandboxStatus) {
	utils.SetSandboxCondition(newStatus, metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxConditionResumed),
		Status:             metav1.ConditionTrue,
		Reason:             agentsv1alpha1.SandboxResumeReasonResumePod,
		LastTransitionTime: metav1.Now(),
	})
	utils.SetSandboxCondition(newStatus, metav1.Condition{
		Type:               string(agentsv1alpha1.RuntimeInitialized),
		Status:             metav1.ConditionFalse,
		Reason:             agentsv1alpha1.SandboxConditionRuntimeInitReasonPending,
		Message:            "Waiting for pod ready before initialization",
		LastTransitionTime: metav1.Now(),
	})
}

// StaleSandboxPodOwner returns the UID of the Sandbox owner reference on pod
// when it does not match the current sandbox, i.e. the pod is a leftover from
// a previous sandbox generation with the same name (see issue #756).
// Returns "", false when the pod is owned by the current sandbox or carries
// no Sandbox owner reference.
func StaleSandboxPodOwner(pod *corev1.Pod, box *agentsv1alpha1.Sandbox) (types.UID, bool) {
	for i := range pod.OwnerReferences {
		ref := &pod.OwnerReferences[i]
		if ref.Kind == "Sandbox" && ref.APIVersion == agentsv1alpha1.SchemeGroupVersion.String() {
			if ref.UID == box.UID {
				return "", false
			}
			return ref.UID, true
		}
	}
	return "", false
}
