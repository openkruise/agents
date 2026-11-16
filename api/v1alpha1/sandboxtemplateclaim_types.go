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

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// SandboxTemplateClaimCleanupFinalizer holds the logical name until owned versions finish foreground GC.
const SandboxTemplateClaimCleanupFinalizer = "agents.kruise.io/template-claim-cleanup"

// +genclient
// +genclient:noStatus
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=sandboxtemplateclaims,scope=Namespaced,singular=sandboxtemplateclaim
// +kubebuilder:storageversion
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$')",message="name must contain 1-63 lowercase letters, digits or hyphens and have alphanumeric ends"
// SandboxTemplateClaim reserves a logical template name in one namespace.
// Complete build input and observed state belong to its owned SandboxTemplate
// versions, not to this logical name. Native metadata carries identity and
// finalizer progress; there is deliberately no spec, status or default ref.
type SandboxTemplateClaim struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`
}

// +kubebuilder:object:root=true
// SandboxTemplateClaimList contains logical template names.
type SandboxTemplateClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxTemplateClaim `json:"items"`
}

var SandboxTemplateClaimControllerKind = GroupVersion.WithKind("SandboxTemplateClaim")

func init() {
	SchemeBuilder.Register(&SandboxTemplateClaim{}, &SandboxTemplateClaimList{})
}
