package sandboxcr

import (
	"context"
	"fmt"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	sandboxclient "github.com/openkruise/agents/client/clientset/versioned"
	informers "github.com/openkruise/agents/client/informers/externalversions"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/logs"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8scache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type Infra struct {
	infra.BaseInfra

	Cache   Cache[*v1alpha1.Sandbox]
	Client  sandboxclient.Interface
	Eventer *events.Eventer
}

func NewInfra(namespace string, templateDir string, eventer *events.Eventer, client sandboxclient.Interface) (*Infra, error) {
	informerFactory := informers.NewSharedInformerFactoryWithOptions(client, time.Minute*10, informers.WithNamespace(namespace))
	sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
	cache, err := NewCache[*v1alpha1.Sandbox](namespace, informerFactory, sandboxInformer)
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
		AddFunc:    instance.onSandboxCreate,
		UpdateFunc: instance.onSandboxUpdate,
		DeleteFunc: instance.onSandboxDelete,
	})
	return instance, nil
}

func (i *Infra) Run(ctx context.Context) error {
	log := klog.FromContext(ctx)
	done := make(chan struct{})
	defer close(done)
	i.Cache.Run(done)
	<-done
	if err := infra.LoadBuiltinTemplates(ctx, i, i.TemplateDir, i.Namespace); err != nil {
		return err
	}
	var err error
	i.Pools.Range(func(key, value any) bool {
		pool := value.(*Pool)
		go pool.Run()
		err = pool.SyncFromCluster(ctx)
		if err != nil {
			return false
		}
		go func() {
			ticker := time.NewTicker(time.Minute)
			// refresh every 1 minute
			for {
				select {
				case <-ctx.Done():
					ticker.Stop()
					return
				case <-ticker.C:
					if err := pool.Scale(ctx); err != nil {
						log.Error(err, "failed to scale pool", "pool", pool.template.Name)
					}
				}
			}
		}()
		return true
	})
	if err != nil {
		return err
	}
	return nil
}

func (i *Infra) Stop() {
	i.Cache.Stop()
	i.Pools.Range(func(key, value any) bool {
		pool := value.(*Pool)
		pool.Stop()
		return true
	})
}

func (i *Infra) NewPoolFromTemplate(template *infra.SandboxTemplate) infra.SandboxPool {
	return &Pool{
		template: template,
		client:   i.Client,
		cache:    i.Cache,
		eventer:  i.Eventer,
		// 默认 queue size 为 1，后面有需求再做成可配置
		reconcileQueue: make(chan context.Context, 1),
	}
}

func (i *Infra) SelectSandboxes(options infra.SandboxSelectorOptions) ([]infra.Sandbox, error) {
	var expectedStates []string
	if options.WantPaused {
		expectedStates = append(expectedStates, consts.SandboxStatePaused)
	}
	if options.WantPending {
		expectedStates = append(expectedStates, consts.SandboxStatePending)
	}
	if options.WantRunning {
		expectedStates = append(expectedStates, consts.SandboxStateRunning)
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
	selectors = append(selectors, consts.LabelSandboxState, state)
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
		BaseSandbox: BaseSandbox[*v1alpha1.Sandbox]{
			Sandbox:       sbx,
			Cache:         i.Cache,
			PatchSandbox:  i.Client.ApiV1alpha1().Sandboxes(i.Namespace).Patch,
			UpdateStatus:  i.Client.ApiV1alpha1().Sandboxes(i.Namespace).UpdateStatus,
			DeleteFunc:    i.Client.ApiV1alpha1().Sandboxes(i.Namespace).Delete,
			SetCondition:  SetSandboxCondition,
			GetConditions: ListSandboxConditions,
			DeepCopy:      DeepCopy,
		},
		Sandbox: sbx,
	}
}

func (i *Infra) onSandboxCreate(obj any) {
	sbx, ok := obj.(*v1alpha1.Sandbox)
	if !ok {
		return
	}
	if pool, ok := i.GetPoolByObject(sbx); ok {
		ctx := logs.NewContext()
		log := klog.FromContext(ctx).WithValues("pool", pool.GetTemplate().Name).V(consts.DebugLogLevel)
		log.Info("sandbox creation watched", "sbx", klog.KObj(sbx))
		err := pool.(*Pool).Scale(ctx)
		if err != nil {
			log.Error(err, "failed to scale pool")
		}
	}
}

func (i *Infra) onSandboxDelete(obj any) {
	sbx, ok := obj.(*v1alpha1.Sandbox)
	if !ok {
		return
	}
	if pool, ok := i.GetPoolByObject(sbx); ok {
		ctx := logs.NewContext()
		log := klog.FromContext(ctx).WithValues("pool", pool.GetTemplate().Name).V(consts.DebugLogLevel)
		err := pool.(*Pool).Scale(ctx)
		if err != nil {
			log.Error(err, "failed to scale pool")
		}
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
	log := klog.FromContext(ctx).WithValues("pool", pool.GetTemplate().Name, "sandbox", klog.KObj(newSbx)).V(consts.DebugLogLevel)
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
	err := pool.(*Pool).Scale(ctx)
	if err != nil {
		log.Error(err, "failed to scale pool")
	}
}

func (i *Infra) InjectTemplateMetadata() metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Labels: map[string]string{
			consts.LabelACS: "true",
		},
	}
}
