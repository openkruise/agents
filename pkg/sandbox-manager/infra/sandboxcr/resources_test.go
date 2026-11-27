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

package sandboxcr

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

func TestTemplateResourceFromSandboxSet(t *testing.T) {
	claim := func(size string) corev1.PersistentVolumeClaim {
		pvc := corev1.PersistentVolumeClaim{}
		pvc.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}
		return pvc
	}

	tests := []struct {
		name       string
		containers []corev1.Container
		claims     []corev1.PersistentVolumeClaim
		want       infra.ResourceList
	}{
		{
			name: "cpu limits with memory and ephemeral storage requests exclude PVC capacity",
			containers: []corev1.Container{
				{Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:              resource.MustParse("250m"),
						corev1.ResourceMemory:           resource.MustParse("512Mi"),
						corev1.ResourceEphemeralStorage: resource.MustParse("4Gi"),
					},
					Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				}},
				{Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				}},
			},
			claims: []corev1.PersistentVolumeClaim{claim("1Gi"), {}, claim("2Gi")},
			want:   infra.ResourceList{CPUMilli: 1500, MemoryMB: 512, DiskSizeMB: 4096},
		},
		{
			name: "cpu request with memory and disk limits",
			containers: []corev1.Container{{Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:              resource.MustParse("500m"),
					corev1.ResourceMemory:           resource.MustParse("256Mi"),
					corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceMemory:           resource.MustParse("512Mi"),
					corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
				},
			}}},
			want: infra.ResourceList{CPUMilli: 500, MemoryMB: 512, DiskSizeMB: 2048},
		},
		{
			name:   "nil pod template skips claims",
			claims: []corev1.PersistentVolumeClaim{claim("1Gi")},
			want:   infra.ResourceList{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := &agentsv1alpha1.SandboxSet{}
			if tt.containers != nil {
				set.Spec.Template = &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: tt.containers}}
			}
			set.Spec.VolumeClaimTemplates = tt.claims
			assert.Equal(t, tt.want, templateResourceFromSandboxSet(set))
		})
	}
}
