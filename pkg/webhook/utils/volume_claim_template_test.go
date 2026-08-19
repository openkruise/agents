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

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestValidateVolumeClaimTemplateMounts(t *testing.T) {
	tests := []struct {
		name         string
		template     *corev1.PodTemplateSpec
		claims       []corev1.PersistentVolumeClaim
		expectError  bool
		errorMessage string
	}{
		{
			name: "mounted by init container",
			template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{
							Name: "init",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "data-vol", MountPath: "/data"},
							},
						},
					},
				},
			},
			claims: []corev1.PersistentVolumeClaim{
				{ObjectMeta: metav1.ObjectMeta{Name: "data-vol"}},
			},
			expectError: false,
		},
		{
			name: "mounted by ephemeral container",
			template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					EphemeralContainers: []corev1.EphemeralContainer{
						{
							EphemeralContainerCommon: corev1.EphemeralContainerCommon{
								Name: "debugger",
								VolumeMounts: []corev1.VolumeMount{
									{Name: "data-vol", MountPath: "/data"},
								},
							},
						},
					},
				},
			},
			claims: []corev1.PersistentVolumeClaim{
				{ObjectMeta: metav1.ObjectMeta{Name: "data-vol"}},
			},
			expectError: false,
		},
		{
			name:     "empty template name is skipped",
			template: &corev1.PodTemplateSpec{},
			claims: []corev1.PersistentVolumeClaim{
				{},
			},
			expectError: false,
		},
		{
			name:     "unmounted volume claim template is rejected",
			template: &corev1.PodTemplateSpec{},
			claims: []corev1.PersistentVolumeClaim{
				{ObjectMeta: metav1.ObjectMeta{Name: "data-vol"}},
			},
			expectError:  true,
			errorMessage: "must be mounted by at least one container",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errList := ValidateVolumeClaimTemplateMounts(tt.template, tt.claims, field.NewPath("spec"))

			if tt.expectError {
				require.NotEmpty(t, errList)
				require.Contains(t, errList.ToAggregate().Error(), tt.errorMessage)
				return
			}
			require.Empty(t, errList)
		})
	}
}

func TestAppendVolumeClaimTemplateVolumes(t *testing.T) {
	template := &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "existing"},
			},
		},
	}
	claims := []corev1.PersistentVolumeClaim{
		{ObjectMeta: metav1.ObjectMeta{Name: "data-vol"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "cache-vol"}},
	}

	AppendVolumeClaimTemplateVolumes(template, claims)

	require.Equal(t, []corev1.Volume{
		{Name: "existing"},
		{
			Name: "data-vol",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: "data-vol",
				},
			},
		},
		{
			Name: "cache-vol",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: "cache-vol",
				},
			},
		},
	}, template.Spec.Volumes)
}
