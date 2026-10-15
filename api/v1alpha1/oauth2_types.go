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

import "k8s.io/apimachinery/pkg/runtime"

// OAuth2Config selects an OAuth endpoint independently of its URL or provider.
type OAuth2Config struct {
	// Operation contains exactly one endpoint role and its options.
	Operation OAuth2Operation `json:"operation"`
	// CustomResponse adds public application fields to generated OAuth replies.
	// It cannot replace standard OAuth fields or change upstream resource replies.
	// +optional
	CustomResponse *OAuth2CustomResponse `json:"customResponse,omitempty"`
}

// OAuth2Operation selects exactly one endpoint role. Empty role objects are valid.
// These roles describe policy behavior, not OAuth grant types or service RPCs.
type OAuth2Operation struct {
	// DeviceAuthorization creates a device authorization session.
	// +optional
	DeviceAuthorization *OAuth2DeviceAuthorizationConfig `json:"deviceAuthorization,omitempty"`
	// Token processes token grants and returns placeholder credentials.
	// +optional
	Token *OAuth2TokenConfig `json:"token,omitempty"`
	// Resource injects a service-issued Bearer credential into the request.
	// +optional
	Resource *OAuth2ResourceConfig `json:"resource,omitempty"`
}

// OAuth2DeviceAuthorizationConfig selects device authorization, including an
// existing authorized session confirmed by the credential service.
type OAuth2DeviceAuthorizationConfig struct{}

// OAuth2TokenConfig configures the token endpoint. Form and JSON requests are
// decoded according to Content-Type; no request format is configured in policy.
type OAuth2TokenConfig struct {
	// AllowedGrantTypes optionally restricts requests to exact grant_type values,
	// including extension grant URIs. Omitted or empty adds no policy restriction;
	// protocol support and service authorization still apply.
	// +optional
	// +listType=set
	// +kubebuilder:validation:items:MinLength=1
	AllowedGrantTypes []string `json:"allowedGrantTypes,omitempty"`
}

// OAuth2ResourceConfig selects Bearer injection without parsing the request body.
type OAuth2ResourceConfig struct{}

// OAuth2CustomResponse adds literal public JSON objects to EPE-generated replies.
// Standard OAuth fields cannot be overridden. These fields do not select credentials.
type OAuth2CustomResponse struct {
	// Success adds fields to a successful token reply. Only Token supports this;
	// device authorization success and upstream resource replies are unchanged.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Success *runtime.RawExtension `json:"success,omitempty"`
	// Error adds fields to an OAuth error reply for any operation.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Error *runtime.RawExtension `json:"error,omitempty"`
	// AuthorizationRequired adds fields when the service confirms a
	// non-retryable need for new authorization. Its fields override matching
	// application fields in Error, while standard OAuth fields remain protected.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	AuthorizationRequired *runtime.RawExtension `json:"authorizationRequired,omitempty"`
}
