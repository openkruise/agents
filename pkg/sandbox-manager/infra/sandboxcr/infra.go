package sandboxcr

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	k8sinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	k8scache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/api/v1alpha1"
	sandboxclient "github.com/openkruise/agents/client/clientset/versioned"
	informers "github.com/openkruise/agents/client/informers/externalversions"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	commonutils "github.com/openkruise/agents/pkg/utils"
	managerutils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
)

type Infra struct {
	Cache  *Cache
	Client sandboxclient.Interface
	Proxy  *proxy.Server

	pickCache sync.Map

	// Currently, templates stores the mapping of sandboxset name -> number of namespaces. For example,
	// if a sandboxset with the same name is created in two different namespaces, the corresponding value would be 2.
	// In the future, this will be changed to integrate with Template CR.
	templates sync.Map
}

func NewInfra(client sandboxclient.Interface, k8sClient kubernetes.Interface, proxy *proxy.Server, systemNamespace string) (*Infra, error) {
	// Create informer factory for custom Sandbox resources
	informerFactory := informers.NewSharedInformerFactory(client, time.Minute*10)
	sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
	sandboxSetInformer := informerFactory.Api().V1alpha1().SandboxSets().Informer()

	// Create informer factory for native Kubernetes resources (PersistentVolume)
	coreInformerFactory := k8sinformers.NewSharedInformerFactory(k8sClient, time.Minute*10)
	persistentVolumeInformer := coreInformerFactory.Core().V1().PersistentVolumes().Informer()
	// Create informer factory with specified namespace for native Kubernetes resources (Secret)
	if systemNamespace == "" {
		systemNamespace = commonutils.DefaultSandboxDeployNamespace
	}
	coreInformerFactorySpecifiedNs := k8sinformers.NewSharedInformerFactoryWithOptions(k8sClient, time.Minute*10, k8sinformers.WithNamespace(systemNamespace))
	// to generate informers only for the specified namespace to avoid potential security privilege escalation risks.
	secretInformer := coreInformerFactorySpecifiedNs.Core().V1().Secrets().Informer()

	// Initialize cache with all required informers
	cache, err := NewCache(informerFactory, sandboxInformer, sandboxSetInformer, coreInformerFactorySpecifiedNs, secretInformer, coreInformerFactory, persistentVolumeInformer)
	if err != nil {
		return nil, err
	}

	instance := &Infra{
		Cache:  cache,
		Client: client,
		Proxy:  proxy,
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

func (i *Infra) Run(ctx context.Context) error {
	return i.Cache.Run(ctx)
}

func (i *Infra) Stop() {
	i.Cache.Stop()
}

func (i *Infra) ClaimSandbox(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, infra.ClaimMetrics, error) {
	log := klog.FromContext(ctx)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	metrics := infra.ClaimMetrics{}

	opts, err := ValidateAndInitClaimOptions(opts)
	if err != nil {
		log.Error(err, "invalid claim options")
		return nil, metrics, err
	}

	metrics.Retries = -1 // starts from 0
	var claimedSandbox infra.Sandbox
	return claimedSandbox, metrics, retry.OnError(wait.Backoff{
		Steps:    int(LockTimeout / RetryInterval),
		Duration: RetryInterval,
		Cap:      LockTimeout,
		Factor:   LockBackoffFactor,
		Jitter:   LockJitter,
	}, func(err error) bool {
		return errors.As(err, &retriableError{})
	}, func() error {
		metrics.Retries++
		log.Info("try to claim sandbox", "retries", metrics.Retries)
		claimed, tryMetrics, claimErr := TryClaimSandbox(ctx, opts, r, &i.pickCache, i.Cache, i.Client)
		if claimErr == nil {
			tryMetrics.Retries = metrics.Retries
			tryMetrics.Wait = metrics.Wait
			tryMetrics.Total += metrics.Wait
			metrics = tryMetrics
			claimedSandbox = claimed
		}
		metrics.Wait += tryMetrics.Total
		return claimErr
	})
}

func (i *Infra) GetCache() infra.CacheProvider {
	return i.Cache
}

func (i *Infra) HasTemplate(name string) bool {
	_, exists := i.templates.Load(name)
	return exists
}

func (i *Infra) SelectSandboxes(user string, limit int, filter func(sandbox infra.Sandbox) bool) ([]infra.Sandbox, error) {
	objects, err := i.Cache.ListSandboxWithUser(user)
	if err != nil {
		return nil, err
	}
	var sandboxes []infra.Sandbox
	for _, obj := range objects {
		if !managerutils.ResourceVersionExpectationSatisfied(obj) {
			continue
		}
		sbx := AsSandbox(obj, i.Cache, i.Client)
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
	if !managerutils.ResourceVersionExpectationSatisfied(sandbox) {
		klog.FromContext(ctx).Info("resource version expectation not satisfied, will request APIServer directly")
		sandbox, err = i.Client.ApiV1alpha1().Sandboxes(sandbox.Namespace).Get(ctx, sandbox.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
	}
	return AsSandbox(sandbox, i.Cache, i.Client), nil
}

func (i *Infra) onSandboxAdd(obj any) {
	sbx, ok := obj.(*v1alpha1.Sandbox)
	if !ok {
		return
	}
	if !i.HasTemplate(GetTemplateFromSandbox(sbx)) {
		return
	}
	route := AsSandbox(sbx, i.Cache, i.Client).GetRoute()
	i.Proxy.SetRoute(route)
	managerutils.ResourceVersionExpectationObserve(sbx)
}

func (i *Infra) onSandboxDelete(obj any) {
	sbx, ok := obj.(*v1alpha1.Sandbox)
	if !ok {
		return
	}
	i.Proxy.DeleteRoute(stateutils.GetSandboxID(sbx))
	managerutils.ResourceVersionExpectationDelete(sbx)
}

func (i *Infra) onSandboxUpdate(_, newObj any) {
	newSbx, ok := newObj.(*v1alpha1.Sandbox)
	if !ok {
		return
	}
	if !i.HasTemplate(GetTemplateFromSandbox(newSbx)) {
		return
	}
	i.refreshRoute(AsSandbox(newSbx, i.Cache, i.Client))
	managerutils.ResourceVersionExpectationObserve(newSbx)
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
	ctx := logs.NewContext("sandboxset", klog.KObj(newSbs))
	log := klog.FromContext(ctx)
	log.Info("sandboxset creation watched")
	for {
		got, loaded := i.templates.LoadOrStore(newSbs.Name, int32(1))
		if !loaded { // stored, the first one
			log.Info("template created")
			return
		}
		old := got.(int32)
		if i.templates.CompareAndSwap(newSbs.Name, old, old+1) {
			log.Info("template count updated", "cnt", old+1)
			break
		}
	}
}

func (i *Infra) onSandboxSetDelete(obj interface{}) {
	sbs, ok := obj.(*v1alpha1.SandboxSet)
	if !ok {
		return
	}
	ctx := logs.NewContext("sandboxset", klog.KObj(sbs))
	log := klog.FromContext(ctx)
	log.Info("sandboxset deletion watched")
	for {
		got, loaded := i.templates.Load(sbs.Name)
		if !loaded { // not exist
			log.Info("template does not exist")
			return
		}
		if old := got.(int32); old == 1 { // the last one is deleted
			if i.templates.CompareAndDelete(sbs.Name, old) {
				log.Info("template deleted")
				break
			}
		} else { // more than one exist
			if i.templates.CompareAndSwap(sbs.Name, old, old-1) {
				log.Info("template count decreased", "cnt", old-1)
				break
			}
		}
	}
}

func GetTemplateFromSandbox(sbx metav1.Object) string {
	tmpl := sbx.GetLabels()[v1alpha1.LabelSandboxTemplate]
	if tmpl == "" {
		tmpl = sbx.GetLabels()[v1alpha1.LabelSandboxPool]
	}
	return tmpl
}
