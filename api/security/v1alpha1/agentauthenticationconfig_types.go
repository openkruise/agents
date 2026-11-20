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

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// JWTAuthentication describes a trusted OIDC issuer.
//
// The issuer and jwks_uri are not configured here: the controller reads them
// from the discovery document, so a caller cannot point an issuer name at
// someone else's keys.
type JWTAuthentication struct {
	// DiscoveryURL is the OIDC discovery document of the issuer. It must be an
	// absolute HTTPS URL.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https://`
	DiscoveryURL string `json:"discoveryUrl"`

	// AllowedAudience lists the audiences accepted in an ID token. An ID token
	// naming none of these is rejected, which is what stops a token minted for
	// another application being replayed here.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=set
	AllowedAudience []string `json:"allowedAudience"`
}

// AgentAuthenticationConfigSpec declares a trusted end-user identity provider.
//
// The CEL rule pairs the issuer type with its configuration block, the same
// way CredentialProviderSpec pairs its own type with its config block. A
// config declaring type "jwt" but carrying no jwt block cannot authenticate
// anything, and failing that at admission is better than failing the first
// time an end user signs in.
//
// +kubebuilder:validation:XValidation:rule="self.type != 'jwt' || has(self.jwt)",message="jwt is required when type is jwt"
type AgentAuthenticationConfigSpec struct {
	// Type selects how an issuer proves an end-user identity. The only
	// supported value is "jwt": an OIDC ID token validated against a
	// discovered issuer and JWKS.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=jwt
	Type string `json:"type"`

	// JWT configures the trusted OIDC issuer. Required when Type is JWT.
	// +optional
	JWT *JWTAuthentication `json:"jwt,omitempty"`
}

// AgentAuthenticationConfigStatus reports whether the issuer is usable.
type AgentAuthenticationConfigStatus struct {
	// ObservedGeneration is the most recent generation observed by the
	// controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Issuer is the issuer identifier read from the discovery document. It is
	// resolved rather than configured, and an ID token must match it exactly.
	// +optional
	Issuer string `json:"issuer,omitempty"`

	// JWKSURI is the key set location read from the discovery document.
	// +optional
	JWKSURI string `json:"jwksUri,omitempty"`

	// Conditions holds the observations of the config's state. Ready is True
	// when the current generation validated and discovery succeeded.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=aac,categories=agents
// +kubebuilder:printcolumn:name="Issuer",type=string,JSONPath=`.status.issuer`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentAuthenticationConfig declares an OIDC issuer trusted to authenticate end
// users for the agents in its namespace.
type AgentAuthenticationConfig struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the trusted issuer.
	Spec AgentAuthenticationConfigSpec `json:"spec"`

	// Status is the current state of the issuer.
	// +optional
	Status AgentAuthenticationConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentAuthenticationConfigList contains a list of AgentAuthenticationConfig.
type AgentAuthenticationConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentAuthenticationConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentAuthenticationConfig{}, &AgentAuthenticationConfigList{})
}
