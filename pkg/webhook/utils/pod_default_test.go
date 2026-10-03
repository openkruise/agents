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
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func TestSetDefaultPodTemplate(t *testing.T) {
	t.Run("nil template does nothing", func(t *testing.T) {
		SetDefaultPodTemplate(nil)
	})

	t.Run("nil automount token defaults to false and applies pod defaults", func(t *testing.T) {
		template := &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "c", Image: "nginx:latest"}},
			},
		}
		SetDefaultPodTemplate(template)
		assert.Equal(t, ptr.To(false), template.Spec.AutomountServiceAccountToken)
		assert.NotEmpty(t, template.Spec.DNSPolicy)
		assert.NotEmpty(t, template.Spec.RestartPolicy)
		assert.NotNil(t, template.Spec.TerminationGracePeriodSeconds)
		assert.NotEmpty(t, template.Spec.Containers[0].ImagePullPolicy)
		assert.NotEmpty(t, template.Spec.Containers[0].TerminationMessagePolicy)
	})

	t.Run("true automount token defaults to false", func(t *testing.T) {
		template := &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				AutomountServiceAccountToken: ptr.To(true),
				Containers:                   []corev1.Container{{Name: "c", Image: "nginx:latest"}},
			},
		}
		SetDefaultPodTemplate(template)
		assert.Equal(t, ptr.To(false), template.Spec.AutomountServiceAccountToken)
	})

	t.Run("explicit false automount token is preserved", func(t *testing.T) {
		template := &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				AutomountServiceAccountToken: ptr.To(false),
				Containers:                   []corev1.Container{{Name: "c", Image: "nginx:latest"}},
			},
		}
		SetDefaultPodTemplate(template)
		assert.Equal(t, ptr.To(false), template.Spec.AutomountServiceAccountToken)
	})
}

func TestSetDefaultVolumeClaimTemplates(t *testing.T) {
	t.Run("nil slice is safe", func(t *testing.T) {
		SetDefaultVolumeClaimTemplates(nil)
	})

	t.Run("empty slice is safe", func(t *testing.T) {
		SetDefaultVolumeClaimTemplates([]corev1.PersistentVolumeClaim{})
	})

	t.Run("missing access mode and volume mode are defaulted", func(t *testing.T) {
		claims := []corev1.PersistentVolumeClaim{{}}
		SetDefaultVolumeClaimTemplates(claims)
		assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, claims[0].Spec.AccessModes)
		assert.Equal(t, corev1.PersistentVolumeFilesystem, *claims[0].Spec.VolumeMode)
	})

	t.Run("explicit access mode is preserved", func(t *testing.T) {
		claims := []corev1.PersistentVolumeClaim{{
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			},
		}}
		SetDefaultVolumeClaimTemplates(claims)
		assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, claims[0].Spec.AccessModes)
		assert.Equal(t, corev1.PersistentVolumeFilesystem, *claims[0].Spec.VolumeMode)
	})

	t.Run("explicit volume mode is preserved", func(t *testing.T) {
		block := corev1.PersistentVolumeBlock
		claims := []corev1.PersistentVolumeClaim{{
			Spec: corev1.PersistentVolumeClaimSpec{
				VolumeMode: &block,
			},
		}}
		SetDefaultVolumeClaimTemplates(claims)
		assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, claims[0].Spec.AccessModes)
		assert.Equal(t, corev1.PersistentVolumeBlock, *claims[0].Spec.VolumeMode)
	})
}
