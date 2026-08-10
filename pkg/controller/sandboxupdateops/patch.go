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

package sandboxupdateops

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// isSandboxTemplateMatchPatch checks whether the sandbox template already matches
// the patch target. If applying the SMP produces no change, the sandbox is already
// up-to-date and should be skipped entirely (no patch, no counting).
func isSandboxTemplateMatchPatch(sbx *agentsv1alpha1.Sandbox, ops *agentsv1alpha1.SandboxUpdateOps) bool {
	if sbx.Spec.Template == nil || len(ops.Spec.Patch.Raw) == 0 {
		return false
	}
	originalBytes, err := json.Marshal(sbx.Spec.Template)
	if err != nil {
		return false
	}
	mergedBytes, err := strategicpatch.StrategicMergePatch(originalBytes, ops.Spec.Patch.Raw, &v1.PodTemplateSpec{})
	if err != nil {
		klog.ErrorS(err, "Failed to apply strategic merge patch for match check", "sandbox", klog.KObj(sbx))
		return false
	}
	merged := &v1.PodTemplateSpec{}
	if err := json.Unmarshal(mergedBytes, merged); err != nil {
		return false
	}
	return reflect.DeepEqual(sbx.Spec.Template, merged)
}

// mergeTemplate applies the ops strategic merge patch onto the sandbox template
// in-place. It uses raw JSON bytes directly to preserve $patch directives
// (e.g. "$patch": "delete") that would be lost if unmarshalled into a typed
// Go struct first.
func mergeTemplate(modified *agentsv1alpha1.Sandbox, ops *agentsv1alpha1.SandboxUpdateOps) error {
	if len(ops.Spec.Patch.Raw) == 0 || modified.Spec.Template == nil {
		return nil
	}
	originalBytes, err := json.Marshal(modified.Spec.Template)
	if err != nil {
		return fmt.Errorf("failed to marshal original template: %w", err)
	}
	mergedBytes, err := strategicpatch.StrategicMergePatch(originalBytes, ops.Spec.Patch.Raw, &v1.PodTemplateSpec{})
	if err != nil {
		return fmt.Errorf("failed to apply strategic merge patch: %w", err)
	}
	merged := &v1.PodTemplateSpec{}
	if err := json.Unmarshal(mergedBytes, merged); err != nil {
		return fmt.Errorf("failed to unmarshal merged template: %w", err)
	}
	modified.Spec.Template = merged
	return nil
}

// patchAndExpect computes a merge patch from sbx to modified, applies it,
// logs the operation, and records a resource-version expectation. The tag
// is appended to log messages to distinguish phase 2 from normal upgrades.
func (r *Reconciler) patchAndExpect(ctx context.Context, sbx, modified *agentsv1alpha1.Sandbox, tag string) error {
	patch := client.MergeFrom(sbx)
	patchData, patchErr := patch.Data(modified)
	if patchErr != nil {
		klog.ErrorS(patchErr, "Failed to compute patch data"+tag, "sandbox", klog.KObj(sbx))
	} else {
		klog.InfoS("Applying sandbox patch"+tag, "sandbox", klog.KObj(sbx), "patch", string(patchData))
	}
	if err := r.Patch(ctx, modified, patch); err != nil {
		klog.ErrorS(err, "Failed to patch sandbox"+tag, "sandbox", klog.KObj(sbx))
		return err
	}
	klog.InfoS("Successfully patched sandbox"+tag, "sandbox", klog.KObj(sbx))
	ResourceVersionExpectations.Expect(modified)
	return nil
}

func (r *Reconciler) applySandboxPatch(ctx context.Context, sbx *agentsv1alpha1.Sandbox, ops *agentsv1alpha1.SandboxUpdateOps) error {
	modified := sbx.DeepCopy()

	// Set UpgradePolicy based on strategy type
	policyType := agentsv1alpha1.SandboxUpgradePolicyRecreate
	if ops.Spec.UpdateStrategy.Type == agentsv1alpha1.SandboxUpdateOpsStrategyCheckpointRestore {
		policyType = agentsv1alpha1.SandboxUpgradePolicyCheckpointRestore
	}
	modified.Spec.UpgradePolicy = &agentsv1alpha1.SandboxUpgradePolicy{
		Type: policyType,
	}

	// Set Lifecycle
	if ops.Spec.Lifecycle != nil {
		modified.Spec.Lifecycle = ops.Spec.Lifecycle.DeepCopy()
	} else {
		modified.Spec.Lifecycle = nil
	}

	// Add tracking label and clear stale upgrade-failed label
	if modified.Labels == nil {
		modified.Labels = map[string]string{}
	}
	modified.Labels[agentsv1alpha1.LabelSandboxUpdateOps] = ops.Name
	delete(modified.Labels, agentsv1alpha1.LabelSandboxUpgradeFailed)

	// For paused sandboxes, use two-phase upgrade:
	// Phase 1: set the resume trigger annotation without patching the template.
	// The sandbox controller resumes with the OLD template, then transitions
	// to ResumeSucceed. SandboxUpdateOps patches the template in phase 2.
	if sbx.Status.Phase == agentsv1alpha1.SandboxPaused {
		if modified.Annotations == nil {
			modified.Annotations = map[string]string{}
		}
		modified.Annotations[agentsv1alpha1.AnnotationUpgradeResumeTrigger] = agentsv1alpha1.True
		return r.patchAndExpect(ctx, sbx, modified, " (resume trigger)")
	}

	// Normal upgrade (Running): apply template patch.
	if err := mergeTemplate(modified, ops); err != nil {
		return err
	}
	return r.patchAndExpect(ctx, sbx, modified, "")
}

// applyTemplatePatch is phase 2 of the two-phase upgrade for paused sandboxes.
// It patches the sandbox template and removes the resume trigger annotation.
// The sandbox already has UpgradePolicy, Lifecycle, and tracking label from
// phase 1; this method only applies the template merge and annotation cleanup.
func (r *Reconciler) applyTemplatePatch(ctx context.Context, sbx *agentsv1alpha1.Sandbox, ops *agentsv1alpha1.SandboxUpdateOps) error {
	modified := sbx.DeepCopy()

	if err := mergeTemplate(modified, ops); err != nil {
		return err
	}

	// Remove the resume trigger annotation (phase 2 cleanup)
	delete(modified.Annotations, agentsv1alpha1.AnnotationUpgradeResumeTrigger)

	return r.patchAndExpect(ctx, sbx, modified, " (phase 2)")
}
