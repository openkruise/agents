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

import corev1 "k8s.io/api/core/v1"

type InitRuntimeOptions struct {
	EnvVars     map[string]string `json:"envVars,omitempty"`
	AccessToken string            `json:"accessToken,omitempty"`
	ReInit      bool              `json:"-"`
	SkipRefresh bool              `json:"skipRefresh,omitempty"`
}

const DefaultCSIMountConcurrency = 3

type CSIMountOptions struct {
	MountOptionList    []MountConfig `json:"mountOptionList"`
	MountOptionListRaw string        `json:"mountOptionListRaw"`    // the raw json string for mount options
	Concurrency        int           `json:"concurrency,omitempty"` // max concurrent CSI mount operations, 0 or negative means unlimited, default is DefaultCSIMountConcurrency
}

type MountConfig struct {
	Driver     string `json:"driver"`
	RequestRaw string `json:"requestRaw"`
}

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
