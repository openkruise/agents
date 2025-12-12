package sandboxcr

import (
	"context"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	sandboxclient "github.com/openkruise/agents/client/clientset/versioned"
	informers "github.com/openkruise/agents/client/informers/externalversions"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8scache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type Infra struct {
	infra.BaseInfra

	Cache  *Cache
	Client sandboxclient.Interface
	Proxy  *proxy.Server
}

func NewInfra(client sandboxclient.Interface, proxy *proxy.Server) (*Infra, error) {
	informerFactory := informers.NewSharedInformerFactory(client, time.Minute*10)
	sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
	sandboxSetInformer := informerFactory.Api().V1alpha1().SandboxSets().Informer()
	cache, err := NewCache(informerFactory, sandboxInformer, sandboxSetInformer)
	if err != nil {
		return nil, err
	}

	instance := &Infra{
		BaseInfra: infra.BaseInfra{},
		Cache:     cache,
		Client:    client,
		Proxy:     proxy,
	}

	cache.AddSandboxEventHandler(k8scache.ResourceEventHandlerFuncs{
		AddFunc:    instance.onSandboxAdd,
		DeleteFunc: instance.onSandboxDelete,
		UpdateFunc: instance.onSandboxUpdate,
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

func (i *Infra) NewPool(name, namespace string, annotations map[string]string) infra.SandboxPool {
	return &Pool{
		Name:        name,
		Namespace:   namespace,
		Annotations: annotations,
		client:      i.Client,
		cache:       i.Cache,
	}
}

func (i *Infra) SelectSandboxes(user string, limit int, filter func(sandbox infra.Sandbox) bool) ([]infra.Sandbox, error) {
	objects, err := i.Cache.ListSandboxWithUser(user)
	if err != nil {
		return nil, err
	}
	var sandboxes []infra.Sandbox
	for _, obj := range objects {
		if !utils.ResourceVersionExpectationSatisfied(obj) {
			continue
		}
		sbx := i.AsSandbox(obj)
		if filter == nil || filter(sbx) {
			sandboxes = append(sandboxes, sbx)
		}
		if len(sandboxes) >= limit {
			break
		}
	}
	return sandboxes, nil
}

func (i *Infra) GetSandbox(ctx context.Context, sandboxID string) (infra.Sandbox, error) {
	sandbox, err := i.Cache.GetSandbox(sandboxID)
	if err != nil {
		return nil, err
	}
	if !utils.ResourceVersionExpectationSatisfied(sandbox) {
		klog.FromContext(ctx).Info("resource version expectation not satisfied, will request APIServer directly")
		sandbox, err = i.Client.ApiV1alpha1().Sandboxes(sandbox.Namespace).Get(ctx, sandbox.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
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
			Update:        i.Client.ApiV1alpha1().Sandboxes(sbx.Namespace).Update,
			DeleteFunc:    i.Client.ApiV1alpha1().Sandboxes(sbx.Namespace).Delete,
			SetCondition:  SetSandboxCondition,
			GetConditions: ListSandboxConditions,
			DeepCopy:      DeepCopy,
		},
		Sandbox: sbx,
	}
}

func (i *Infra) onSandboxAdd(obj any) {
	sbx, ok := obj.(*v1alpha1.Sandbox)
	if !ok {
		return
	}
	_, ok = i.GetPoolByObject(sbx)
	if !ok {
		return
	}
	route := i.AsSandbox(sbx).GetRoute()
	i.Proxy.SetRoute(route)
	utils.ResourceVersionExpectationObserve(sbx)
}

func (i *Infra) onSandboxDelete(obj any) {
	sbx, ok := obj.(*v1alpha1.Sandbox)
	if !ok {
		return
	}
	i.Proxy.DeleteRoute(sbx.Name)
	utils.ResourceVersionExpectationDelete(sbx)
}

func (i *Infra) onSandboxUpdate(_, newObj any) {
	newSbx, ok := newObj.(*v1alpha1.Sandbox)
	if !ok {
		return
	}
	_, ok = i.GetPoolByObject(newSbx)
	if !ok {
		return
	}
	i.refreshRoute(i.AsSandbox(newSbx))
}

func (i *Infra) refreshRoute(sbx infra.Sandbox) {
	oldRoute, _ := i.Proxy.LoadRoute(sbx.GetName())
	newRoute := sbx.GetRoute()
	if newRoute.State != oldRoute.State || newRoute.IP != oldRoute.IP {
		i.Proxy.SetRoute(newRoute)
	}
}

func (i *Infra) onSandboxSetCreate(newObj interface{}) {
	newSbs, ok := newObj.(*v1alpha1.SandboxSet)
	if !ok {
		return
	}
	_, ok = i.GetPoolByTemplate(newSbs.Name)
	if !ok {
		pool := i.NewPool(newSbs.Name, newSbs.Namespace, newSbs.Annotations)
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
