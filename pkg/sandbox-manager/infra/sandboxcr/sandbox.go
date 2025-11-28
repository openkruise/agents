package sandboxcr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	kruise "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	utils2 "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	microvm "gitlab.alibaba-inc.com/serverlessinfra/sandbox-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

type SandboxCR interface {
	*kruise.Sandbox | *microvm.Sandbox
	metav1.Object
}

type PatchFunc[T SandboxCR] func(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subResources ...string) (result T, err error)
type UpdateFunc[T SandboxCR] func(ctx context.Context, sbx T, opts metav1.UpdateOptions) (T, error)
type DeleteFunc func(ctx context.Context, name string, opts metav1.DeleteOptions) error
type ModifierFunc[T SandboxCR] func(sbx T)
type SetConditionFunc[T SandboxCR] func(sbx T, tp string, status metav1.ConditionStatus, reason, message string)
type GetConditionsFunc[T SandboxCR] func(sbx T) []metav1.Condition
type DeepCopyFunc[T any] func(src T) T

type BaseSandbox[T SandboxCR] struct {
	Sandbox T
	Cache   Cache[T]

	PatchSandbox  PatchFunc[T]
	UpdateStatus  UpdateFunc[T]
	DeleteFunc    DeleteFunc
	SetCondition  SetConditionFunc[T]
	GetConditions GetConditionsFunc[T]
	DeepCopy      DeepCopyFunc[T]
}

func (s *BaseSandbox[T]) Patch(ctx context.Context, patchStr string) error {
	if s.PatchSandbox == nil {
		return errors.New("patch is not supported")
	}
	_, err := s.PatchSandbox(
		ctx,
		s.Sandbox.GetName(),
		types.MergePatchType,
		[]byte(patchStr),
		metav1.PatchOptions{},
	)

	return err
}

func (s *BaseSandbox[T]) PatchLabels(ctx context.Context, labels map[string]string) error {
	if labels == nil || len(labels) == 0 {
		return nil
	}
	j, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	patchStr := fmt.Sprintf(`{"metadata":{"labels":%s}}`, string(j))
	return s.Patch(ctx, patchStr)
}

func (s *BaseSandbox[T]) GetState() string {
	return s.Sandbox.GetLabels()[kruise.LabelSandboxState]
}

func (s *BaseSandbox[T]) GetTemplate() string {
	return s.Sandbox.GetLabels()[kruise.LabelSandboxPool]
}

func (s *BaseSandbox[T]) SetState(ctx context.Context, state string) error {
	return s.Patch(ctx, fmt.Sprintf(`{"metadata":{"labels":{"%s":"%s"}}}`, kruise.LabelSandboxState, state))
}

func (s *BaseSandbox[T]) GetOwnerUser() string {
	return s.Sandbox.GetAnnotations()[kruise.AnnotationOwner]
}

func (s *BaseSandbox[T]) InplaceRefresh(deepcopy bool) error {
	sbx, err := s.Cache.GetSandbox(s.Sandbox.GetName())
	if err != nil {
		return err
	}
	if deepcopy {
		s.Sandbox = s.DeepCopy(sbx)
	} else {
		s.Sandbox = sbx
	}
	return nil
}

func (s *BaseSandbox[T]) RetryModifyStatus(ctx context.Context, modifier func(sbx T)) error {
	return s.retryUpdate(ctx, s.UpdateStatus, modifier)
}

func (s *BaseSandbox[T]) retryUpdate(ctx context.Context, updateFunc UpdateFunc[T], modifier func(sbx T)) error {
	if s.Sandbox == nil {
		return errors.New("sandbox is nil")
	}
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox))
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// get the latest sandbox
		if err := s.InplaceRefresh(true); err != nil {
			return fmt.Errorf("failed to refresh sandbox: %w", err)
		}
		modifier(s.Sandbox)
		_, err := updateFunc(ctx, s.Sandbox, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		log.Error(err, "failed to update sandbox status after retries")
	}
	return err
}

func (s *BaseSandbox[T]) SaveTimer(ctx context.Context, afterSeconds int, event consts.EventType, triggered bool, result string) error {
	key, status, reason, message := utils2.GenerateTimerCondition(afterSeconds, event, triggered, result)
	modifier := func(sbx T) {
		s.SetCondition(sbx, key, metav1.ConditionStatus(status), reason, message)
	}
	return s.RetryModifyStatus(ctx, modifier)
}

func (s *BaseSandbox[T]) LoadTimers(callback func(after time.Duration, eventType consts.EventType)) error {
	for _, condition := range s.GetConditions(s.Sandbox) {
		if condition.Status != metav1.ConditionFalse {
			continue
		}
		if err := utils2.CheckAndLoadTimerFromCondition(
			condition.Type, condition.Message, condition.LastTransitionTime.Time, callback); err != nil {
			return err
		}
	}
	return nil
}

func (s *BaseSandbox[T]) Kill(ctx context.Context) error {
	if s.Sandbox.GetDeletionTimestamp() != nil {
		return nil
	}
	if err := s.SetState(ctx, kruise.SandboxStateKilling); err != nil {
		return err
	}
	return s.DeleteFunc(ctx, s.Sandbox.GetName(), metav1.DeleteOptions{})
}

type Sandbox struct {
	BaseSandbox[*kruise.Sandbox]
	*kruise.Sandbox
}

func (s *Sandbox) GetIP() string {
	return s.Status.PodInfo.PodIP
}

func (s *Sandbox) GetRouteHeader() map[string]string {
	return nil
}

func (s *Sandbox) GetResource() infra.SandboxResource {
	return utils2.CalculateResourceFromContainers(s.Spec.Template.Spec.Containers)
}

func (s *Sandbox) Request(r *http.Request, path string, port int) (*http.Response, error) {
	if s.Status.Phase != kruise.SandboxRunning {
		return nil, errors.New("sandbox is not running")
	}
	return utils2.ProxyRequest(r, path, port, s.GetIP())
}

func (s *Sandbox) Pause(ctx context.Context) error {
	if s.Status.Phase != kruise.SandboxRunning {
		return fmt.Errorf("sandbox is not in running phase")
	}
	state := s.GetState()
	if state != kruise.SandboxStateRunning && state != kruise.SandboxStateAvailable {
		return fmt.Errorf("pausing is only available for the following state [%s,%s],not for the current state %s",
			kruise.SandboxStateRunning, kruise.SandboxStateAvailable, state)
	}
	var nextState string
	if state == kruise.SandboxStateRunning {
		nextState = kruise.SandboxStatePaused
	} else {
		nextState = state
	}
	return s.Patch(ctx, fmt.Sprintf(`{"metadata":{"labels":{"%s":"%s"}},"spec":{"paused":true}}`,
		kruise.LabelSandboxState, nextState))
}

func (s *Sandbox) Resume(ctx context.Context) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox))
	if s.Status.Phase != kruise.SandboxPaused {
		return fmt.Errorf("sandbox is not in paused state")
	}
	state := s.GetState()
	if state != kruise.SandboxStatePaused && state != kruise.SandboxStateAvailable {
		return fmt.Errorf("resuming is only available for the following state [%s,%s],not for the current state %s",
			kruise.SandboxStatePaused, kruise.SandboxStateAvailable, state)
	}
	cond, ok := GetSandboxCondition(s.Sandbox, kruise.SandboxConditionPaused)
	if !ok || cond.Status != metav1.ConditionTrue {
		return fmt.Errorf("sandbox is pausing, please wait a moment and try again")
	}
	err := s.Patch(ctx, `{"spec":{"paused":false}}`)
	if err != nil {
		log.Error(err, "failed to patch sandbox spec")
		return err
	}
	log.Info("waiting sandbox resume")
	start := time.Now()
	err = retry.OnError(wait.Backoff{
		Steps:    900, // 1.5 min
		Duration: 100 * time.Millisecond,
		Factor:   1.0,
		Jitter:   0.1,
	}, func(err error) bool {
		return true
	}, func() error {
		err := s.InplaceRefresh(false)
		if err != nil {
			return err
		}
		if s.Status.Phase != kruise.SandboxRunning {
			return fmt.Errorf("check phase failed, expect: %s, actual: %s", kruise.SandboxRunning, s.Status.Phase)
		}
		condition, ok := GetSandboxCondition(s.Sandbox, kruise.SandboxConditionReady)
		if !ok {
			return fmt.Errorf("check condition failed, SandboxConditionReady not found")
		}
		if condition.Status != metav1.ConditionTrue {
			return fmt.Errorf("check condition failed, expect: %s, actual: %s", metav1.ConditionTrue, condition.Status)
		}
		return nil
	})
	if err != nil {
		log.Error(err, "failed to wait sandbox resume")
		return err
	}
	// 对于 paused state，转化为 running，其他的保持不变
	if state == kruise.SandboxStatePaused {
		if err = s.Patch(ctx, fmt.Sprintf(`{"metadata":{"labels":{"%s":"%s"}}}`,
			kruise.LabelSandboxState, kruise.SandboxStateRunning)); err != nil {
			log.Error(err, "failed to patch sandbox state")
			return err
		}
	}
	log.Info("sandbox resumed", "cost", time.Since(start))
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

func DeepCopy(sbx *kruise.Sandbox) *kruise.Sandbox {
	return sbx.DeepCopy()
}

var _ infra.Sandbox = &Sandbox{}
