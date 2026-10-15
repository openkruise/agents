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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

// NewSandboxFromSandboxSet builds a Sandbox object from the SandboxSet. When
// spec.templateRef is used, refTemplate must be the resolved SandboxTemplate.
// Its PodTemplateSpec is copied into the new Sandbox; callers pass nil for the
// inline template case.
func NewSandboxFromSandboxSet(sbs *agentsv1alpha1.SandboxSet, refTemplate *agentsv1alpha1.SandboxTemplate) *agentsv1alpha1.Sandbox {
	generateName := utils.GenerateSandboxName(sbs.Name)
	templateName := sbs.Name
	var template *corev1.PodTemplateSpec
	var inheritedLabels, inheritedAnnotations map[string]string
	// spec.template takes precedence over spec.templateRef. Deep copy the source
	// pod template before reading labels/annotations so subsequent mutations
	// (clearAndInitInnerKeys, internal label writes) never leak back into the
	// SandboxSet spec or the cached SandboxTemplate.
	// The metadata maps are cloned (not shared) so metadata-only internal labels
	// do not leak into the Pod template. Pool and template identity labels are
	// added to the materialized Pod template explicitly below.
	if sbs.Spec.Template != nil {
		template = sbs.Spec.Template.DeepCopy()
		inheritedLabels = maps.Clone(template.Labels)
		inheritedAnnotations = maps.Clone(template.Annotations)
	} else if refTemplate != nil && refTemplate.Spec.Template != nil {
		template = refTemplate.Spec.Template.DeepCopy()
		templateName = refTemplate.Name
		inheritedLabels = maps.Clone(template.Labels)
		inheritedAnnotations = maps.Clone(template.Annotations)
	}

	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: generateName,
			Namespace:    sbs.Namespace,
			Labels:       inheritedLabels,
			Annotations:  inheritedAnnotations,
		},
		Spec: agentsv1alpha1.SandboxSpec{
			PersistentContents: sbs.Spec.PersistentContents,
			Runtimes:           sbs.Spec.Runtimes,
			PauseStrategy:      sbs.Spec.PauseStrategy,
			Probes:             sbs.Spec.Probes,
			// Deep copy as well: claim-time wake-rule adjustments write into the
			// sandbox's policy and must not reach the cached SandboxSet.
			AutoPausePolicy: sbs.Spec.AutoPausePolicy.DeepCopy(),
			Lifecycle:       sbs.Spec.Lifecycle.DeepCopy(),
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template:             template,
				VolumeClaimTemplates: sbs.Spec.VolumeClaimTemplates,
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

	return sbx
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
