/*
Copyright 2025.

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

package core

import (
	"encoding/json"
	"fmt"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	corev1 "k8s.io/api/core/v1"
)

// HashSandbox calculates the hash value using sandbox.spec.template
func HashSandbox(box *agentsv1alpha1.Sandbox) (string, string) {
	// hash using sandbox.spec.template
	by, _ := json.Marshal(*box.Spec.Template)
	hash := utils.HashData(by)

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
	hashWithoutImageResources := utils.HashData(by)
	return hash, hashWithoutImageResources
}

// GeneratePVCName generates a persistent volume claim name from template name and sandbox name
func GeneratePVCName(templateName, sandboxName string) (string, error) {
	if templateName == "" || sandboxName == "" {
		return "", fmt.Errorf("template name and sandbox name cannot be empty")
	}

	name := fmt.Sprintf("%s-%s", templateName, sandboxName)

	return name, nil
}
