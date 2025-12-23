package sandboxcr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/pkg/utils/sandbox-manager/proxyutils"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

type SandboxCR interface {
	*agentsv1alpha1.Sandbox
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
	Cache   *Cache

	PatchSandbox  PatchFunc[T]
	UpdateStatus  UpdateFunc[T]
	Update        UpdateFunc[T]
	DeleteFunc    DeleteFunc
	SetCondition  SetConditionFunc[T]
	GetConditions GetConditionsFunc[T]
	DeepCopy      DeepCopyFunc[T]
}

func (s *BaseSandbox[T]) GetTemplate() string {
	return s.Sandbox.GetLabels()[agentsv1alpha1.LabelSandboxPool]
}

func (s *BaseSandbox[T]) InplaceRefresh(deepcopy bool) error {
	sbx, err := s.Cache.GetSandbox(stateutils.GetSandboxID(s.Sandbox))
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
		updated, err := updateFunc(ctx, s.Sandbox, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		s.Sandbox = updated
		return nil
	})
	if err != nil {
		log.Error(err, "failed to update sandbox after retries")
	} else {
		log.Info("sandbox updated successfully")
	}
	return err
}

func (s *BaseSandbox[T]) Kill(ctx context.Context) error {
	if s.Sandbox.GetDeletionTimestamp() != nil {
		return nil
	}
	return s.DeleteFunc(ctx, s.Sandbox.GetName(), metav1.DeleteOptions{})
}

type Sandbox struct {
	BaseSandbox[*agentsv1alpha1.Sandbox]
	*agentsv1alpha1.Sandbox
}

func (s *Sandbox) GetSandboxID() string {
	return stateutils.GetSandboxID(s.Sandbox)
}

func (s *Sandbox) GetRoute() proxy.Route {
	state, _ := s.GetState()
	return proxy.Route{
		IP:    s.Status.PodInfo.PodIP,
		ID:    s.GetSandboxID(),
		Owner: s.GetAnnotations()[agentsv1alpha1.AnnotationOwner],
		State: state,
	}
}

func (s *Sandbox) SetTimeout(ttl time.Duration) {
	s.Spec.ShutdownTime = ptr.To(metav1.NewTime(time.Now().Add(ttl)))
}

func (s *Sandbox) SaveTimeout(ctx context.Context, ttl time.Duration) error {
	return s.retryUpdate(ctx, s.Update, func(sbx *agentsv1alpha1.Sandbox) {
		sbx.Spec.ShutdownTime = ptr.To(metav1.NewTime(time.Now().Add(ttl)))
	})
}

func (s *Sandbox) GetTimeout() time.Time {
	if s.Spec.ShutdownTime == nil {
		return time.Time{}
	}
	return s.Spec.ShutdownTime.Time
}

func (s *Sandbox) GetResource() infra.SandboxResource {
	if s.Spec.Template == nil {
		return infra.SandboxResource{}
	}
	return utils.CalculateResourceFromContainers(s.Spec.Template.Spec.Containers)
}

func (s *Sandbox) Request(r *http.Request, path string, port int) (*http.Response, error) {
	if s.Status.Phase != agentsv1alpha1.SandboxRunning {
		return nil, errors.New("sandbox is not running")
	}
	return proxyutils.ProxyRequest(r, path, port, s.Status.PodInfo.PodIP)
}

func (s *Sandbox) Pause(ctx context.Context) error {
	log := klog.FromContext(ctx)
	if s.Status.Phase != agentsv1alpha1.SandboxRunning {
		return fmt.Errorf("sandbox is not in running phase")
	}
	state, reason := s.GetState()
	if state != agentsv1alpha1.SandboxStateRunning {
		err := fmt.Errorf("pausing is only available for running state, current state: %s", state)
		log.Error(err, "sandbox is not running", "state", state, "reason", reason)
		return err
	}
	err := s.retryUpdate(ctx, s.Update, func(sbx *agentsv1alpha1.Sandbox) {
		sbx.Spec.Paused = true
	})
	if err != nil {
		log.Error(err, "failed to update sandbox spec.paused")
		return err
	}
	utils.ResourceVersionExpectationExpect(s.Sandbox)
	return nil
}

func (s *Sandbox) Resume(ctx context.Context) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox))
	state, reason := s.GetState()
	log.Info("try to resume sandbox", "state", state, "reason", reason)
	if state != agentsv1alpha1.SandboxStatePaused {
		err := fmt.Errorf("resuming is only available for paused state, current state: %s", state)
		log.Error(err, "sandbox is not paused", "state", state, "reason", reason)
		return err
	}
	cond, ok := GetSandboxCondition(s.Sandbox, agentsv1alpha1.SandboxConditionPaused)
	if ok && cond.Status != metav1.ConditionTrue {
		return fmt.Errorf("sandbox is pausing, please wait a moment and try again")
	}
	if s.Sandbox.Spec.Paused {
		if err := s.retryUpdate(ctx, s.Update, func(sbx *agentsv1alpha1.Sandbox) {
			sbx.Spec.Paused = false
		}); err != nil {
			log.Error(err, "failed to update sandbox spec.paused")
			return err
		}
	}
	utils.ResourceVersionExpectationExpect(s.Sandbox) // expect Resuming
	log.Info("waiting sandbox resume")
	start := time.Now()
	err := s.Cache.WaitForSandboxSatisfied(ctx, s.Sandbox, WaitActionResume, func(sbx *agentsv1alpha1.Sandbox) (bool, error) {
		state, reason := stateutils.GetSandboxState(sbx)
		log.V(consts.DebugLogLevel).Info("sandbox state updated", "state", state, "reason", reason)
		return state == agentsv1alpha1.SandboxStateRunning, nil
	}, time.Minute)
	if err != nil {
		log.Error(err, "failed to wait sandbox resume")
		return err
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

func (s *Sandbox) GetState() (string, string) {
	return stateutils.GetSandboxState(s.Sandbox)
}

func (s *Sandbox) GetClaimTime() (time.Time, error) {
	claimTimestamp := s.GetAnnotations()[agentsv1alpha1.AnnotationClaimTime]
	return time.Parse(time.RFC3339, claimTimestamp)
}

func DeepCopy(sbx *agentsv1alpha1.Sandbox) *agentsv1alpha1.Sandbox {
	return sbx.DeepCopy()
}

var _ infra.Sandbox = &Sandbox{}
