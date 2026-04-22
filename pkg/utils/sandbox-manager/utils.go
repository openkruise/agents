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

// Package utils provides utility functions for parsing and handling Kubernetes resource quantities.
//
//nolint:revive // Package name is acceptable for this utility package
package utils

import (
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

func CalculateResourceFromContainers(containers []corev1.Container) infra.SandboxResource {
	resource := infra.SandboxResource{}
	for _, container := range containers {
		if requests := container.Resources.Requests; requests != nil {
			if cpu, ok := requests[corev1.ResourceCPU]; ok {
				// Convert CPU quantity to cores (e.g., 1000m = 1 core)
				resource.CPUMilli += cpu.MilliValue()
			}
			if memory, ok := requests[corev1.ResourceMemory]; ok {
				// Convert memory quantity to MB (1 MB = 1024 * 1024 bytes)
				resource.MemoryMB += memory.Value() / (1024 * 1024)
			}
			if disk, ok := requests[corev1.ResourceEphemeralStorage]; ok {
				// Convert disk quantity to MB (1 MB = 1024 * 1024 bytes)
				resource.DiskSizeMB += disk.Value() / (1024 * 1024)
			}
		}
	}
	return resource
}

func LockSandbox(sbx client.Object, lock string, owner string) {
	annotations := sbx.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string, 2)
	}
	annotations[v1alpha1.AnnotationLock] = lock
	annotations[v1alpha1.AnnotationOwner] = owner
	sbx.SetAnnotations(annotations)
}

func NewLockString() string {
	return uuid.NewString()
}
