package microvm

import (
	"context"
	"sync/atomic"

	"github.com/openkruise/agents/pkg/sandbox-manager/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	sandboxcr2 "github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"gitlab.alibaba-inc.com/serverlessinfra/sandbox-operator/api/v1alpha1"
	sandboxclient "gitlab.alibaba-inc.com/serverlessinfra/sandbox-operator/client/clientset/versioned"
)

var DebugLogLevel = 5

type PoolStatus struct {
	creating atomic.Int32 // creating sandboxes
	pending  atomic.Int32 // pods available to be claimed
	claimed  atomic.Int32 // claimed running sandboxes
	total    atomic.Int32
}
type Pool struct {
	client  sandboxclient.Interface
	cache   sandboxcr2.Cache[*v1alpha1.Sandbox]
	eventer *events.Eventer

	Status PoolStatus
}

func (p *Pool) GetAnnotations() map[string]string {
	panic("MicroVM Infra is not ready")
}

func (p *Pool) GetName() string {
	panic("MicroVM Infra is not ready")
}

func (p *Pool) ClaimSandbox(ctx context.Context, user string, modifier func(sbx infra.Sandbox)) (infra.Sandbox, error) {
	panic("MicroVM Infra is not ready")
	//spec, ok := MicroSandboxSpecMap[p.template.Name]
	//if !ok {
	//	return nil, fmt.Errorf("micro-vm template %s not found", p.template.Name)
	//}
	//var err error
	//var sbx *v1alpha1.Sandbox
	//log := klog.FromContext(ctx).WithValues("pool", p.template.Name).V(DebugLogLevel)
	//lock := uuid.New().String()
	//start := time.Now()
	//for i := 0; i < 10; i++ {
	//	log.Info("will try to create sandbox", "retries", i)
	//	generatedName := fmt.Sprintf("%s-%s-%s",
	//		p.template.Name, p.template.Labels[agentsv1alpha1.LabelTemplateHash], rand.String(5))
	//	instance := &v1alpha1.Sandbox{
	//		ObjectMeta: metav1.ObjectMeta{
	//			Name:        generatedName,
	//			Namespace:   p.template.Namespace,
	//			Labels:      p.template.Spec.Template.Labels,
	//			Annotations: p.template.Spec.Template.Annotations,
	//		},
	//		// MicroVM 还在 POC 中，这里只能写死
	//		Spec: v1alpha1.SandboxSpec{
	//			Template: spec,
	//		},
	//	}
	//	sandbox := p.AsSandbox(instance)
	//	if modifier != nil {
	//		modifier(sandbox)
	//	}
	//	sandbox.Labels[agentsv1alpha1.LabelSandboxState] = agentsv1alpha1.SandboxStateRunning
	//	sandbox.Annotations[agentsv1alpha1.AnnotationLock] = lock
	//	sandbox.Annotations[agentsv1alpha1.AnnotationOwner] = user
	//	sbx, err = p.client.ApiV1alpha1().Sandboxes(p.template.Namespace).Create(ctx, sandbox.Sandbox, metav1.CreateOptions{})
	//	if err == nil || !apierrors.IsAlreadyExists(err) {
	//		break
	//	}
	//}
	//if err != nil {
	//	log.Error(err, "failed to create sandbox after retries")
	//	return nil, err
	//}
	//// wait for node ip
	//log.Info("sandbox created, waiting micro-vm creation", "sandbox", klog.KObj(sbx), "cost", time.Since(start))
	//start = time.Now()
	//for sbx.Status.Info.NodeIP == "" && sbx.Status.Info.SandboxId == "" {
	//	since := time.Since(start)
	//	if since > 5*time.Second {
	//		log.Error(nil, "timeout waiting for node ip", "since", since)
	//		return nil, fmt.Errorf("timeout waiting for node ip")
	//	}
	//	got, err := p.cache.GetSandbox(sbx.GetName())
	//	if err != nil {
	//		log.Error(err, "failed to get sandbox from cache")
	//	} else {
	//		sbx = got
	//	}
	//	time.Sleep(10 * time.Millisecond)
	//}
	//log.Info("micro-vm created", "sandbox", klog.KObj(sbx), "cost", time.Since(start))
	//return p.AsSandbox(sbx), err
}

func (p *Pool) SyncFromCluster(ctx context.Context) error {
	panic("MicroVM Infra is not ready")
	//log := klog.FromContext(ctx).WithValues("pool", p.template.Name)
	//sandboxList, err := p.client.ApiV1alpha1().Sandboxes(p.template.Namespace).List(ctx, metav1.ListOptions{
	//	LabelSelector: fmt.Sprintf("%s=%s", agentsv1alpha1.LabelSandboxPool, p.template.Name),
	//})
	//if err != nil {
	//	log.Error(err, "list sandboxes failed")
	//	return err
	//}
	//total := int32(len(sandboxList.Items))
	//log.Info("existing sandboxes listed", "total", total)
	//allSandboxes := make([]*v1alpha1.Sandbox, 0, total)
	//for i := range sandboxList.Items {
	//	allSandboxes = append(allSandboxes, &sandboxList.Items[i])
	//	condition, _ := GetSandboxCondition(&sandboxList.Items[i], v1alpha1.SandboxConditionReady)
	//	if condition.Status == metav1.ConditionTrue {
	//		log.V(DebugLogLevel).Info("will re-trigger SandboxCreated for existing ready sandbox")
	//		go p.eventer.Trigger(events.Event{
	//			Type:    consts.SandboxCreated,
	//			Sandbox: p.AsSandbox(&sandboxList.Items[i]),
	//			Message: "OnRestart",
	//			Source:  "SyncWithCluster",
	//		})
	//	}
	//}
	//return nil
}

func (p *Pool) AsSandbox(sbx *v1alpha1.Sandbox) *Sandbox {
	panic("MicroVM Infra is not ready")
	//return &Sandbox{
	//	BaseSandbox: sandboxcr2.BaseSandbox[*v1alpha1.Sandbox]{
	//		Sandbox:       sbx,
	//		Cache:         p.cache,
	//		PatchSandbox:  p.client.ApiV1alpha1().Sandboxes(p.template.Namespace).Patch,
	//		UpdateStatus:  p.client.ApiV1alpha1().Sandboxes(p.template.Namespace).UpdateStatus,
	//		DeleteFunc:    p.client.ApiV1alpha1().Sandboxes(p.template.Namespace).Delete,
	//		SetCondition:  SetSandboxCondition,
	//		GetConditions: ListSandboxConditions,
	//		DeepCopy:      DeepCopy,
	//	},
	//	Sandbox: sbx,
	//}
}
