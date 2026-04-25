package utils

import (
	"encoding/json"

	corev1 "k8s.io/api/core/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// HashSandbox calculates the hash value using sandbox.spec.template
func HashSandbox(box *agentsv1alpha1.Sandbox) (string, string) {
	if box.Spec.Template == nil {
		return "", ""
	}
	// hash using sandbox.spec.template
	by, _ := json.Marshal(*box.Spec.Template)
	hash := HashData(by)

	// hash using sandbox.spec.template without image and resources
	tempClone := box.Spec.Template.DeepCopy()
	for i := range tempClone.Spec.Containers {
		container := &tempClone.Spec.Containers[i]
		container.Image = ""
		container.Resources = corev1.ResourceRequirements{}
	}
	for i := range tempClone.Spec.InitContainers {
		container := &tempClone.Spec.InitContainers[i]
		container.Image = ""
		container.Resources = corev1.ResourceRequirements{}
	}
	by, _ = json.Marshal(*tempClone)
	hashWithoutImageResources := HashData(by)
	return hash, hashWithoutImageResources
}
