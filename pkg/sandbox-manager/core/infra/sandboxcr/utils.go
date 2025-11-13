package sandboxcr

import (
	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
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

var (
	GroupCreating = "creating"
	GroupFailed   = "failed"
	GroupPending  = "pending"
	GroupClaimed  = "claimed"
	GroupUnknown  = "unknown"
)

func FindSandboxGroup(sbx *v1alpha1.Sandbox, updateHash string) (group, reason string) {
	if sbx.DeletionTimestamp != nil {
		return GroupFailed, "ResourceDeleted"
	}
	if sbx.Labels[consts.LabelTemplateHash] != updateHash {
		return GroupFailed, "LegacyHash"
	}
	switch sbx.Status.Phase {
	case "":
		fallthrough
	case v1alpha1.SandboxPending:
		return GroupCreating, "ResourcePending"
	case v1alpha1.SandboxFailed:
		return GroupFailed, "ResourceFailed"
	case v1alpha1.SandboxSucceeded:
		return GroupFailed, "ResourceSucceeded"
	case v1alpha1.SandboxTerminating:
		return GroupFailed, "ResourceTerminating"
	default: // Running, Paused
		switch sbx.Labels[consts.LabelSandboxState] {
		case consts.SandboxStateRunning:
			return GroupClaimed, "SandboxRunning"
		case consts.SandboxStatePaused:
			return GroupClaimed, "SandboxPaused"
		case consts.SandboxStatePending:
			return GroupPending, "SandboxPending"
		case consts.SandboxStateKilling:
			// 业务逻辑进入到 killing，但是还没有触发 CR 删除逻辑
			return GroupFailed, "SandboxKilling"
		case "":
			return GroupCreating, "SandboxStateNotPatched"
		default: // 不可能进入的分支，以防万一
			return GroupUnknown, "Unknown"
		}
	}
}
