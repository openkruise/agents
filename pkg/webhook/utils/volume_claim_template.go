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
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func ValidateVolumeClaimTemplateMounts(template *corev1.PodTemplateSpec, claims []corev1.PersistentVolumeClaim, fldPath *field.Path) field.ErrorList {
	errList := field.ErrorList{}
	mountedVolumeNames := map[string]struct{}{}

	if template != nil {
		recordMounts := func(containers []corev1.Container) {
			for i := range containers {
				for j := range containers[i].VolumeMounts {
					mountedVolumeNames[containers[i].VolumeMounts[j].Name] = struct{}{}
				}
			}
		}
		recordMounts(template.Spec.InitContainers)
		recordMounts(template.Spec.Containers)
		for i := range template.Spec.EphemeralContainers {
			for j := range template.Spec.EphemeralContainers[i].VolumeMounts {
				mountedVolumeNames[template.Spec.EphemeralContainers[i].VolumeMounts[j].Name] = struct{}{}
			}
		}
	}

	for i, claim := range claims {
		if claim.Name == "" {
			continue
		}
		if _, mounted := mountedVolumeNames[claim.Name]; !mounted {
			errList = append(errList, field.Invalid(
				fldPath.Child("volumeClaimTemplates").Index(i).Child("metadata").Child("name"),
				claim.Name,
				"must be mounted by at least one container, init container, or ephemeral container",
			))
		}
	}
	return errList
}

func AppendVolumeClaimTemplateVolumes(template *corev1.PodTemplateSpec, claims []corev1.PersistentVolumeClaim) {
	if template == nil {
		return
	}
	for _, claim := range claims {
		template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{
			Name: claim.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: claim.Name,
				},
			},
		})
	}
}
