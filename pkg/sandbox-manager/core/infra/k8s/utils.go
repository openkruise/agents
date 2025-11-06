package k8s

import (
	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func ParseTemplateAsDeployment(t *infra.SandboxTemplate) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        t.Name,
			Namespace:   t.Namespace,
			Labels:      t.Labels,
			Annotations: t.Annotations,
		},
		Spec: appsv1.DeploymentSpec{
			RevisionHistoryLimit: ptr.To(int32(2)),
			Replicas:             &t.Spec.MinPoolSize,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					consts.LabelSandboxPool: t.Name,
				},
			},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					// necessary for updating paused sandboxes
					MaxUnavailable: ptr.To(intstr.Parse("100%")),
				},
			},
			Template: t.Spec.Template,
		},
	}
}

func SetPodCondition(pod *corev1.Pod, tp corev1.PodConditionType, status corev1.ConditionStatus,
	reason, message string) {
	now := metav1.Now()
	for i, condition := range pod.Status.Conditions {
		if condition.Type == tp {
			pod.Status.Conditions[i].Status = status
			pod.Status.Conditions[i].Reason = reason
			pod.Status.Conditions[i].Message = message
			pod.Status.Conditions[i].LastTransitionTime = now
			return
		}
	}
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:               tp,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		LastProbeTime:      now,
	})
}

func GetPodCondition(pod *corev1.Pod, tp corev1.PodConditionType) (corev1.PodCondition, bool) {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == tp {
			return condition, true
		}
	}
	return corev1.PodCondition{}, false
}

var (
	GroupCreating = "creating"
	GroupFailed   = "failed"
	GroupPending  = "pending"
	GroupRunning  = "running"
	GroupPaused   = "paused"
)

func FindSandboxGroup(pod *corev1.Pod) (group, reason string) {
	if pod.DeletionTimestamp != nil {
		return GroupFailed, "ResourceDeleted"
	}
	switch pod.Status.Phase {
	case "":
		fallthrough
	case corev1.PodPending:
		return GroupCreating, "ResourcePending"
	case corev1.PodFailed:
		return GroupFailed, "ResourceFailed"
	case corev1.PodSucceeded:
		return GroupFailed, "ResourceSucceeded"
	default:
		switch pod.Labels[consts.LabelSandboxState] {
		case consts.SandboxStateRunning:
			return GroupRunning, "SandboxRunning"
		case consts.SandboxStatePaused:
			return GroupPaused, "SandboxPaused"
		case consts.SandboxStatePending:
			return GroupPending, "SandboxPending"
		case consts.SandboxStateKilling:
			// 业务逻辑进入到 killing，但是还没有触发 CR 删除逻辑
			return GroupFailed, "SandboxKilling"
		case "":
			return GroupCreating, "SandboxStateNotPatched"
		default: // 不可能进入的分支，以防万一
			return GroupFailed, "UnknownState"
		}
	}
}
