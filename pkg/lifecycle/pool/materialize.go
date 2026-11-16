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

// Package pool holds neutral primitives for materializing pool Sandbox CRs from
// a SandboxSet. It is shared by the SandboxSet controller and the Sandbox CR
// infra claim path, so it must depend only on the API types and neutral
// utilities, never on controller or sandbox-manager packages.
package pool

import (
	"maps"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/templateconfig"
	"github.com/openkruise/agents/pkg/templateindex"
	"github.com/openkruise/agents/pkg/utils"
)

// NewSandboxFromSandboxSet builds a Sandbox object from the SandboxSet. When
// spec.templateRef is used, refTemplate must be the resolved SandboxTemplate.
// A resolved controller owner disambiguates companion and external references.
// Effective workload policy is shared with revision calculation; invalid managed
// relationships return an error rather than silently falling back to legacy policy.
func NewSandboxFromSandboxSet(sbs *agentsv1alpha1.SandboxSet, refTemplate, ownerTemplate *agentsv1alpha1.SandboxTemplate) (*agentsv1alpha1.Sandbox, error) {
	effective, err := templateconfig.ResolveTemplateSpec(sbs, refTemplate, ownerTemplate)
	if err != nil {
		return nil, err
	}
	generateName := utils.GenerateSandboxName(sbs.Name)
	templateName := sbs.Name
	template := effective.Template
	var inheritedLabels, inheritedAnnotations map[string]string
	if template != nil {
		inheritedLabels = maps.Clone(template.Labels)
		inheritedAnnotations = maps.Clone(template.Annotations)
	}
	// ResolveTemplateSpec has validated the complete owner/reference identity.
	// Root business metadata is inherited only for genuine custom companions;
	// legacy/external pools and the PodTemplate keep their existing behavior.
	owner := templateindex.VersionOwnerReference(sbs)
	if owner != nil && refTemplate != nil && owner.UID == refTemplate.UID && templateindex.ClaimOwnerReference(refTemplate) != nil {
		metadata := templateconfig.UserMetadata(refTemplate.Labels, refTemplate.Annotations)
		if len(metadata.Labels) > 0 {
			if inheritedLabels == nil {
				inheritedLabels = map[string]string{}
			}
			maps.Copy(inheritedLabels, metadata.Labels)
		}
		if len(metadata.Annotations) > 0 {
			if inheritedAnnotations == nil {
				inheritedAnnotations = map[string]string{}
			}
			maps.Copy(inheritedAnnotations, metadata.Annotations)
		}
	}
	if sbs.Spec.Template == nil && refTemplate != nil && refTemplate.Spec.Template != nil {
		templateName = refTemplate.Name
	}

	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: generateName,
			Namespace:    sbs.Namespace,
			Labels:       inheritedLabels,
			Annotations:  inheritedAnnotations,
		},
		Spec: agentsv1alpha1.SandboxSpec{
			PersistentContents: effective.PersistentContents,
			Runtimes:           effective.Runtimes,
			PauseStrategy:      effective.PauseStrategy,
			Probes:             effective.Probes,
			AutoPausePolicy:    effective.AutoPausePolicy,
			Lifecycle:          effective.Lifecycle.DeepCopy(),
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template:             template,
				VolumeClaimTemplates: effective.VolumeClaimTemplates,
			},
		},
	}
	sbx.Annotations = clearAndInitInnerKeys(sbx.Annotations)
	sbx.Labels = clearAndInitInnerKeys(sbx.Labels)
	sbx.Labels[agentsv1alpha1.LabelSandboxPool] = sbs.Name
	sbx.Labels[agentsv1alpha1.LabelSandboxTemplate] = templateName
	sbx.Labels[agentsv1alpha1.LabelSandboxIsClaimed] = "false"
	if sbx.Spec.Template != nil {
		if sbx.Spec.Template.Labels == nil {
			sbx.Spec.Template.Labels = map[string]string{}
		}
		sbx.Spec.Template.Labels[agentsv1alpha1.LabelSandboxTemplate] = sbx.Labels[agentsv1alpha1.LabelSandboxTemplate]
		sbx.Spec.Template.Labels[agentsv1alpha1.LabelSandboxPool] = sbx.Labels[agentsv1alpha1.LabelSandboxPool]
	}

	// A blocking, non-controller version owner makes foreground template cleanup
	// wait for undelivered inventory even after pool loss. Native claim delivery
	// removes all owners, so already-delivered Sandboxes stay independent.
	if owner != nil && refTemplate != nil && owner.UID == refTemplate.UID && templateindex.ClaimOwnerReference(refTemplate) != nil {
		sbx.OwnerReferences = append(sbx.OwnerReferences, metav1.OwnerReference{APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "SandboxTemplate", Name: refTemplate.Name, UID: refTemplate.UID, Controller: ptr.To(false), BlockOwnerDeletion: ptr.To(true)})
	}
	return sbx, nil
}

func clearAndInitInnerKeys(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	for k := range m {
		if strings.HasPrefix(k, agentsv1alpha1.InternalPrefix) {
			if _, preserve := agentsv1alpha1.InternalKeysPreservedOnCreation[k]; !preserve {
				delete(m, k)
			}
		}
	}
	return m
}
