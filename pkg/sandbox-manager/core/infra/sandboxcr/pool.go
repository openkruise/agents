package sandboxcr

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/openkruise/agents/api/v1alpha1"
	sandboxclient "github.com/openkruise/agents/client/clientset/versioned"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

type GroupedSandboxes struct {
	Creating []*Sandbox // 容器实例正在创建中的 Sandbox
	Pending  []*Sandbox // 容器实例已经就绪，但是未被消费的池化 Sandbox
	Claimed  []*Sandbox // 已经被分配给用户的 Sandbox
	Failed   []*Sandbox // 由于各种原因需要被删除的 Sandbox，包含删除中的对象
}

type PoolSpec struct {
	Replicas atomic.Int32
}

type PoolStatus struct {
	creating atomic.Int32 // creating sandboxes
	pending  atomic.Int32 // pods available to be claimed
	claimed  atomic.Int32 // claimed running sandboxes
	total    atomic.Int32
}

type Pool struct {
	Spec           PoolSpec
	Status         PoolStatus
	reconcileQueue chan context.Context
	stopped        atomic.Bool

	// Should init fields
	eventer  *events.Eventer
	client   sandboxclient.Interface
	cache    Cache[*v1alpha1.Sandbox]
	template *infra.SandboxTemplate
}

func (p *Pool) AsSandbox(sbx *v1alpha1.Sandbox) *Sandbox {
	if sbx.Annotations == nil {
		sbx.Annotations = make(map[string]string)
	}
	if sbx.Labels == nil {
		sbx.Labels = make(map[string]string)
	}
	return &Sandbox{
		BaseSandbox: BaseSandbox[*v1alpha1.Sandbox]{
			Sandbox:       sbx,
			Cache:         p.cache,
			PatchSandbox:  p.client.ApiV1alpha1().Sandboxes(p.template.Namespace).Patch,
			UpdateStatus:  p.client.ApiV1alpha1().Sandboxes(p.template.Namespace).UpdateStatus,
			DeleteFunc:    p.client.ApiV1alpha1().Sandboxes(p.template.Namespace).Delete,
			SetCondition:  SetSandboxCondition,
			GetConditions: ListSandboxConditions,
			DeepCopy:      DeepCopy,
		},
		Sandbox: sbx,
	}
}

func (p *Pool) SyncFromCluster(ctx context.Context) error {
	log := klog.FromContext(ctx).WithValues("pool", p.template.Name)
	sandboxList, err := p.client.ApiV1alpha1().Sandboxes(p.template.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", consts.LabelSandboxPool, p.template.Name),
	})
	if err != nil {
		log.Error(err, "list sandboxes failed")
		return err
	}
	total := int32(len(sandboxList.Items))
	log.Info("existing sandboxes listed", "total", total)
	if total > p.template.Spec.MaxPoolSize {
		total = p.template.Spec.MaxPoolSize
	}
	if total < p.template.Spec.MinPoolSize {
		total = p.template.Spec.MinPoolSize
	}
	allSandboxes := make([]*v1alpha1.Sandbox, 0, total)
	for i := range sandboxList.Items {
		allSandboxes = append(allSandboxes, &sandboxList.Items[i])
		condition, _ := GetSandboxCondition(&sandboxList.Items[i], v1alpha1.SandboxConditionReady)
		if condition.Status == metav1.ConditionTrue {
			log.V(consts.DebugLogLevel).Info("will re-trigger SandboxCreated for existing ready sandbox")
			go p.eventer.Trigger(events.Event{
				Type:    consts.SandboxCreated,
				Sandbox: p.AsSandbox(&sandboxList.Items[i]),
				Message: "OnRestart",
				Source:  "SyncFromCluster",
			})
		}
	}
	groups, err := p.GroupAllSandboxes(ctx, allSandboxes)
	p.SaveStatusFromGroup(groups)
	p.Spec.Replicas.Store(total)
	log.Info("starting first reconcile", "spec.total", total)
	return p.Reconcile(ctx)
}

func (p *Pool) Scale(ctx context.Context) error {
	if p.template == nil {
		return errors.New("pool template is not set")
	}
	log := klog.FromContext(ctx).V(consts.DebugLogLevel).WithValues("pool", p.template.Name)
	total, creating, pending := p.Status.total.Load(), p.Status.creating.Load(), p.Status.pending.Load()
	expectTotal, err := utils.CalculateExpectPoolSize(ctx, total-creating, pending, p.template)
	if err != nil {
		log.Error(err, "calculate expect pool size failed")
		return err
	}
	if expectTotal == total {
		log.Info("no need to scale pool", "total", total)
	} else {
		log.Info("will scale pool", "total", total, "expectTotal", expectTotal)
		p.Spec.Replicas.Store(expectTotal)
	}
	p.EnqueueRequest(ctx)
	return nil
}

func (p *Pool) ClaimSandbox(ctx context.Context, user string, modifier func(sbx infra.Sandbox)) (infra.Sandbox, error) {
	if p.Status.pending.Load() == 0 {
		return nil, fmt.Errorf("no pending sandboxes for template %s", p.template.Name)
	}
	lock := uuid.New().String()
	for i := 0; i < 10; i++ {
		objects, err := p.cache.SelectSandboxes(consts.LabelSandboxState, consts.SandboxStatePending,
			consts.LabelSandboxPool, p.template.Name)
		if err != nil {
			return nil, err
		}
		if len(objects) == 0 {
			return nil, fmt.Errorf("cannot find pending sandboxes for template %s", p.template.Name)
		}
		var obj *v1alpha1.Sandbox
		for _, obj = range objects {
			if obj.Status.Phase == v1alpha1.SandboxRunning && obj.Annotations[consts.AnnotationLock] == "" {
				break
			}
		}
		if obj == nil || obj.Annotations[consts.AnnotationLock] != "" {
			return nil, fmt.Errorf("all sandboxes are locked")
		}

		// Go to Sandbox interface
		sbx := p.AsSandbox(obj.DeepCopy())
		if modifier != nil {
			modifier(sbx)
		}
		sbx.Labels[consts.LabelSandboxState] = consts.SandboxStateRunning
		sbx.Annotations[consts.AnnotationLock] = lock
		sbx.Annotations[consts.AnnotationOwner] = user
		updated, err := p.client.ApiV1alpha1().Sandboxes(sbx.Namespace).Update(ctx, sbx.Sandbox, metav1.UpdateOptions{})
		if err == nil {
			sbx.Sandbox = updated
			return sbx, nil
		}
		klog.ErrorS(err, "failed to acquire optimistic lock of pod", "pool", klog.KObj(p.template), "retries", i+1)
	}
	return nil, fmt.Errorf("failed to acquire optimistic lock of pod after max retries")
}

func (p *Pool) GetTemplate() *infra.SandboxTemplate {
	if p.template == nil {
		return &infra.SandboxTemplate{}
	}
	return p.template
}

func (p *Pool) LoadDebugInfo() map[string]int32 {
	return map[string]int32{
		"total":    p.Status.total.Load(),
		"pending":  p.Status.pending.Load(),
		"claimed":  p.Status.claimed.Load(),
		"creating": p.Status.creating.Load(),
	}
}
