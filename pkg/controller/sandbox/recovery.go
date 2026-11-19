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

package sandbox

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/sandbox/core"
	"github.com/openkruise/agents/pkg/utils"
)

// beginEvictionRecovery writes intent without performing any Pod lifecycle action.
// Already Failed Sandboxes stay terminal; policy changes cannot resurrect them.
func beginEvictionRecovery(args core.EnsureFuncArgs) bool {
	box, pod, status := args.Box, args.Pod, args.NewStatus
	if !core.RecoveryEnabled(box) || !core.IsEvictedPod(pod) ||
		box.Status.Phase != agentsv1alpha1.SandboxRunning ||
		utils.IsControlledBySandboxSet(box) {
		return false
	}
	recovery := status.Recovery
	hash, err := core.RecoveryTemplateHash(box)
	if err != nil || recovery == nil || recovery.PodUID != pod.UID || pod.UID == "" ||
		!metav1.IsControlledBy(pod, box) || hash != recovery.TemplateHash || pod.Spec.NodeName == "" {
		status.Phase = agentsv1alpha1.SandboxFailed
		status.Message = "Eviction recovery requires an unchanged template and a recorded healthy Pod/volume baseline"
		core.SetRecoveryNotReady(status, "RecoveryBaselineMissing", status.Message)
		return true
	}
	if !box.Spec.Paused && recovery.Attempts >= core.RecoveryPolicy(box).MaxAttempts {
		status.Phase = agentsv1alpha1.SandboxFailed
		status.Message = "Eviction recovery attempt budget exhausted"
		core.SetRecoveryNotReady(status, "RecoveryFailed", status.Message)
		return true
	}
	now := metav1.Now()
	recovery.StartedAt = &now
	recovery.HealthySince, recovery.RetryAfter = nil, nil
	recovery.ReplacementPodUID = ""
	recovery.NodeName = pod.Spec.NodeName
	status.Phase, status.Message = agentsv1alpha1.SandboxRecovering, "Pod was evicted"
	core.SetRecoveryNotReady(status, "Recovering", status.Message)
	utils.SetSandboxCondition(status, metav1.Condition{
		Type: string(agentsv1alpha1.RuntimeInitialized), Status: metav1.ConditionFalse,
		Reason: agentsv1alpha1.SandboxConditionRuntimeInitReasonPending, LastTransitionTime: now,
	})
	return true
}

// recordRecoveryBaseline runs before publishing Ready. A missing/replaced PVC
// cannot be silently recreated by ensureVolumeClaimTemplates on later reconciles.
func (r *SandboxReconciler) recordRecoveryBaseline(ctx context.Context, args core.EnsureFuncArgs) (time.Duration, error) {
	box, pod, status := args.Box, args.Pod, args.NewStatus
	if !core.RecoveryEnabled(box) || status.Phase != agentsv1alpha1.SandboxRunning || pod == nil {
		return 0, nil
	}
	ready := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionReady))
	if ready == nil || ready.Status != metav1.ConditionTrue {
		if status.Recovery != nil {
			status.Recovery.HealthySince = nil
		}
		return 0, nil
	}
	if !metav1.IsControlledBy(pod, box) || pod.UID == "" {
		return 0, fmt.Errorf("cannot record recovery baseline for a Pod not owned by this Sandbox")
	}
	hash, err := core.RecoveryTemplateHash(box)
	if err != nil {
		return 0, err
	}
	volumes, err := core.RecoveryVolumes(ctx, r.Client, box)
	if err != nil {
		return 0, err
	}
	images, err := core.RecoveryImages(pod)
	if err != nil {
		return 0, err
	}
	if status.Recovery == nil {
		status.Recovery = &agentsv1alpha1.SandboxRecoveryStatus{}
	}
	recovery := status.Recovery
	if recovery.PodUID != pod.UID {
		recovery.HealthySince = nil
	}
	recovery.PodUID, recovery.NodeName, recovery.TemplateHash = pod.UID, pod.Spec.NodeName, hash
	recovery.Volumes = volumes
	recovery.Images = images
	if recovery.HealthySince == nil {
		now := metav1.Now()
		recovery.HealthySince = &now
	}
	if recovery.Attempts == 0 {
		return 0, nil
	}
	wait := time.Until(recovery.HealthySince.Add(time.Duration(core.RecoveryPolicy(box).HealthyResetSeconds) * time.Second))
	if wait <= 0 {
		recovery.Attempts = 0
		return 0, nil
	}
	return wait, nil
}
