package sandboxcr

import (
	"github.com/openkruise/agents/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func ListSandboxConditions(sbx *v1alpha1.Sandbox) []metav1.Condition {
	return sbx.Status.Conditions
}

func GetSandboxCondition(sbx *v1alpha1.Sandbox, tp v1alpha1.SandboxConditionType) (metav1.Condition, bool) {
	for _, condition := range sbx.Status.Conditions {
		if condition.Type == string(tp) {
			return condition, true
		}
	}
	return metav1.Condition{}, false
}
