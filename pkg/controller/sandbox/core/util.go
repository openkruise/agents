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
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
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

func GeneratePodFromSandbox(ctx context.Context, cli client.Client, box *agentsv1alpha1.Sandbox, revision string) (*corev1.Pod, error) {
	podTemplate := box.Spec.Template
	if box.Spec.TemplateRef != nil {
		refTemplate := &agentsv1alpha1.SandboxTemplate{}
		err := cli.Get(ctx, client.ObjectKey{Namespace: box.Namespace, Name: box.Spec.TemplateRef.Name}, refTemplate)
		if err != nil {
			klog.ErrorS(err, "failed to get sandbox template", "sandbox", klog.KObj(box), "template", box.Spec.TemplateRef.Name)
			return nil, err
		}
		podTemplate = refTemplate.Spec.Template
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       box.Namespace,
			Name:            box.Name,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(box, sandboxControllerKind)},
			Labels:          podTemplate.Labels,
			Annotations:     podTemplate.Annotations,
		},
		Spec: podTemplate.Spec,
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[utils.PodAnnotationCreatedBy] = utils.CreatedBySandbox
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[utils.PodLabelCreatedBy] = utils.CreatedBySandbox
	// todo, when resume, create Pod based on the revision from the paused state.
	pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = revision

	volumes := make([]corev1.Volume, 0, len(box.Spec.VolumeClaimTemplates))
	for _, template := range box.Spec.VolumeClaimTemplates {
		pvcName, err := GeneratePVCName(template.Name, box.Name)
		if err != nil {
			klog.ErrorS(err, "failed to generate PVC name", "sandbox", klog.KObj(box), "template", template.Name)
			return nil, err
		}
		volumes = append(volumes, corev1.Volume{
			Name: template.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
					ReadOnly:  false,
				},
			},
		})
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, volumes...)
	return pod, nil
}
