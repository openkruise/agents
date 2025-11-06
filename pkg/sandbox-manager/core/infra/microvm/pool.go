package microvm

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra/sandboxcr"
	"gitlab.alibaba-inc.com/serverlessinfra/sandbox-operator/api/v1alpha1"
	sandboxclient "gitlab.alibaba-inc.com/serverlessinfra/sandbox-operator/client/clientset/versioned"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/klog/v2"
)

var DebugLogLevel = 5

type PoolStatus struct {
	creating atomic.Int32 // creating sandboxes
	pending  atomic.Int32 // pods available to be claimed
	claimed  atomic.Int32 // claimed running sandboxes
	total    atomic.Int32
}
type Pool struct {
	template *infra.SandboxTemplate
	client   sandboxclient.Interface
	cache    sandboxcr.Cache[*v1alpha1.Sandbox]
	eventer  *events.Eventer

	Status PoolStatus
}

func (p *Pool) GetTemplate() *infra.SandboxTemplate {
	return p.template
}

func (p *Pool) ClaimSandbox(ctx context.Context, user string, modifier func(sbx infra.Sandbox)) (infra.Sandbox, error) {
	spec, ok := MicroSandboxSpecMap[p.template.Name]
	if !ok {
		return nil, fmt.Errorf("micro-vm template %s not found", p.template.Name)
	}
	var err error
	var sbx *v1alpha1.Sandbox
	log := klog.FromContext(ctx).WithValues("pool", p.template.Name).V(DebugLogLevel)
	lock := uuid.New().String()
	start := time.Now()
	for i := 0; i < 10; i++ {
		log.Info("will try to create sandbox", "retries", i)
		generatedName := fmt.Sprintf("%s-%s-%s",
			p.template.Name, p.template.Labels[consts.LabelTemplateHash], rand.String(5))
		instance := &v1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:        generatedName,
				Namespace:   p.template.Namespace,
				Labels:      p.template.Spec.Template.Labels,
				Annotations: p.template.Spec.Template.Annotations,
			},
			// MicroVM 还在 POC 中，这里只能写死
			Spec: v1alpha1.SandboxSpec{
				Template: spec,
			},
		}
		sandbox := p.AsSandbox(instance)
		if modifier != nil {
			modifier(sandbox)
		}
		sandbox.Labels[consts.LabelSandboxState] = consts.SandboxStateRunning
		sandbox.Annotations[consts.AnnotationLock] = lock
		sandbox.Annotations[consts.AnnotationOwner] = user
		sbx, err = p.client.ApiV1alpha1().Sandboxes(p.template.Namespace).Create(ctx, sandbox.Sandbox, metav1.CreateOptions{})
		if err == nil || !apierrors.IsAlreadyExists(err) {
			break
		}
	}
	if err != nil {
		log.Error(err, "failed to create sandbox after retries")
		return nil, err
	}
	// wait for node ip
	log.Info("sandbox created, waiting micro-vm creation", "sandbox", klog.KObj(sbx), "cost", time.Since(start))
	start = time.Now()
	for sbx.Status.Info.NodeIP == "" && sbx.Status.Info.SandboxId == "" {
		since := time.Since(start)
		if since > 5*time.Second {
			log.Error(nil, "timeout waiting for node ip", "since", since)
			return nil, fmt.Errorf("timeout waiting for node ip")
		}
		got, err := p.cache.GetSandbox(sbx.GetName())
		if err != nil {
			log.Error(err, "failed to get sandbox from cache")
		} else {
			sbx = got
		}
		time.Sleep(10 * time.Millisecond)
	}
	log.Info("micro-vm created", "sandbox", klog.KObj(sbx), "cost", time.Since(start))
	return p.AsSandbox(sbx), err
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
	allSandboxes := make([]*v1alpha1.Sandbox, 0, total)
	for i := range sandboxList.Items {
		allSandboxes = append(allSandboxes, &sandboxList.Items[i])
		condition, _ := GetSandboxCondition(&sandboxList.Items[i], v1alpha1.SandboxConditionReady)
		if condition.Status == metav1.ConditionTrue {
			log.V(DebugLogLevel).Info("will re-trigger SandboxCreated for existing ready sandbox")
			go p.eventer.Trigger(events.Event{
				Type:    consts.SandboxCreated,
				Sandbox: p.AsSandbox(&sandboxList.Items[i]),
				Message: "OnRestart",
				Source:  "SyncFromCluster",
			})
		}
	}
	return nil
}

func (p *Pool) AsSandbox(sbx *v1alpha1.Sandbox) *Sandbox {
	return &Sandbox{
		BaseSandbox: sandboxcr.BaseSandbox[*v1alpha1.Sandbox]{
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

//// =====================
//// 下面的 Refresh 是顺手写的定时同步集群中 Sandbox 数量信息的函数，没有作用，鸡肋，不舍得删，先注释掉
//// =====================
//func (p *Pool) Refresh(ctx context.Context) error {
//	log := klog.FromContext(ctx).WithValues("pool", p.template.Name).V(DebugLogLevel)
//	sandboxes, err := p.cache.SelectSandboxes(consts.LabelSandboxPool, p.template.Name)
//	if err != nil {
//		log.Error(err, "failed to select sandboxes")
//		return err
//	}
//	groups, err := p.GroupAllSandboxes(ctx, sandboxes)
//	if err != nil {
//		log.Error(err, "failed to group sandboxes")
//		return err
//	}
//	p.SaveStatusFromGroup(groups)
//	log.Info("status saved",
//		"claimed", p.Status.claimed.Load(), "pending", p.Status.pending.Load(),
//		"creating", p.Status.creating.Load(), "total", p.Status.total.Load())
//	return nil
//}
//
//func (p *Pool) SaveStatusFromGroup(groups GroupedSandboxes) (actualReplicas int) {
//	creatingReplicas := len(groups.Creating)
//	pendingReplicas := len(groups.Pending)
//	claimedReplicas := len(groups.Claimed)
//	availableReplicas := pendingReplicas + claimedReplicas
//	actualReplicas = creatingReplicas + availableReplicas
//	p.Status.claimed.Store(int32(claimedReplicas))
//	p.Status.pending.Store(int32(pendingReplicas))
//	p.Status.creating.Store(int32(creatingReplicas))
//	p.Status.total.Store(int32(actualReplicas))
//	return actualReplicas
//}
//
//type GroupedSandboxes struct {
//	Creating []*Sandbox // 容器实例正在创建中的 Sandbox
//	Pending  []*Sandbox // 容器实例已经就绪，但是未被消费的池化 Sandbox
//	Claimed  []*Sandbox // 已经被分配给用户的 Sandbox
//	Failed   []*Sandbox // 由于各种原因需要被删除的 Sandbox，包含删除中的对象
//}
//
//func (p *Pool) GroupAllSandboxes(ctx context.Context, sandboxes []*v1alpha1.Sandbox) (GroupedSandboxes, error) {
//	log := klog.FromContext(ctx).WithValues("pool", p.template.Name)
//	templateHash := p.template.Labels[consts.LabelTemplateHash]
//	groups := GroupedSandboxes{}
//	var unknownSandboxes []*Sandbox
//	for _, s := range sandboxes {
//		debugLog := log.V(DebugLogLevel).WithValues("sandbox", s.Name)
//		group, reason := FindSandboxGroup(s, templateHash)
//		sbx := p.AsSandbox(s)
//		switch group {
//		case sandboxcr.GroupCreating:
//			groups.Creating = append(groups.Creating, sbx)
//		case sandboxcr.GroupPending:
//			groups.Pending = append(groups.Pending, sbx)
//		case sandboxcr.GroupClaimed:
//			groups.Claimed = append(groups.Claimed, sbx)
//		case sandboxcr.GroupFailed:
//			groups.Failed = append(groups.Failed, sbx)
//		default: // unknown
//			return GroupedSandboxes{}, fmt.Errorf("cannot find group for sandbox %s", sbx.Name)
//		}
//		debugLog.Info("sandbox is grouped", "group", group, "reason", reason)
//	}
//	log.Info("sandbox group done", "total", len(sandboxes),
//		"creating", len(groups.Creating), "pending", len(groups.Pending),
//		"claimed", len(groups.Claimed), "failed", len(groups.Failed), "unknown", len(unknownSandboxes))
//	return groups, nil
//}
