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

const SandboxTemplateClaimCleanupFinalizer = "agents.kruise.io/template-claim-cleanup"

// +genclient
// +genclient:noStatus
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=sandboxtemplateclaims,scope=Namespaced,singular=sandboxtemplateclaim
// +kubebuilder:storageversion
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$') && self.metadata.name != 'tags'",message="name must be a 1-63 character DNS label other than tags"
// SandboxTemplateClaim reserves a template name and owns its build Versions.
type SandboxTemplateClaim struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`
}

// +kubebuilder:object:root=true
// SandboxTemplateClaimList contains template Claims.
type SandboxTemplateClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxTemplateClaim `json:"items"`
}

var SandboxTemplateClaimControllerKind = GroupVersion.WithKind("SandboxTemplateClaim")

func init() {
	SchemeBuilder.Register(&SandboxTemplateClaim{}, &SandboxTemplateClaimList{})
}
