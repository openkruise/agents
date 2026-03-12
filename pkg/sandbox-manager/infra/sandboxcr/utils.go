package sandboxcr

import (
	"encoding/json"
	"fmt"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
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

func GetSandboxCondition(sbx *v1alpha1.Sandbox, tp v1alpha1.SandboxConditionType) metav1.Condition {
	for _, condition := range sbx.Status.Conditions {
		if condition.Type == string(tp) {
			return condition
		}
	}
	return metav1.Condition{}
}

func getInitRuntimeRequest(s metav1.Object) (*config.InitRuntimeOptions, error) {
	// Build initRuntimeOpts from annotation at the beginning
	var initRuntimeOpts *config.InitRuntimeOptions
	if initRuntimeRequest := s.GetAnnotations()[v1alpha1.AnnotationInitRuntimeRequest]; initRuntimeRequest != "" {
		var opts config.InitRuntimeOptions
		if err := json.Unmarshal([]byte(initRuntimeRequest), &opts); err != nil {
			return nil, fmt.Errorf("failed to unmarshal init runtime request: %w", err)
		}
		opts.ReInit = true
		initRuntimeOpts = &opts
	}
	return initRuntimeOpts, nil
}
