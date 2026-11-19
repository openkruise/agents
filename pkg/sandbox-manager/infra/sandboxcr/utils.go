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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/lifecycle/claim"
)

func SetSandboxCondition(sbx *v1alpha1.Sandbox, tp string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i, condition := range sbx.Status.Conditions {
		if condition.Type == tp {
			sbx.Status.Conditions[i].Status = status
			sbx.Status.Conditions[i].Reason = reason
			sbx.Status.Conditions[i].Message = message
			sbx.Status.Conditions[i].LastTransitionTime = now
			return
		}
	}
	sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
		Type:               tp,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

func GetSandboxCondition(sbx *v1alpha1.Sandbox, tp v1alpha1.SandboxConditionType) metav1.Condition {
	return claim.GetSandboxCondition(sbx, tp)
}

// ControlPlaneSourceOrDefault normalizes the control plane that handles a sandbox
// into a v1alpha1.LabelSandboxSource value. agent-sandbox-controller is the
// mandatory layer and sandbox-manager an optional one above it, so anything other
// than an explicit sandbox-manager source belongs to the controller.
func ControlPlaneSourceOrDefault(source string) string {
	return claim.ControlPlaneSourceOrDefault(source)
}

func BuildUserMetadataKeys(labels, annotations map[string]string) *v1alpha1.UpdatedMetadataInClaim {
	return claim.BuildUserMetadataKeys(labels, annotations)
}
