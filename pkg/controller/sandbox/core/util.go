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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

// HashSandbox calculates the hash value using sandbox.spec.template
func HashSandbox(box *agentsv1alpha1.Sandbox) (string, string) {
	if box.Spec.Template == nil {
		if box.Spec.TemplateRef == nil {
			return "", ""
		}
		// templateRef mode does not carry inline PodTemplate in Sandbox spec.
		// Use TemplateRef itself as a stable revision key to avoid nil dereference.
		by, _ := json.Marshal(box.Spec.TemplateRef)
		hash := utils.HashData(by)
		return hash, hash
	}

	// hash using sandbox.spec.template
	by, _ := json.Marshal(*box.Spec.Template)
	hash := utils.HashData(by)

	// hash using sandbox.spec.template without image and resources
	tempClone := box.Spec.Template.DeepCopy()
	tempClone.Labels = nil
	tempClone.Annotations = nil
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
	hashImmutablePart := utils.HashData(by)
	return hash, hashImmutablePart
}

// GeneratePVCName generates a persistent volume claim name from template name and sandbox name
func GeneratePVCName(templateName, sandboxName string) (string, error) {
	if templateName == "" || sandboxName == "" {
		return "", fmt.Errorf("template name and sandbox name cannot be empty")
	}

	name := fmt.Sprintf("%s-%s", templateName, sandboxName)

	return name, nil
}

func GetControllerKey(obj client.Object) string {
	return types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}.String()
}

// StaleSandboxPodOwner returns the UID of the Sandbox owner reference on pod
// when it does not match the current sandbox, i.e. the pod is a leftover from
// a previous sandbox generation with the same name (see issue #756).
// Returns "", false when the pod is owned by the current sandbox or carries
// no Sandbox owner reference.
func StaleSandboxPodOwner(pod *corev1.Pod, box *agentsv1alpha1.Sandbox) (types.UID, bool) {
	for i := range pod.OwnerReferences {
		ref := &pod.OwnerReferences[i]
		if ref.Kind == "Sandbox" && ref.APIVersion == agentsv1alpha1.SchemeGroupVersion.String() {
			if ref.UID == box.UID {
				return "", false
			}
			return ref.UID, true
		}
	}
	return "", false
}
