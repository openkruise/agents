// Package utils provides utility functions for parsing and handling Kubernetes resource quantities.
//
//nolint:revive // Package name is acceptable for this utility package
package utils

import (
	"github.com/google/uuid"
	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils/expectations"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// resourceVersionExpectation usage:
// 1. Observes in utils.SelectObjectWithIndex, which is the final step of selecting any object from informer.
// 2. Expects when sandbox state changes, including claim, pause, resume, delete, etc.
// 3. Always check satisfied after select.
// 4. Use functions like ResourceVersionExpectationSatisfied, don't call IsSatisfied directly.
var resourceVersionExpectation = expectations.NewResourceVersionExpectation()

func ResourceVersionExpectationObserve(obj metav1.Object) {
	resourceVersionExpectation.Observe(obj)
}

func ResourceVersionExpectationExpect(obj metav1.Object) {
	resourceVersionExpectation.Expect(obj)
}

func ResourceVersionExpectationDelete(obj metav1.Object) {
	resourceVersionExpectation.Delete(obj)
}

func ResourceVersionExpectationSatisfied(obj metav1.Object) bool {
	satisfied, sinceFirstUnsatisfied := resourceVersionExpectation.IsSatisfied(obj)
	if sinceFirstUnsatisfied > expectations.ExpectationTimeout {
		ResourceVersionExpectationDelete(obj)
		return true
	}
	return satisfied
}

func NewLockString() string {
	return uuid.NewString()
}
