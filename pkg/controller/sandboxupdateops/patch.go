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
	"bytes"
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

// sanitizeTemplatePatch strips explicit nulls for fields a pod template can
// never drop, so they cannot act as strategic-merge-patch delete directives.
// Marshaling a PodTemplateSpec whose PodSpec is empty emits
// "spec":{"containers":null} because PodSpec.Containers has no omitempty tag;
// without this cleanup an annotation-only patch would delete the required
// containers of the sandbox template. The raw bytes are returned unchanged
// when the patch does not carry such nulls or cannot be parsed (parse errors
// are left to the strategic merge patch call, which reports them).
// Numbers are decoded with UseNumber so the round trip keeps int64 values
// (e.g. activeDeadlineSeconds, runAsUser) beyond float64 precision intact.
func sanitizeTemplatePatch(raw []byte) []byte {
	var obj map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return raw
	}
	spec, ok := obj["spec"].(map[string]interface{})
	if !ok {
		return raw
	}
	// containers is required in a pod spec, so a null value can never be a
	// legitimate delete request; it only appears when an empty PodSpec is
	// marshaled into the patch.
	containers, exists := spec["containers"]
	if !exists || containers != nil {
		return raw
	}
	delete(spec, "containers")
	if len(spec) == 0 {
		delete(obj, "spec")
	}
	sanitized, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return sanitized
}

// isSandboxTemplateMatchPatch checks whether the sandbox template already matches
// the patch target. If applying the SMP produces no change, the sandbox is already
// up-to-date and should be skipped entirely (no patch, no counting).
func isSandboxTemplateMatchPatch(sbx *agentsv1alpha1.Sandbox, ops *agentsv1alpha1.SandboxUpdateOps) bool {
	if sbx.Spec.Template == nil || len(ops.Spec.Patch.Raw) == 0 {
		return false
	}
	merged, err := mergePodTemplate(sbx.Spec.Template, ops.Spec.Patch.Raw)
	if err != nil {
		klog.ErrorS(err, "Failed to merge template for match check", "sandbox", klog.KObj(sbx))
		return false
	}
	return reflect.DeepEqual(sbx.Spec.Template, merged)
}

// isInstallerUpdateOps reports whether the ops uses the InstallerUpdate strategy.
func isInstallerUpdateOps(ops *agentsv1alpha1.SandboxUpdateOps) bool {
	return ops.Spec.UpdateStrategy.Type == agentsv1alpha1.SandboxUpdateOpsStrategyInstallerUpdate
}

// sandboxMatchesOpsTarget reports whether the sandbox already reached the
// operation's target. For InstallerUpdate the target is the install of this
// ops's patch version token (status.completedPatchVersion); for the
// template-based strategies it is the template matching the patch.
func sandboxMatchesOpsTarget(sbx *agentsv1alpha1.Sandbox, ops *agentsv1alpha1.SandboxUpdateOps) bool {
	if isInstallerUpdateOps(ops) {
		return sbx.Status.CompletedPatchVersion == string(ops.UID)
	}
	return isSandboxTemplateMatchPatch(sbx, ops)
}

// mergeTemplate applies the ops strategic merge patch onto the sandbox template
// in-place. It uses raw JSON bytes directly to preserve $patch directives
// (e.g. "$patch": "delete") that would be lost if unmarshalled into a typed
// Go struct first.
func mergeTemplate(modified *agentsv1alpha1.Sandbox, ops *agentsv1alpha1.SandboxUpdateOps) error {
	merged, err := mergePodTemplate(modified.Spec.Template, ops.Spec.Patch.Raw)
	if err != nil {
		return err
	}
	modified.Spec.Template = merged
	return nil
}

func mergePodTemplate(template *v1.PodTemplateSpec, raw []byte) (*v1.PodTemplateSpec, error) {
	if template == nil || len(raw) == 0 {
		return template, nil
	}
	original, err := json.Marshal(template)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal original template: %w", err)
	}
	data, err := strategicpatch.StrategicMergePatch(original, sanitizeTemplatePatch(raw), &v1.PodTemplateSpec{})
	if err != nil {
		return nil, fmt.Errorf("failed to apply strategic merge patch: %w", err)
	}
	merged := new(v1.PodTemplateSpec)
	if err := json.Unmarshal(data, merged); err != nil {
		return nil, fmt.Errorf("failed to unmarshal merged template: %w", err)
	}
	return merged, nil
}

// patchAndExpect computes a merge patch from sbx to modified, applies it,
// logs the operation, and records a resource-version expectation.
func (r *Reconciler) patchAndExpect(ctx context.Context, sbx, modified *agentsv1alpha1.Sandbox, phase string) error {
	logger := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx), "phase", phase)
	patch := client.MergeFrom(sbx)
	data, err := patch.Data(modified)
	if err != nil {
		return fmt.Errorf("failed to compute sandbox patch: %w", err)
	}
	logger.Info("Applying sandbox patch", "patch", string(data))
	if err := r.Patch(ctx, modified, client.RawPatch(patch.Type(), data)); err != nil {
		logger.Error(err, "Failed to patch sandbox")
		return err
	}
	ResourceVersionExpectations.Expect(modified)
	logger.Info("Successfully patched sandbox")
	return nil
}

func (r *Reconciler) applySandboxPatch(ctx context.Context, sbx *agentsv1alpha1.Sandbox, ops *agentsv1alpha1.SandboxUpdateOps) error {
	modified := sbx.DeepCopy()

	modified.Spec.UpgradePolicy = &agentsv1alpha1.SandboxUpgradePolicy{
		Type: agentsv1alpha1.SandboxUpgradePolicyRecreate,
	}
	switch ops.Spec.UpdateStrategy.Type {
	case agentsv1alpha1.SandboxUpdateOpsStrategyCheckpointRestore:
		modified.Spec.UpgradePolicy.Type = agentsv1alpha1.SandboxUpgradePolicyCheckpointRestore
	case agentsv1alpha1.SandboxUpdateOpsStrategyInstallerUpdate:
		modified.Spec.UpgradePolicy.Type = agentsv1alpha1.SandboxUpgradePolicyInstallerUpdate
		modified.Spec.UpgradePolicy.PatchVersion = string(ops.UID)
		modified.Spec.UpgradePolicy.Installer = ops.Spec.UpdateStrategy.Installer.DeepCopy()
	}

	// UpdateOps owns only upgrade hooks. Preserve pause/resume hooks configured
	// directly on the Sandbox while replacing the two upgrade fields.
	lifecycle := modified.Spec.Lifecycle
	if lifecycle == nil {
		lifecycle = &agentsv1alpha1.SandboxLifecycle{}
	}
	lifecycle.PreUpgrade = nil
	lifecycle.PostUpgrade = nil
	if ops.Spec.Lifecycle != nil {
		lifecycle.PreUpgrade = ops.Spec.Lifecycle.PreUpgrade.DeepCopy()
		lifecycle.PostUpgrade = ops.Spec.Lifecycle.PostUpgrade.DeepCopy()
	}
	if lifecycle.PrePause == nil && lifecycle.PostResume == nil && lifecycle.PreUpgrade == nil && lifecycle.PostUpgrade == nil {
		modified.Spec.Lifecycle = nil
	} else {
		modified.Spec.Lifecycle = lifecycle
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
		return r.patchAndExpect(ctx, sbx, modified, "resume-trigger")
	}

	// Normal upgrade (Running): apply template patch. InstallerUpdate carries
	// its payload in the installer image instead, so there is no template
	// change to merge.
	if !isInstallerUpdateOps(ops) {
		if err := mergeTemplate(modified, ops); err != nil {
			return err
		}
	}
	return r.patchAndExpect(ctx, sbx, modified, "update")
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

	return r.patchAndExpect(ctx, sbx, modified, "template")
}
