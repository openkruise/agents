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

package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
)

// projectSandboxForGatewayCache keeps only fields used to calculate gateway routes.
func projectSandboxForGatewayCache(in interface{}) (interface{}, error) {
	sandbox, ok := in.(*agentsv1alpha1.Sandbox)
	if !ok {
		return in, nil
	}

	return &agentsv1alpha1.Sandbox{
		TypeMeta: sandbox.TypeMeta,
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         sandbox.Namespace,
			Name:              sandbox.Name,
			UID:               sandbox.UID,
			ResourceVersion:   sandbox.ResourceVersion,
			DeletionTimestamp: sandbox.DeletionTimestamp.DeepCopy(),
			OwnerReferences:   copyOwnerReferences(sandbox.OwnerReferences),
			Annotations:       gatewayRouteAnnotations(sandbox.Annotations),
		},
		Spec: agentsv1alpha1.SandboxSpec{
			Paused:       sandbox.Spec.Paused,
			ShutdownTime: sandbox.Spec.ShutdownTime.DeepCopy(),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase:      sandbox.Status.Phase,
			Conditions: gatewayReadyCondition(sandbox.Status.Conditions),
			PodInfo: agentsv1alpha1.PodInfo{
				PodIP: sandbox.Status.PodInfo.PodIP,
			},
		},
	}, nil
}

func gatewayRouteAnnotations(annotations map[string]string) map[string]string {
	projected := make(map[string]string, 3)
	for _, key := range []string{
		agentsv1alpha1.AnnotationOwner,
		agentsv1alpha1.AnnotationRuntimeAccessToken,
		identity.AnnotationEnableJwtAuth,
	} {
		if value, ok := annotations[key]; ok {
			projected[key] = value
		}
	}
	if len(projected) == 0 {
		return nil
	}
	return projected
}

func gatewayReadyCondition(conditions []metav1.Condition) []metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == string(agentsv1alpha1.SandboxConditionReady) {
			return []metav1.Condition{{
				Type:   conditions[i].Type,
				Status: conditions[i].Status,
			}}
		}
	}
	return nil
}

func copyOwnerReferences(ownerReferences []metav1.OwnerReference) []metav1.OwnerReference {
	if len(ownerReferences) == 0 {
		return nil
	}
	projected := make([]metav1.OwnerReference, len(ownerReferences))
	for i := range ownerReferences {
		projected[i] = *ownerReferences[i].DeepCopy()
	}
	return projected
}
