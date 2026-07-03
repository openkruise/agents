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

package config

import (
	corev1 "k8s.io/api/core/v1"

	runtimeconfig "github.com/openkruise/agents/pkg/utils/runtime/config"
)

// Re-exported types for backward compatibility.
// New code should import pkg/utils/runtime/config directly.
type InitRuntimeOptions = runtimeconfig.InitRuntimeOptions
type CSIMountOptions = runtimeconfig.CSIMountOptions
type MountConfig = runtimeconfig.MountConfig

const DefaultCSIMountConcurrency = runtimeconfig.DefaultCSIMountConcurrency

func NewDefaultAccessToken() string { return runtimeconfig.NewDefaultAccessToken() }

// InplaceUpdateOptions stays in pkg/sandbox-manager/config — not used by pkg/utils.
// Metadata (labels/annotations) changes are handled separately by the controller
// via isMetadataOnlyChange and do not require InplaceUpdateOptions.
type InplaceUpdateOptions struct {
	Image string
	// Resources specifies in-place resource update options.
	// +optional
	Resources *InplaceUpdateResourcesOptions `json:"resources,omitempty"`
}

type InplaceUpdateResourcesOptions struct {
	// Requests specifies the target resource requests.
	Requests corev1.ResourceList
	// Limits specifies the target resource limits.
	Limits corev1.ResourceList
}
