package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

type Sandbox struct {
	*corev1.Pod
	Client kubernetes.Interface
	Cache  *Cache
}

func (s *Sandbox) Pause(_ context.Context) error {
	return errors.New("native pod cannot be paused")
}

func (s *Sandbox) Resume(_ context.Context) error {
	return nil
}

func (s *Sandbox) GetIP() string {
	return s.Status.PodIP
}

func (s *Sandbox) GetRouteHeader() map[string]string {
	return nil
}

func (s *Sandbox) PatchLabels(ctx context.Context, labels map[string]string) error {
	if labels == nil || len(labels) == 0 {
		return nil
	}
	j, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	return s.Patch(ctx, fmt.Sprintf(`{"metadata":{"labels":%s}}`, string(j)))
}

func (s *Sandbox) SetState(ctx context.Context, state string) error {
	return s.Patch(ctx, fmt.Sprintf(`{"metadata":{"labels":{"%s":"%s"}}}`, consts.LabelSandboxState, state))
}

func (s *Sandbox) GetState() string {
	return s.Labels[consts.LabelSandboxState]
}

func (s *Sandbox) GetTemplate() string {
	return s.Labels[consts.LabelSandboxPool]
}

func (s *Sandbox) GetResource() infra.SandboxResource {
	return utils.CalculateResourceFromContainers(s.Pod.Spec.Containers)
}

func (s *Sandbox) GetOwnerUser() string {
	return s.Annotations[consts.AnnotationOwner]
}

func (s *Sandbox) SaveTimer(ctx context.Context, afterSeconds int, event consts.EventType, triggered bool, result string) error {
	key, status, reason, message := utils.GenerateTimerCondition(afterSeconds, event, triggered, result)
	modifier := func(pod *corev1.Pod) {
		SetPodCondition(pod, corev1.PodConditionType(key), corev1.ConditionStatus(status), reason, message)
	}
	return s.RetryModifyPodStatus(ctx, modifier)
}

func (s *Sandbox) LoadTimers(callback func(after time.Duration, eventType consts.EventType)) error {
	for _, condition := range s.Status.Conditions {
		if condition.Status != corev1.ConditionFalse {
			continue
		}
		if err := utils.CheckAndLoadTimerFromCondition(
			string(condition.Type), condition.Message, condition.LastTransitionTime.Time, callback); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sandbox) Kill(ctx context.Context) error {
	if s.GetDeletionTimestamp() != nil {
		return nil
	}
	if err := s.SetState(ctx, consts.SandboxStateKilling); err != nil {
		return err
	}
	pod := s.Pod
	return s.Client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
}

func (s *Sandbox) InplaceRefresh(deepcopy bool) error {
	pod, err := s.Cache.GetPod(s.Name)
	if err != nil {
		return err
	}
	if deepcopy {
		s.Pod = pod.DeepCopy()
	} else {
		s.Pod = pod
	}
	return nil
}

func (s *Sandbox) Request(r *http.Request, path string, port int) (*http.Response, error) {
	if s.Status.Phase != corev1.PodRunning {
		return nil, fmt.Errorf("sandbox pod is not running, current phase: %s", s.Status.Phase)
	}
	return utils.ProxyRequest(r, path, port, s.GetIP())
}

func (s *Sandbox) RetryModifyPodStatus(ctx context.Context, modifier func(pod *corev1.Pod)) error {
	return s.retryUpdatePod(ctx, s.Client.CoreV1().Pods(s.Namespace).UpdateStatus, modifier)
}

func (s *Sandbox) RetryModifyPod(ctx context.Context, modifier func(pod *corev1.Pod)) error {
	return s.retryUpdatePod(ctx, s.Client.CoreV1().Pods(s.Namespace).Update, modifier)
}

func (s *Sandbox) retryUpdatePod(ctx context.Context, updateFunc func(ctx context.Context, pod *corev1.Pod, opts metav1.UpdateOptions) (*corev1.Pod, error), modifier func(pod *corev1.Pod)) error {
	if s.Pod == nil {
		return errors.New("pod is nil")
	}
	log := klog.FromContext(ctx).WithValues("pod", klog.KObj(s.Pod))
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// get the latest pod
		if err := s.InplaceRefresh(true); err != nil {
			return fmt.Errorf("failed to refresh pod: %w", err)
		}
		modifier(s.Pod)
		_, err := updateFunc(ctx, s.Pod, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		log.Error(err, "failed to update pod status after retries")
	}
	return err
}

func (s *Sandbox) Patch(ctx context.Context, patchStr string) error {
	_, err := s.Client.CoreV1().Pods(s.Namespace).Patch(
		ctx,
		s.Name,
		types.StrategicMergePatchType,
		[]byte(patchStr),
		metav1.PatchOptions{},
	)

	return err
}
