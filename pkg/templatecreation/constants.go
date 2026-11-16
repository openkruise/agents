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
// Package templatecreation defines durable creation vocabulary shared by the
// Kubernetes template writer and controller, with no runtime globals.
package templatecreation

import (
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/openkruise/agents/api/v1alpha1"
)

const (
	AllocationAnnotation       = "agents.kruise.io/template-allocation"
	CreationCompleteAnnotation = "agents.kruise.io/template-creation-complete"
	DefinitionCondition        = "DefinitionReady"
	DefinitionValidReason      = "ValidDefinition"
	DefinitionInvalidReason    = "InvalidDefinition"
	CreationRequestBudget      = 30 * time.Second
	EmptyCreationGrace         = 2 * CreationRequestBudget
)

// DefinitionPhase requires the phase and condition to agree on the current generation.
func DefinitionPhase(v *api.SandboxTemplate) api.SandboxTemplateBuildPhase {
	c := meta.FindStatusCondition(v.Status.Conditions, DefinitionCondition)
	if c == nil || c.ObservedGeneration != v.Generation || v.Status.ObservedGeneration != v.Generation {
		return ""
	}
	switch {
	case v.Status.Phase == api.SandboxTemplatePhaseReady && c.Status == metav1.ConditionTrue:
		return api.SandboxTemplatePhaseReady
	case v.Status.Phase == api.SandboxTemplatePhaseFailed && c.Status == metav1.ConditionFalse && c.Reason == DefinitionInvalidReason:
		return api.SandboxTemplatePhaseFailed
	}
	return ""
}
