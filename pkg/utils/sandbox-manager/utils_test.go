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

package utils

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

func TestCalculateResourceFromContainers(t *testing.T) {
	cpuQuantity1, _ := resource.ParseQuantity("1000m")
	cpuQuantity2, _ := resource.ParseQuantity("500m")
	memoryQuantity1, _ := resource.ParseQuantity("1024Mi")
	memoryQuantity2, _ := resource.ParseQuantity("512Mi")

	tests := []struct {
		name string
		pod  *corev1.Pod
		want infra.SandboxResource
	}{
		{
			name: "single container with resources",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQuantity1,
									corev1.ResourceMemory: memoryQuantity1,
								},
							},
						},
					},
				},
			},
			want: infra.SandboxResource{
				CPUMilli: 1000,
				MemoryMB: 1024,
			},
		},
		{
			name: "multiple containers with resources",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQuantity1,
									corev1.ResourceMemory: memoryQuantity1,
								},
							},
						},
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQuantity2,
									corev1.ResourceMemory: memoryQuantity2,
								},
							},
						},
					},
				},
			},
			want: infra.SandboxResource{
				CPUMilli: 1500,
				MemoryMB: 1536,
			},
		},
		{
			name: "no containers",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{},
				},
			},
			want: infra.SandboxResource{
				CPUMilli: 0,
				MemoryMB: 0,
			},
		},
		{
			name: "containers without resources",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{},
							},
						},
					},
				},
			},
			want: infra.SandboxResource{
				CPUMilli: 0,
				MemoryMB: 0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateResourceFromContainers(tt.pod.Spec.Containers)
			if got.CPUMilli != tt.want.CPUMilli {
				t.Errorf("GetResource().CPUMilli = %v, want %v", got.CPUMilli, tt.want.CPUMilli)
			}
			if got.MemoryMB != tt.want.MemoryMB {
				t.Errorf("GetResource().MemoryMB = %v, want %v", got.MemoryMB, tt.want.MemoryMB)
			}
		})
	}
}
