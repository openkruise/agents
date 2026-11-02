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

// Package claim holds the neutral sandbox claim engine shared by the
// sandbox-manager E2B path and the independent SandboxClaim controller. It must
// keep a dependency closure free of pkg/sandbox-manager/** and pkg/servers/**:
// both binaries compile it directly, so any manager- or API-specific concern
// belongs at the caller's adapter boundary, not here.
package claim

import (
	corev1 "k8s.io/api/core/v1"
)

// InplaceUpdateOptions describes a claim-level in-place update of a sandbox's
// image and/or resources. It is distinct from the pod-level
// pkg/utils/inplaceupdate.InPlaceUpdateOptions and stays separate.
type InplaceUpdateOptions struct {
	Image string
	// Resources specifies in-place resource update options.
	// +optional
	Resources *InplaceUpdateResourcesOptions `json:"resources,omitempty"`
}

// InplaceUpdateResourcesOptions holds the target resource requests and limits
// for a claim-level in-place resize.
type InplaceUpdateResourcesOptions struct {
	// Requests specifies the target resource requests.
	Requests corev1.ResourceList
	// Limits specifies the target resource limits.
	Limits corev1.ResourceList
}
