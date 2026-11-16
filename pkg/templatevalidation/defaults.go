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

// Package templatevalidation holds pure template defaulting and validation.
// It depends neither on API servers/Manager nor on controller/admission state.
package templatevalidation

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	nativev1 "k8s.io/kubernetes/pkg/apis/core/v1"
	"k8s.io/utils/ptr"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/defaults"
)

// DefaultPodTemplate retains the existing admission defaults/security setting.
func DefaultPodTemplate(template *corev1.PodTemplateSpec) {
	if template == nil {
		return
	}
	if ptr.Deref(template.Spec.AutomountServiceAccountToken, true) {
		template.Spec.AutomountServiceAccountToken = ptr.To(false)
	}
	defaults.SetDefaultPodSpec(&template.Spec)
}

// DefaultVolumeClaimTemplates applies the existing PVC-template defaults.
func DefaultVolumeClaimTemplates(templates []corev1.PersistentVolumeClaim) {
	for i := range templates {
		if len(templates[i].Spec.AccessModes) == 0 {
			templates[i].Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
		}
		if templates[i].Spec.VolumeMode == nil {
			templates[i].Spec.VolumeMode = ptr.To(corev1.PersistentVolumeFilesystem)
		}
	}
}

// DefaultSpec mutates caller-owned data only, preserving the native webhook path.
func DefaultSpec(spec *api.SandboxTemplateSpec) {
	if spec == nil {
		return
	}
	DefaultPodTemplate(spec.Template)
	DefaultVolumeClaimTemplates(spec.VolumeClaimTemplates)
}

// NormalizeCustomSpec validates declaration presence before filling defaults and
// never mutates the input (which may have come from an informer). Wire-only
// unknown keys/explicit nulls must additionally be checked in the API DTO layer.
func NormalizeCustomSpec(input *api.SandboxTemplateSpec, path *field.Path) (*api.SandboxTemplateSpec, field.ErrorList) {
	if input == nil {
		return nil, field.ErrorList{field.Required(path, "spec is required")}
	}
	if errs := ValidateBuildOptions(input.BuildOptions, path.Child("buildOptions")); len(errs) != 0 {
		return nil, errs
	}
	normalized := input.DeepCopy()
	if normalized.Backend == "" {
		normalized.Backend = api.SandboxTemplateBackendKubernetes
	}
	DefaultSpec(normalized)
	for i := range normalized.Probes {
		nativev1.SetDefaults_Probe(&normalized.Probes[i].Probe)
	}
	if normalized.Lifecycle != nil {
		for _, action := range []*api.LifecycleAction{normalized.Lifecycle.PrePause, normalized.Lifecycle.PostResume} {
			if action != nil && action.TimeoutSeconds == 0 {
				action.TimeoutSeconds = 60
			}
		}
	}
	if normalized.BuildOptions == nil {
		normalized.BuildOptions = &api.SandboxTemplateBuildOptions{}
	}
	if normalized.BuildOptions.WarmPool == nil {
		normalized.BuildOptions.WarmPool = &api.SandboxTemplateWarmPoolOptions{}
	}
	warm := normalized.BuildOptions.WarmPool
	if warm.Enabled == nil {
		warm.Enabled = ptr.To(true)
	}
	if *warm.Enabled && warm.Size == nil {
		warm.Size = ptr.To(int32(0))
	}
	if errs := ValidateCustomSpec(*normalized, path); len(errs) != 0 {
		return nil, errs
	}
	return normalized, nil
}
