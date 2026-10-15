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

// Actions an AgentRole may permit.
const (
	// ActionExchangePrincipalToken permits exchanging a user ID token and an
	// agent token for a principal token. Its resource names the AgentIdentity
	// being exchanged for, or "*" for any identity in the namespace.
	ActionExchangePrincipalToken = "ExchangePrincipalToken"

	// ActionGetResourceOAuth2Token permits obtaining a third-party resource
	// token, or starting the authorization session that leads to one. Its
	// resource must name one CredentialProvider exactly: a wildcard here would
	// grant access to every third-party credential in the namespace, including
	// ones added later.
	ActionGetResourceOAuth2Token = "GetResourceOAuth2Token"
)

// AgentRoleRule permits a set of actions on a set of resources.
//
// A resource is either "*" or "<Kind>/<name>", naming what the action applies
// to. Which kinds are meaningful depends on the action: ExchangePrincipalToken
// acts on an AgentIdentity, GetResourceOAuth2Token on a CredentialProvider.
type AgentRoleRule struct {
	// Actions permitted by this rule.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:Enum=ExchangePrincipalToken;GetResourceOAuth2Token
	// +listType=set
	Actions []string `json:"actions"`

	// Resources the actions apply to, as "*" or "<Kind>/<name>".
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=317
	// +listType=set
	Resources []string `json:"resources"`
}

// AgentRoleSpec declares what an agent is permitted to do.
//
// Rules are Allow-only and there is no deny form. A request that matches no
// rule is denied, so permission comes only from something having granted it
// rather than from nothing having forbidden it.
type AgentRoleSpec struct {
	// Rules granting permissions. An empty list is a role that permits nothing,
	// which is valid but useless, so at least one is required to make an
	// accidentally empty role visible at admission instead of at request time.
	//
	// The CEL rule enforces the one constraint that cannot be expressed per
	// field: GetResourceOAuth2Token must name a provider exactly, because a
	// wildcard would also grant every CredentialProvider added in future.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	// +kubebuilder:validation:XValidation:rule="self.all(r, !('GetResourceOAuth2Token' in r.actions) || !r.resources.exists(x, x.contains('*')))",message="GetResourceOAuth2Token must name a CredentialProvider exactly and cannot use a wildcard"
	Rules []AgentRoleRule `json:"rules"`
}

// AgentRoleStatus reports whether the role is usable.
type AgentRoleStatus struct {
	// ObservedGeneration is the most recent generation observed by the
	// controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the observations of the role's state. Ready is True when
	// the current generation validated.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=arole,categories=agents
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentRole declares a set of permitted identity and credential actions.
//
// A role grants nothing on its own. An AgentRoleBinding is what attaches it to
// an AgentIdentity, and deleting that binding withdraws the permissions from
// the next request onward.
type AgentRole struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the permitted actions.
	Spec AgentRoleSpec `json:"spec"`

	// Status is the current state of the role.
	// +optional
	Status AgentRoleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentRoleList contains a list of AgentRole.
type AgentRoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentRole `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentRole{}, &AgentRoleList{})
}
