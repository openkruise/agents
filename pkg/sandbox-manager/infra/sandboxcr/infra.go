package sandboxcr

import (
	"context"
	"fmt"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	sandboxclient "github.com/openkruise/agents/client/clientset/versioned"
	informers "github.com/openkruise/agents/client/informers/externalversions"
	consts "github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/events"
	infra "github.com/openkruise/agents/pkg/sandbox-manager/infra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8scache "k8s.io/client-go/tools/cache"
)

type Infra struct {
	infra.BaseInfra

	Cache   Cache[*v1alpha1.Sandbox]
	Client  sandboxclient.Interface
	Eventer *events.Eventer
}

func NewInfra(namespace string, eventer *events.Eventer, client sandboxclient.Interface) (*Infra, error) {
	informerFactory := informers.NewSharedInformerFactoryWithOptions(client, time.Minute*10, informers.WithNamespace(namespace))
	sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
	sandboxSetInformer := informerFactory.Api().V1alpha1().SandboxSets().Informer()
	cache, err := NewCache[*v1alpha1.Sandbox](namespace, informerFactory, sandboxInformer, sandboxSetInformer)
	if err != nil {
		return nil, err
	}

	instance := &Infra{
		BaseInfra: infra.BaseInfra{},
		Cache:     cache,
		Client:    client,
		Eventer:   eventer,
	}

	cache.AddSandboxEventHandler(k8scache.ResourceEventHandlerFuncs{
		DeleteFunc: instance.onSandboxDelete,
	})

	cache.AddSandboxSetEventHandler(k8scache.ResourceEventHandlerFuncs{
		AddFunc:    instance.onSandboxSetCreate,
		DeleteFunc: instance.onSandboxSetDelete,
	})
	return instance, nil
}

func (i *Infra) Run(context.Context) error {
	done := make(chan struct{})
	defer close(done)
	i.Cache.Run(done)
	<-done
	return nil
}

func (i *Infra) Stop() {
	i.Cache.Stop()
}

func (i *Infra) NewPool(name, namespace string) infra.SandboxPool {
	return &Pool{
		Name:      name,
		Namespace: namespace,
		client:    i.Client,
		cache:     i.Cache,
		eventer:   i.Eventer,
	}
}

func (i *Infra) SelectSandboxes(options infra.SandboxSelectorOptions) ([]infra.Sandbox, error) {
	var expectedStates []string
	if options.WantPaused {
		expectedStates = append(expectedStates, v1alpha1.SandboxStatePaused)
	}
	if options.WantAvailable {
		expectedStates = append(expectedStates, v1alpha1.SandboxStateAvailable)
	}
	if options.WantRunning {
		expectedStates = append(expectedStates, v1alpha1.SandboxStateRunning)
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
	selectors = append(selectors, v1alpha1.LabelSandboxState, state)
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
	if _, ok = i.GetPoolByObject(sbx); ok {
		go i.Eventer.Trigger(events.Event{
			Type:    consts.SandboxKill,
			Sandbox: i.AsSandbox(sbx),
			Source:  "SandboxDeleted",
			Message: fmt.Sprintf("Sandbox %s is deleted", sbx.Name),
		})
	}
	i.Eventer.OnSandboxDelete(i.AsSandbox(sbx))
}

func (i *Infra) onSandboxSetCreate(newObj interface{}) {
	newSbs, ok := newObj.(*v1alpha1.SandboxSet)
	if !ok {
		return
	}
	pool, ok := i.GetPoolByTemplate(newSbs.Name)
	if !ok {
		pool = i.NewPool(newSbs.Name, newSbs.Namespace)
		i.AddPool(newSbs.Name, pool)
	}
}

func (i *Infra) onSandboxSetDelete(obj interface{}) {
	sbs, ok := obj.(*v1alpha1.SandboxSet)
	if !ok {
		return
	}
	i.Pools.Delete(sbs.Name)
}

func (i *Infra) InjectTemplateMetadata() metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Labels: map[string]string{
			consts.LabelACS: "true",
		},
	}
}
