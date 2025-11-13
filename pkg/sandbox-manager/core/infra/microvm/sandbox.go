package microvm

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/utils"
	"gitlab.alibaba-inc.com/serverlessinfra/sandbox-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

type Sandbox struct {
	sandboxcr.BaseSandbox[*v1alpha1.Sandbox]
	*v1alpha1.Sandbox
}

func (s *Sandbox) GetIP() string {
	return s.Status.Info.NodeIP
}

func (s *Sandbox) GetRouteHeader() map[string]string {
	return map[string]string{
		"X-MICROSANDBOX-ID": s.Status.Info.SandboxId,
	}
}

func (s *Sandbox) GetResource() infra.SandboxResource {
	return infra.SandboxResource{
		CPUMilli: s.Spec.Template.Vcpu * 1000,
		MemoryMB: s.Spec.Template.RamMb,
	}
}

func (s *Sandbox) Request(r *http.Request, path string, port int) (*http.Response, error) {
	headers := s.GetRouteHeader()
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	r.Header.Set("X-MICROSANDBOX-PORT", strconv.Itoa(port))
	return utils.ProxyRequest(r, path, 5007, s.GetIP())
}

func (s *Sandbox) Pause(ctx context.Context) error {
	if s.Status.Phase != v1alpha1.SandboxRunning {
		return fmt.Errorf("sandbox is not in running state")
	}
	return s.Patch(ctx, fmt.Sprintf(`{"metadata":{"labels":{"%s":"%s"}},"spec":{"paused":true}}`,
		consts.LabelSandboxState, consts.SandboxStatePaused))
}

func (s *Sandbox) Resume(ctx context.Context) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox))
	if s.Status.Phase != v1alpha1.SandboxPaused {
		return fmt.Errorf("sandbox is not in paused state")
	}
	err := s.Patch(ctx, `{"spec":{"paused":false}}`)
	if err != nil {
		log.Error(err, "failed to patch sandbox spec")
		return err
	}
	log.Info("waiting sandbox resume")
	start := time.Now()
	err = retry.OnError(wait.Backoff{
		Steps:    600, // 1 min
		Duration: 100 * time.Millisecond,
		Factor:   1.0,
		Jitter:   0.1,
	}, func(err error) bool {
		return true
	}, func() error {
		return s.checkPhase(v1alpha1.SandboxRunning)
	})
	if err != nil {
		log.Error(err, "failed to wait sandbox resume")
		return err
	}
	err = s.Patch(ctx, fmt.Sprintf(`{"metadata":{"labels":{"%s":"%s"}}}`,
		consts.LabelSandboxState, consts.SandboxStateRunning))
	if err != nil {
		log.Error(err, "failed to patch sandbox state")
		return err
	}
	log.Info("sandbox resumed", "cost", time.Since(start))
	return nil
}

func (s *Sandbox) checkPhase(phase v1alpha1.Phase) error {
	err := s.InplaceRefresh(false)
	if err != nil {
		return err
	}
	if s.Status.Phase != phase {
		return fmt.Errorf("check phase failed, expect: %s, actual: %s", phase, s.Status.Phase)
	}
	condition, ok := GetSandboxCondition(s.Sandbox, v1alpha1.SandboxConditionReady)
	if !ok {
		return fmt.Errorf("check condition failed, SandboxConditionReady not found")
	}
	if condition.Status != metav1.ConditionTrue {
		return fmt.Errorf("check condition failed, expect: %s, actual: %s", metav1.ConditionTrue, condition.Status)
	}
	return nil
}

func (s *Sandbox) InplaceRefresh(deepcopy bool) error {
	err := s.BaseSandbox.InplaceRefresh(deepcopy)
	if err != nil {
		return err
	}
	s.Sandbox = s.BaseSandbox.Sandbox
	return nil
}

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

func GetSandboxCondition(sbx *v1alpha1.Sandbox, tp v1alpha1.ConditionType) (metav1.Condition, bool) {
	for _, condition := range sbx.Status.Conditions {
		if condition.Type == string(tp) {
			return condition, true
		}
	}
	return metav1.Condition{}, false
}

func DeepCopy(sbx *v1alpha1.Sandbox) *v1alpha1.Sandbox {
	return sbx.DeepCopy()
}

//func FindSandboxGroup(sbx *v1alpha1.Sandbox, updateHash string) (group, reason string) {
//	if sbx.DeletionTimestamp != nil {
//		return sandboxcr.GroupFailed, "ResourceDeleted"
//	}
//	if sbx.Labels[consts.LabelTemplateHash] != updateHash {
//		return sandboxcr.GroupFailed, "LegacyHash"
//	}
//	switch sbx.Status.Phase {
//	case "":
//		fallthrough
//	case v1alpha1.SandboxPending:
//		return sandboxcr.GroupCreating, "ResourcePending"
//	case v1alpha1.SandboxFailed:
//		return sandboxcr.GroupFailed, "ResourceFailed"
//	default:
//		switch sbx.Labels[consts.LabelSandboxState] {
//		case consts.SandboxStateRunning:
//			return sandboxcr.GroupClaimed, "SandboxRunning"
//		case consts.SandboxStatePaused:
//			return sandboxcr.GroupClaimed, "SandboxPaused"
//		case consts.SandboxStatePending:
//			return sandboxcr.GroupPending, "SandboxPending"
//		case consts.SandboxStateKilling:
//			// 业务逻辑进入到 killing，但是还没有触发 CR 删除逻辑
//			return sandboxcr.GroupFailed, "SandboxKilling"
//		case "":
//			return sandboxcr.GroupCreating, "SandboxStateNotPatched"
//		default: // 不可能进入的分支，以防万一
//			return sandboxcr.GroupUnknown, "Unknown"
//		}
//	}
//}
