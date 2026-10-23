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

package util

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/openkruise/agents/pkg/utils/defaults"
)

// SetDefaultPodTemplate applies Kubernetes PodSpec defaults to the template and
// disables automatic service account token mounting for Sandboxes.
func SetDefaultPodTemplate(template *corev1.PodTemplateSpec) {
	if template == nil {
		return
	}
	if ptr.Deref(template.Spec.AutomountServiceAccountToken, true) {
		template.Spec.AutomountServiceAccountToken = ptr.To(false)
	}
	defaults.SetDefaultPodSpec(&template.Spec)
}

// SetDefaultVolumeClaimTemplates applies default access modes and volume mode
// to persistent volume claim templates.
func SetDefaultVolumeClaimTemplates(templates []corev1.PersistentVolumeClaim) {
	for i := range templates {
		vct := &templates[i]
		if len(vct.Spec.AccessModes) == 0 {
			vct.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
		}
		if vct.Spec.VolumeMode == nil {
			volumeMode := corev1.PersistentVolumeFilesystem
			vct.Spec.VolumeMode = &volumeMode
		}
	}
}
