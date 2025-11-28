package microvm

import (
	"context"
	"fmt"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"gitlab.alibaba-inc.com/serverlessinfra/sandbox-operator/api/v1alpha1"
	sandboxclient "gitlab.alibaba-inc.com/serverlessinfra/sandbox-operator/client/clientset/versioned"
	informers "gitlab.alibaba-inc.com/serverlessinfra/sandbox-operator/client/informers/externalversions"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8scache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type Infra struct {
	infra.BaseInfra

	Cache   sandboxcr.Cache[*v1alpha1.Sandbox]
	Client  sandboxclient.Interface
	Eventer *events.Eventer
}

func NewInfra(namespace string, templateDir string, eventer *events.Eventer, client sandboxclient.Interface) (*Infra, error) {
	informerFactory := informers.NewSharedInformerFactoryWithOptions(client, time.Minute*10, informers.WithNamespace(namespace))
	sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
	cache, err := sandboxcr.NewCache[*v1alpha1.Sandbox](namespace, informerFactory, sandboxInformer)
	if err != nil {
		return nil, err
	}

	instance := &Infra{
		BaseInfra: infra.BaseInfra{
			TemplateDir: templateDir,
			Namespace:   namespace,
		},
		Cache:   cache,
		Client:  client,
		Eventer: eventer,
	}

	cache.AddSandboxEventHandler(k8scache.ResourceEventHandlerFuncs{
		UpdateFunc: instance.onSandboxUpdate,
		DeleteFunc: instance.onSandboxDelete,
	})
	return instance, nil
}

func (i *Infra) Run(ctx context.Context) error {
	panic("MicroVM Infra is not ready")
	//log := klog.FromContext(ctx)
	//done := make(chan struct{})
	//defer close(done)
	//i.Cache.Run(done)
	//<-done
	//if err := infra.LoadBuiltinTemplates(ctx, i, i.TemplateDir, i.Namespace); err != nil {
	//	return err
	//}
	//var err error
	//i.Pools.Range(func(key, value any) bool {
	//	pool := value.(*Pool)
	//	err = pool.SyncFromCluster(ctx)
	//	if err != nil {
	//		log.Error(err, "failed to sync pool", "pool", pool.template.Name)
	//		return false
	//	}
	//	return true
	//})
	//return err
}

func (i *Infra) Stop() {
	i.Cache.Stop()
}

func (i *Infra) NewPool(name, namespace string) infra.SandboxPool {
	panic("MicroVM Infra is not ready")
}

func (i *Infra) LoadDebugInfo() map[string]any {
	panic("MicroVM Infra is not ready")
	//infos := make(map[string]any)
	//i.Pools.Range(func(key, value any) bool {
	//	pool := value.(*Pool)
	//	infos[pool.template.Name] = sandboxcr.DebugInfo{
	//		Pending: int(pool.Status.pending.Load()),
	//		Claimed: int(pool.Status.claimed.Load()),
	//		Total:   int(pool.Status.total.Load()),
	//	}
	//	return true
	//})
	//infos["infra"] = consts.InfraMicroVM
	//return infos
}

func (i *Infra) SelectSandboxes(options infra.SandboxSelectorOptions) ([]infra.Sandbox, error) {
	var expectedStates []string
	if options.WantPaused {
		expectedStates = append(expectedStates, agentsv1alpha1.SandboxStatePaused)
	}
	if options.WantAvailable {
		expectedStates = append(expectedStates, agentsv1alpha1.SandboxStateAvailable)
	}
	if options.WantRunning {
		expectedStates = append(expectedStates, agentsv1alpha1.SandboxStateRunning)
	}
	var sandboxes []infra.Sandbox
	for _, state := range expectedStates {
		got, err := i.listSandboxWithState(state, options.Labels)
		if err != nil {
			return nil, err
		}
		sandboxes = append(sandboxes, got...)
	}
	return sandboxes, nil
}

func (i *Infra) listSandboxWithState(state string, labels map[string]string) ([]infra.Sandbox, error) {
	selectors := make([]string, 0, len(labels)+2)
	selectors = append(selectors, agentsv1alpha1.LabelSandboxState, state)
	for k, v := range labels {
		selectors = append(selectors, k, v)
	}
	selected, err := i.Cache.SelectSandboxes(selectors...)
	if err != nil {
		return nil, err
	}
	sandboxes := make([]infra.Sandbox, 0, len(selected))
	for _, s := range selected {
		sandboxes = append(sandboxes, i.AsSandbox(s))
	}
	return sandboxes, nil
}

func (i *Infra) GetSandbox(sandboxID string) (infra.Sandbox, error) {
	sandbox, err := i.Cache.GetSandbox(sandboxID)
	if err != nil {
		return nil, err
	}
	return i.AsSandbox(sandbox), nil
}

func (i *Infra) AsSandbox(sbx *v1alpha1.Sandbox) *Sandbox {
	return &Sandbox{
		BaseSandbox: sandboxcr.BaseSandbox[*v1alpha1.Sandbox]{
			Sandbox:       sbx,
			Cache:         i.Cache,
			PatchSandbox:  i.Client.ApiV1alpha1().Sandboxes(sbx.Namespace).Patch,
			UpdateStatus:  i.Client.ApiV1alpha1().Sandboxes(sbx.Namespace).UpdateStatus,
			DeleteFunc:    i.Client.ApiV1alpha1().Sandboxes(sbx.Namespace).Delete,
			SetCondition:  SetSandboxCondition,
			GetConditions: ListSandboxConditions,
			DeepCopy:      DeepCopy,
		},
		Sandbox: sbx,
	}
}

func (i *Infra) onSandboxDelete(obj any) {
	sbx, ok := obj.(*v1alpha1.Sandbox)
	if !ok {
		return
	}
	if _, ok := i.GetPoolByObject(sbx); ok {
		go i.Eventer.Trigger(events.Event{
			Type:    consts.SandboxKill,
			Sandbox: i.AsSandbox(sbx),
			Source:  "SandboxDeleted",
			Message: fmt.Sprintf("Sandbox %s is deleted", sbx.Name),
		})
	}
	i.Eventer.OnSandboxDelete(i.AsSandbox(sbx))
}

func (i *Infra) onSandboxUpdate(oldObj, newObj any) {
	oldSbx, ok1 := oldObj.(*v1alpha1.Sandbox)
	newSbx, ok2 := newObj.(*v1alpha1.Sandbox)
	if !ok1 || !ok2 {
		return
	}
	pool, ok := i.GetPoolByObject(newSbx)
	if !ok {
		return
	}
	oldCond, _ := GetSandboxCondition(oldSbx, v1alpha1.SandboxConditionReady)
	newCond, _ := GetSandboxCondition(newSbx, v1alpha1.SandboxConditionReady)
	ctx := logs.NewContext()
	log := klog.FromContext(ctx).WithValues("pool", pool.GetName(), "sandbox", klog.KObj(newSbx)).V(DebugLogLevel)
	log.Info("sandbox update watched", "oldCond", oldCond.Status, "newCond", newCond.Status)
	if oldCond.Status != metav1.ConditionTrue && newCond.Status == metav1.ConditionTrue {
		log.Info("sandbox ready condition turns from False to True")
		go i.Eventer.Trigger(events.Event{
			Type:    consts.SandboxCreated,
			Sandbox: i.AsSandbox(newSbx),
			Source:  "SandboxCreated",
			Message: fmt.Sprintf("Sandbox %s is ready", newSbx.Name),
		})
	}
}

func (i *Infra) InjectTemplateMetadata() metav1.ObjectMeta {
	return metav1.ObjectMeta{}
}
