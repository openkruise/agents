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
	sandboxcore "github.com/openkruise/agents/pkg/controller/sandbox/core"
)

// mergeTemplateWithPatch applies the ops patch (Strategic Merge Patch) to the
// sandbox's current template and returns the merged result. Raw JSON bytes are
// used directly to preserve $patch directives (e.g. "$patch": "delete") that
// would be lost if unmarshalled into a typed Go struct first.
func mergeTemplateWithPatch(sbx *agentsv1alpha1.Sandbox, ops *agentsv1alpha1.SandboxUpdateOps) (*v1.PodTemplateSpec, error) {
	originalBytes, err := json.Marshal(sbx.Spec.Template)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal original template: %w", err)
	}
	mergedBytes, err := strategicpatch.StrategicMergePatch(originalBytes, ops.Spec.Patch.Raw, &v1.PodTemplateSpec{})
	if err != nil {
		return nil, fmt.Errorf("failed to apply strategic merge patch: %w", err)
	}
	merged := &v1.PodTemplateSpec{}
	if err := json.Unmarshal(mergedBytes, merged); err != nil {
		return nil, fmt.Errorf("failed to unmarshal merged template: %w", err)
	}
	return merged, nil
}

// isSandboxTemplateMatchPatch checks whether the sandbox template already matches
// the patch target. If applying the SMP produces no change, the sandbox is already
// up-to-date and should be skipped entirely (no patch, no counting).
func isSandboxTemplateMatchPatch(sbx *agentsv1alpha1.Sandbox, ops *agentsv1alpha1.SandboxUpdateOps) bool {
	if sbx.Spec.Template == nil || len(ops.Spec.Patch.Raw) == 0 {
		return false
	}
	merged, err := mergeTemplateWithPatch(sbx, ops)
	if err != nil {
		klog.ErrorS(err, "Failed to apply strategic merge patch for match check", "sandbox", klog.KObj(sbx))
		return false
	}
	return reflect.DeepEqual(sbx.Spec.Template, merged)
}

// validateInplaceUpdateFeasible checks, before patching, whether applying the ops
// patch to the sandbox template keeps the hash-immutable-part unchanged, i.e. the
// patch only modifies container images, resources, and template metadata. It
// mirrors the sandbox controller's own check (annotation vs. new template hash) so
// an infeasible patch is caught before the sandbox is ever patched. Returns a
// non-empty message describing the violation.
func validateInplaceUpdateFeasible(sbx *agentsv1alpha1.Sandbox, ops *agentsv1alpha1.SandboxUpdateOps) string {
	// Without an inline template or a patch, nothing can change the immutable part.
	if sbx.Spec.Template == nil || len(ops.Spec.Patch.Raw) == 0 {
		return ""
	}
	// No hash annotation means the sandbox controller has not recorded a baseline
	// yet; skip the check, consistent with the controller's own short-circuit.
	annotationHash := sbx.Annotations[agentsv1alpha1.SandboxHashImmutablePart]
	if annotationHash == "" {
		return ""
	}
	merged, err := mergeTemplateWithPatch(sbx, ops)
	if err != nil {
		return err.Error()
	}
	mergedBox := sbx.DeepCopy()
	mergedBox.Spec.Template = merged
	if _, immutableHash := sandboxcore.HashSandbox(mergedBox); immutableHash != annotationHash {
		return "patch modifies fields other than container images, resources, and metadata, which the in-place update path does not support"
	}
	return ""
}

func (r *Reconciler) applySandboxPatch(ctx context.Context, sbx *agentsv1alpha1.Sandbox, ops *agentsv1alpha1.SandboxUpdateOps) error {
	modified := sbx.DeepCopy()

	// 1. Apply template patch (Strategic Merge Patch)
	if len(ops.Spec.Patch.Raw) > 0 && modified.Spec.Template != nil {
		merged, err := mergeTemplateWithPatch(modified, ops)
		if err != nil {
			return err
		}
		modified.Spec.Template = merged
	}

	// 2. Set UpgradePolicy based on strategy type
	switch ops.Spec.UpdateStrategy.Type {
	case agentsv1alpha1.SandboxUpdateOpsStrategyInplaceUpdate:
		// InplaceUpdate keeps the pod but still runs the upgrade lifecycle, so the
		// sandbox needs an explicit policy. Leaving the policy unset would instead
		// select the SandboxClaim in-place path, which stays in Running and is not
		// observable as an upgrade.
		modified.Spec.UpgradePolicy = &agentsv1alpha1.SandboxUpgradePolicy{
			Type: agentsv1alpha1.SandboxUpgradePolicyInplaceUpdate,
		}
	case agentsv1alpha1.SandboxUpdateOpsStrategyCheckpointRestore:
		modified.Spec.UpgradePolicy = &agentsv1alpha1.SandboxUpgradePolicy{
			Type: agentsv1alpha1.SandboxUpgradePolicyCheckpointRestore,
		}
	default:
		modified.Spec.UpgradePolicy = &agentsv1alpha1.SandboxUpgradePolicy{
			Type: agentsv1alpha1.SandboxUpgradePolicyRecreate,
		}
	}

	// 3. Set Lifecycle
	if ops.Spec.Lifecycle != nil {
		modified.Spec.Lifecycle = ops.Spec.Lifecycle.DeepCopy()
	} else {
		modified.Spec.Lifecycle = nil
	}

	// 4. Add tracking label and clear stale upgrade-failed label
	if modified.Labels == nil {
		modified.Labels = map[string]string{}
	}
	modified.Labels[agentsv1alpha1.LabelSandboxUpdateOps] = ops.Name
	delete(modified.Labels, agentsv1alpha1.LabelSandboxUpgradeFailed)

	// 5. Patch the sandbox
	patch := client.MergeFrom(sbx)
	patchData, patchErr := patch.Data(modified)
	if patchErr != nil {
		klog.ErrorS(patchErr, "Failed to compute patch data", "sandbox", klog.KObj(sbx))
	} else {
		klog.InfoS("Applying sandbox patch", "sandbox", klog.KObj(sbx), "patch", string(patchData))
	}
	if err := r.Patch(ctx, modified, patch); err != nil {
		klog.ErrorS(err, "Failed to patch sandbox", "sandbox", klog.KObj(sbx))
		return err
	}
	klog.InfoS("Successfully patched sandbox", "sandbox", klog.KObj(sbx))
	ResourceVersionExpectations.Expect(modified)
	return nil
}
