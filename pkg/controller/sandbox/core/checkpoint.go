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
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
	"github.com/openkruise/agents/pkg/utils/fieldindex"
)

// CheckpointControl manages Checkpoint CR lifecycle for sandbox pause/resume flows.
type CheckpointControl struct {
	client.Client
	recorder record.EventRecorder
}

const (
	EventCheckpointStarted   = "CheckpointStarted"
	EventCheckpointSucceeded = "CheckpointSucceeded"
)

// NewCheckpointControl creates a new CheckpointControl.
func NewCheckpointControl(cli client.Client, recorder record.EventRecorder) *CheckpointControl {
	return &CheckpointControl{Client: cli, recorder: recorder}
}

// AssumePodCheckpointed validates container images and manages the Checkpoint CR lifecycle.
// Returns true if the pause flow should wait (checkpoint in progress or image rejected).
func (c *CheckpointControl) AssumePodCheckpointed(ctx context.Context, pod *corev1.Pod, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus, cond *metav1.Condition) bool {
	if !utilfeature.DefaultFeatureGate.Enabled(features.SandboxPauseCheckpointGate) {
		cond.Reason = agentsv1alpha1.SandboxPausedReasonCheckpointSucceeded
		return false
	}
	// Allow-list of paused reasons that should drive the checkpoint flow.
	// Any other reason (e.g. CheckpointSucceeded already reached, or a reason
	// introduced in the future) skips this flow on purpose; new reasons that
	// need checkpointing must be added here explicitly.
	switch cond.Reason {
	case "",
		agentsv1alpha1.SandboxPausedReasonPausing,
		agentsv1alpha1.SandboxPausedReasonCheckpointCreating,
		agentsv1alpha1.SandboxPausedReasonImageChanged,
		agentsv1alpha1.SandboxPausedReasonCheckpointFailed:
		// fall through to checkpoint handling below
	default:
		return false
	}

	if err := validateContainerImages(pod, box); err != nil {
		cond.Status = metav1.ConditionFalse
		cond.Reason = agentsv1alpha1.SandboxPausedReasonImageChanged
		cond.Message = err.Error()
		utils.SetSandboxCondition(newStatus, *cond)
		c.recorder.Event(box, corev1.EventTypeWarning, agentsv1alpha1.SandboxPausedReasonImageChanged, err.Error())
		klog.ErrorS(err, "Image validation failed, pause rejected", "sandbox", klog.KObj(box))
		return true
	}
	if cond.Reason == "" || cond.Reason == agentsv1alpha1.SandboxPausedReasonPausing ||
		cond.Reason == agentsv1alpha1.SandboxPausedReasonImageChanged {
		cond.Reason = agentsv1alpha1.SandboxPausedReasonCheckpointCreating
		cond.Message = "Checkpoint created, waiting for completion"
		utils.SetSandboxCondition(newStatus, *cond)
	}

	cpList, err := listCheckpointsForSandbox(ctx, c.Client, box)
	if err != nil {
		klog.ErrorS(err, "Failed to list checkpoints", "sandbox", klog.KObj(box))
		cond.Reason = agentsv1alpha1.SandboxPausedReasonCheckpointFailed
		cond.Message = fmt.Sprintf("Failed to list checkpoints: %v", err)
		utils.SetSandboxCondition(newStatus, *cond)
		c.recorder.Event(box, corev1.EventTypeWarning, agentsv1alpha1.SandboxPausedReasonCheckpointFailed, cond.Message)
		return true
	} else if len(cpList) == 0 {
		if err := c.createCheckpoint(ctx, box); err != nil {
			klog.ErrorS(err, "Failed to create checkpoint", "sandbox", klog.KObj(box))
			cond.Reason = agentsv1alpha1.SandboxPausedReasonCheckpointFailed
			cond.Message = fmt.Sprintf("Failed to create checkpoint: %v", err)
			utils.SetSandboxCondition(newStatus, *cond)
			c.recorder.Event(box, corev1.EventTypeWarning, agentsv1alpha1.SandboxPausedReasonCheckpointFailed, cond.Message)
		}
		return true
	}

	cp := &cpList[0]
	switch cp.Status.Phase {
	case agentsv1alpha1.CheckpointSucceeded:
		cond.Reason = agentsv1alpha1.SandboxPausedReasonCheckpointSucceeded
		cond.Message = ""
		utils.SetSandboxCondition(newStatus, *cond)
		c.recordCheckpointEvent(box, corev1.EventTypeNormal, EventCheckpointSucceeded, "Checkpoint %s succeeded", cp.Name)
		return false
	case agentsv1alpha1.CheckpointFailed:
		cond.Reason = agentsv1alpha1.SandboxPausedReasonCheckpointFailed
		cond.Message = fmt.Sprintf("Checkpoint failed: %s", cp.Status.Message)
		utils.SetSandboxCondition(newStatus, *cond)
		c.recorder.Event(box, corev1.EventTypeWarning, agentsv1alpha1.SandboxPausedReasonCheckpointFailed, cond.Message)
		return true
	default:
		cond.Message = fmt.Sprintf("Waiting for checkpoint %s", cp.Name)
		utils.SetSandboxCondition(newStatus, *cond)
		klog.InfoS("Waiting for checkpoint to complete", "sandbox", klog.KObj(box), "checkpoint", cp.Name, "phase", cp.Status.Phase)
		return true
	}
}

// GetPodTemplateDelta retrieves the pod template delta from the latest checkpoint for the given sandbox.
func (c *CheckpointControl) GetPodTemplateDelta(ctx context.Context, box *agentsv1alpha1.Sandbox) *runtime.RawExtension {
	if !utilfeature.DefaultFeatureGate.Enabled(features.SandboxPauseCheckpointGate) {
		return nil
	}
	cpList, cpErr := listCheckpointsForSandbox(ctx, c.Client, box)
	if cpErr != nil {
		klog.ErrorS(cpErr, "Failed to list checkpoints for resume, proceeding without", "sandbox", klog.KObj(box))
		return nil
	}
	// Normally the checkpoint list contains only one element
	for i := range cpList {
		if len(cpList[i].Status.PodTemplateDelta.Raw) > 0 {
			return &cpList[i].Status.PodTemplateDelta
		}
	}
	return nil
}

// Cleanup deletes all pod-info Checkpoint CRs for the given sandbox.
func (c *CheckpointControl) Cleanup(ctx context.Context, box *agentsv1alpha1.Sandbox) {
	if !utilfeature.DefaultFeatureGate.Enabled(features.SandboxPauseCheckpointGate) {
		return
	}
	cpList, cpErr := listCheckpointsForSandbox(ctx, c.Client, box)
	if cpErr != nil {
		klog.ErrorS(cpErr, "Failed to list checkpoints for cleanup", "sandbox", klog.KObj(box))
		return
	}
	for i := range cpList {
		ScaleExpectation.ExpectScale(GetControllerKey(box), expectations.Delete, cpList[i].Name)
		if delErr := c.Delete(ctx, &cpList[i]); delErr != nil && !errors.IsNotFound(delErr) {
			ScaleExpectation.ObserveScale(GetControllerKey(box), expectations.Delete, cpList[i].Name)
			klog.ErrorS(delErr, "Failed to delete checkpoint after resume", "sandbox", klog.KObj(box), "checkpoint", cpList[i].Name)
		} else {
			klog.InfoS("Deleted checkpoint after successful resume", "sandbox", klog.KObj(box), "checkpoint", cpList[i].Name)
		}
	}
}

// createCheckpoint creates a Checkpoint CR. The checkpoint controller is
// responsible for processing it and updating the status.
//
// The name carries a random suffix so each invocation produces a distinct
// checkpoint name. Idempotency within the same reconcile cycle is guaranteed
// by the caller, which only invokes this function when no existing checkpoint
// is found for the sandbox (see AssumePodCheckpointed).
func (c *CheckpointControl) createCheckpoint(ctx context.Context, box *agentsv1alpha1.Sandbox) error {
	cpName := box.Name + "-" + utils.RandStringN(8)
	cp := &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cpName,
			Namespace: box.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(box, sandboxControllerKind),
			},
			Labels: map[string]string{
				agentsv1alpha1.CheckpointLabelSandboxName: box.Name,
				agentsv1alpha1.CheckpointLabelType:        agentsv1alpha1.CheckpointTypePodInfo,
			},
		},
		Spec: agentsv1alpha1.CheckpointSpec{
			SandboxName: &box.Name,
		},
	}
	ScaleExpectation.ExpectScale(GetControllerKey(box), expectations.Create, cpName)
	if err := c.Create(ctx, cp); err != nil {
		ScaleExpectation.ObserveScale(GetControllerKey(box), expectations.Create, cpName)
		return fmt.Errorf("failed to create checkpoint CR: %w", err)
	}
	c.recordCheckpointEvent(box, corev1.EventTypeNormal, EventCheckpointStarted, "Checkpoint %s created, waiting for completion", cpName)
	klog.InfoS("Created checkpoint CR", "sandbox", klog.KObj(box), "checkpoint", cpName)
	return nil
}

func (c *CheckpointControl) recordCheckpointEvent(box *agentsv1alpha1.Sandbox, eventType, reason, messageFmt string, args ...any) {
	if c.recorder == nil {
		return
	}
	c.recorder.Eventf(box, eventType, reason, messageFmt, args...)
}

// validateContainerImages compares each user container's Image in the live Pod
// against the Image defined in sandbox.spec.template. If any image differs,
// the pause is rejected.
func validateContainerImages(pod *corev1.Pod, box *agentsv1alpha1.Sandbox) error {
	if box.Spec.Template == nil {
		return nil
	}
	for _, tc := range box.Spec.Template.Spec.Containers {
		for _, pc := range pod.Spec.Containers {
			if tc.Name == pc.Name && tc.Image != pc.Image {
				return fmt.Errorf("container %q image changed from %q to %q, pause is not allowed",
					tc.Name, tc.Image, pc.Image)
			}
		}
	}
	for _, tc := range box.Spec.Template.Spec.InitContainers {
		if tc.RestartPolicy == nil || *tc.RestartPolicy != corev1.ContainerRestartPolicyAlways {
			continue
		}
		for _, pc := range pod.Spec.InitContainers {
			if tc.Name == pc.Name && tc.Image != pc.Image {
				return fmt.Errorf("sidecar init container %q image changed from %q to %q, pause is not allowed",
					tc.Name, tc.Image, pc.Image)
			}
		}
	}
	return nil
}

// listCheckpointsForSandbox returns all pod-info Checkpoint CRs for the given sandbox,
// sorted newest-first by creation timestamp.
func listCheckpointsForSandbox(ctx context.Context, cli client.Client, box *agentsv1alpha1.Sandbox) ([]agentsv1alpha1.Checkpoint, error) {
	cpList := &agentsv1alpha1.CheckpointList{}
	err := cli.List(ctx, cpList,
		client.InNamespace(box.Namespace),
		client.MatchingFields{fieldindex.IndexNameForOwnerRefUID: string(box.UID)},
		client.MatchingLabels{agentsv1alpha1.CheckpointLabelType: agentsv1alpha1.CheckpointTypePodInfo},
		client.UnsafeDisableDeepCopy,
	)
	if err != nil {
		return nil, err
	}
	if len(cpList.Items) == 0 {
		return nil, nil
	}
	sort.Slice(cpList.Items, func(i, j int) bool {
		return cpList.Items[j].CreationTimestamp.Before(&cpList.Items[i].CreationTimestamp)
	})
	return cpList.Items, nil
}
