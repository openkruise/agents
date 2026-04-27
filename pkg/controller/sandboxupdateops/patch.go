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

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func (r *Reconciler) applySandboxPatch(ctx context.Context, sbx *agentsv1alpha1.Sandbox, ops *agentsv1alpha1.SandboxUpdateOps) error {
	modified := sbx.DeepCopy()

	// 1. Apply template patch (Strategic Merge Patch)
	// Use raw JSON bytes directly to preserve $patch directives (e.g. "$patch": "delete")
	// that would be lost if unmarshalled into a typed Go struct first.
	if len(ops.Spec.Patch.Raw) > 0 && modified.Spec.Template != nil {
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
	}

	// 2. Set UpgradePolicy to Recreate
	modified.Spec.UpgradePolicy = &agentsv1alpha1.SandboxUpgradePolicy{
		Type: agentsv1alpha1.SandboxUpgradePolicyRecreate,
	}

	// 3. Set Lifecycle
	if ops.Spec.Lifecycle != nil {
		modified.Spec.Lifecycle = ops.Spec.Lifecycle.DeepCopy()
	} else {
		modified.Spec.Lifecycle = nil
	}

	// 4. Add tracking label
	if modified.Labels == nil {
		modified.Labels = map[string]string{}
	}
	modified.Labels[agentsv1alpha1.LabelSandboxUpdateOps] = ops.Name

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
