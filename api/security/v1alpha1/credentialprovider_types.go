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

// CallbackPath is appended to a provider's callbackBaseUrl to form the redirect
// URI registered with the third party.
//
// It is fixed rather than configurable because the third-party OAuth app
// registration has to match the callback exactly. A per-provider path would
// mean a separate app registration for every provider in every namespace.
const CallbackPath = "/oauth2/callback"

// SecretKeySelector names one key of a Secret in the same namespace.
//
// Cross-namespace references are not supported: a namespace must not be able to
// borrow a client secret it does not control.
type SecretKeySelector struct {
	// Name of the Secret in the same namespace.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Key within that Secret holding the value.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key"`
}

// SecretKeyReference wraps a Secret key selector.
//
// The indirection matches the shape in the proposal and leaves room for other
// sources later without changing the field that callers already write.
type SecretKeyReference struct {
	// SecretKeyRef names the Secret and key holding the value.
	// +kubebuilder:validation:Required
	SecretKeyRef SecretKeySelector `json:"secretKeyRef"`
}

// OAuth2Discovery is where the third party's OAuth endpoints live.
//
// These are configured rather than discovered, unlike the OIDC issuer on
// AgentAuthenticationConfig. Most OAuth2 providers publish no discovery
// document, so there is nothing to resolve them from.
type OAuth2Discovery struct {
	// Issuer identifies the provider.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https://`
	Issuer string `json:"issuer"`

	// AuthorizationEndpoint is where the user is sent to approve access.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https://`
	AuthorizationEndpoint string `json:"authorizationEndpoint"`

	// TokenEndpoint is where an authorization code is exchanged for a token.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https://`
	TokenEndpoint string `json:"tokenEndpoint"`
}

// OAuth2Client is the registered application's credentials.
type OAuth2Client struct {
	// ClientID of the registered application. Not a secret.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	ClientID string `json:"clientId"`

	// ClientSecret references the Secret holding the client secret. It is a
	// reference rather than a value so the secret never appears in this object,
	// which is readable by anything that can read the CRD.
	// +kubebuilder:validation:Required
	ClientSecret SecretKeyReference `json:"clientSecret"`
}

// OAuth2UserFederation configures the flow where a user approves an agent's
// access to their own third-party account.
type OAuth2UserFederation struct {
	// CallbackBaseURL is the externally reachable base the callback is served
	// under. The controller appends CallbackPath to form status.callbackUrl,
	// which is what gets registered with the third party.
	//
	// A query, fragment or user information here would produce a redirect URI
	// that cannot match the registration, so they are rejected rather than
	// silently dropped.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https://[^/?#@]+(/[^?#]*)?$`
	// +kubebuilder:validation:MaxLength=2048
	CallbackBaseURL string `json:"callbackBaseUrl"`

	// DefaultScopes are requested when a caller names none. A caller may request
	// a subset but never more, so this is the ceiling on what any agent in this
	// namespace can be granted through this provider.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=256
	// +listType=set
	DefaultScopes []string `json:"defaultScopes"`
}

// OAuth2Provider is the OAuth2 half of a CredentialProvider spec.
//
// The CEL rule pairs the grant type with its configuration block. userFederation
// is the only grant type, so without the rule every provider could be created
// without the block describing the flow it says it uses, and the omission would
// only surface the first time an agent needed a credential.
//
// +kubebuilder:validation:XValidation:rule="self.grantType != 'userFederation' || has(self.userFederation)",message="userFederation is required when grantType is userFederation"
type OAuth2Provider struct {
	// GrantType selects the flow. The only supported value is "userFederation":
	// a user approving an agent's access to their own account.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=userFederation
	GrantType string `json:"grantType"`

	// Discovery locates the provider's OAuth endpoints.
	// +kubebuilder:validation:Required
	Discovery OAuth2Discovery `json:"discovery"`

	// Client is the registered application.
	// +kubebuilder:validation:Required
	Client OAuth2Client `json:"client"`

	// UserFederation configures the approval flow. Required when GrantType is
	// userFederation.
	// +optional
	UserFederation *OAuth2UserFederation `json:"userFederation,omitempty"`
}

// CredentialProviderSpec declares a third-party OAuth client.
//
// The CEL rule pairs the credential type with its configuration block, the same
// way OAuth2Provider pairs the grant type with its own. Admission is where that
// belongs: a provider missing the block for the mechanism it declares cannot
// serve a single request, and failing at creation is better than failing the
// first time an agent needs a credential.
//
// +kubebuilder:validation:XValidation:rule="self.type != 'oauth2' || has(self.oauth2)",message="oauth2 is required when type is oauth2"
type CredentialProviderSpec struct {
	// Type selects the credential mechanism. The only supported value is
	// "oauth2".
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=oauth2
	Type string `json:"type"`

	// OAuth2 configures the third-party OAuth client. Required when Type is
	// oauth2.
	// +optional
	OAuth2 *OAuth2Provider `json:"oauth2,omitempty"`
}

// CredentialProviderStatus reports whether the provider is usable.
type CredentialProviderStatus struct {
	// ObservedGeneration is the most recent generation observed by the
	// controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// CallbackURL is callbackBaseUrl with CallbackPath appended. It is derived
	// rather than configured so that what an operator registers with the third
	// party is exactly what the server will accept, with no chance of the two
	// drifting apart.
	// +optional
	CallbackURL string `json:"callbackUrl,omitempty"`

	// Conditions holds the observations of the provider's state. Ready is True
	// when the current generation validated and the client secret was loaded.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=acp,categories=agents
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Callback",type=string,JSONPath=`.status.callbackUrl`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CredentialProvider declares a third-party OAuth client that agents in its
// namespace may obtain user-delegated tokens through.
//
// An AgentRole grants access to a named provider; a wildcard is not accepted
// there, so adding a provider never widens an existing role.
type CredentialProvider struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the provider.
	Spec CredentialProviderSpec `json:"spec"`

	// Status is the current state of the provider.
	// +optional
	Status CredentialProviderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CredentialProviderList contains a list of CredentialProvider.
type CredentialProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CredentialProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CredentialProvider{}, &CredentialProviderList{})
}
