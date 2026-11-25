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

package sandboxcr

import (
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/lifecycle/claim"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

const bytesPerMiB = int64(1024 * 1024)

// templateResourceFromSandboxSet calculates the resource view of a template,
// including the storage requested by its volume claim templates, which stacks
// on top of the effective container footprint.
func templateResourceFromSandboxSet(tmpl *agentsv1alpha1.SandboxSet) infra.ResourceList {
	if tmpl == nil || tmpl.Spec.Template == nil {
		return infra.ResourceList{}
	}
	out := claim.CalculateResourceFromContainers(tmpl.Spec.Template.Spec.Containers).Effective()
	for _, pvc := range tmpl.Spec.VolumeClaimTemplates {
		if storage := pvc.Spec.Resources.Requests.Storage(); storage != nil && !storage.IsZero() {
			out.DiskSizeMB += storage.Value() / bytesPerMiB
		}
	}
	return out
}
