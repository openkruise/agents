package sandboxcr

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
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
	managerutils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
)

type Infra struct {
	Cache  *Cache
	Client sandboxclient.Interface
	Proxy  *proxy.Server

	// For claiming sandbox
	pickCache        sync.Map
	claimLockChannel chan struct{}

	// Currently, templates stores the mapping of sandboxset name -> number of namespaces. For example,
	// if a sandboxset with the same name is created in two different namespaces, the corresponding value would be 2.
	// In the future, this will be changed to integrate with Template CR.
	templates sync.Map

	reconcileRouteStopCh chan struct{}
}

func NewInfra(client sandboxclient.Interface, k8sClient kubernetes.Interface, proxy *proxy.Server, opts config.SandboxManagerOptions) (*Infra, error) {
	// Create informer factory for custom Sandbox resources
	informerFactory := informers.NewSharedInformerFactory(client, time.Minute*10)
	sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
	sandboxSetInformer := informerFactory.Api().V1alpha1().SandboxSets().Informer()

	// Create informer factory for native Kubernetes resources (PersistentVolume)
	coreInformerFactory := k8sinformers.NewSharedInformerFactory(k8sClient, time.Minute*10)
	persistentVolumeInformer := coreInformerFactory.Core().V1().PersistentVolumes().Informer()
	// Create informer factory with specified namespace for native Kubernetes resources (Secret)
	coreInformerFactorySpecifiedNs := k8sinformers.NewSharedInformerFactoryWithOptions(k8sClient, time.Minute*10, k8sinformers.WithNamespace(opts.SystemNamespace))
	// to generate informers only for the specified namespace to avoid potential security privilege escalation risks.
	secretInformer := coreInformerFactorySpecifiedNs.Core().V1().Secrets().Informer()

	// Initialize cache with all required informers
	cache, err := NewCache(informerFactory, sandboxInformer, sandboxSetInformer, coreInformerFactorySpecifiedNs, secretInformer, coreInformerFactory, persistentVolumeInformer)
	if err != nil {
		return nil, err
	}

	instance := &Infra{
		Cache:                cache,
		Client:               client,
		Proxy:                proxy,
		reconcileRouteStopCh: make(chan struct{}),
		claimLockChannel:     make(chan struct{}, opts.MaxClaimWorkers),
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

	// Start route reconciler to handle missed delete events
	go instance.startRouteReconciler(RouteReconcileInterval)

	return instance, nil
}

func (i *Infra) Run(ctx context.Context) error {
	return i.Cache.Run(ctx)
}

func (i *Infra) Stop() {
	close(i.reconcileRouteStopCh)
	i.Cache.Stop()
}

func (i *Infra) ClaimSandbox(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, infra.ClaimMetrics, error) {
	log := klog.FromContext(ctx)
	metrics := infra.ClaimMetrics{}

	opts, err := ValidateAndInitClaimOptions(opts)
	if err != nil {
		log.Error(err, "invalid claim options")
		return nil, metrics, err
	}

	claimCtx, cancel := context.WithTimeout(ctx, opts.ClaimTimeout)
	defer cancel()

	// Start claiming sandbox
	log.V(consts.DebugLogLevel).Info("claim sandbox options", "options", opts)
	metrics.Retries = -1 // starts from 0
	var claimedSandbox infra.Sandbox
	err = retry.OnError(wait.Backoff{
		Steps:    int(opts.ClaimTimeout / RetryInterval),
		Duration: RetryInterval,
		Factor:   LockBackoffFactor,
		Jitter:   LockJitter,
	}, func(err error) bool {
		return errors.As(err, &retriableError{})
	}, func() error {
		metrics.Retries++
		log.Info("try to claim sandbox", "retries", metrics.Retries)
		claimed, tryMetrics, claimErr := TryClaimSandbox(claimCtx, opts, &i.pickCache, i.Cache, i.Client, i.claimLockChannel)
		metrics.Total += tryMetrics.Total
		metrics.Wait += tryMetrics.Wait
		metrics.PickAndLock += tryMetrics.PickAndLock
		metrics.WaitReady += tryMetrics.WaitReady
		metrics.InitRuntime += tryMetrics.InitRuntime
		metrics.CSIMount += tryMetrics.CSIMount
		if tryMetrics.LastError != nil {
			metrics.LastError = tryMetrics.LastError
		}
		if claimErr == nil {
			claimedSandbox = claimed
		} else {
			metrics.RetryCost += tryMetrics.Total
		}
		return claimErr
	})
	return claimedSandbox, metrics, buildClaimError(err, metrics.LastError)
}

func buildClaimError(err error, lastError error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%v, last error: %v", err, lastError)
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
	i.Proxy.SetRoute(logs.NewContext(), route)
	managerutils.ResourceVersionExpectationObserve(sbx)
}

func (i *Infra) onSandboxDelete(obj any) {
	sbx, ok := obj.(*v1alpha1.Sandbox)
	if !ok {
		return
	}
	sandboxID := stateutils.GetSandboxID(sbx)
	i.Proxy.DeleteRoute(sandboxID)
	klog.InfoS("sandbox route deleted", "sandboxID", sandboxID)
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
		i.Proxy.SetRoute(logs.NewContext(), newRoute)
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

const (
	// RouteReconcileInterval is the interval for route reconciliation
	RouteReconcileInterval = 5 * time.Minute
)

// startRouteReconciler periodically reconciles routes to clean up orphaned entries
// that might be left due to missed delete events from Kubernetes informer
func (i *Infra) startRouteReconciler(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			i.reconcileRoutes()
		case <-i.reconcileRouteStopCh:
			klog.Info("route reconciler stopped")
			return
		}
	}
}

// reconcileRoutes compares routes in Proxy with Sandboxes in Cache
// and deletes orphaned routes that no longer have corresponding Sandboxes
func (i *Infra) reconcileRoutes() {
	ctx := logs.NewContext()
	log := klog.FromContext(ctx)
	log.Info("starting route reconciliation")
	// Build set of existing sandbox IDs from cache
	existingSandboxIDs := make(map[string]struct{})

	sandboxList := i.Cache.sandboxInformer.GetStore().List()
	for _, obj := range sandboxList {
		sbx, ok := obj.(*v1alpha1.Sandbox)
		if !ok {
			continue
		}
		sandboxID := stateutils.GetSandboxID(sbx)
		existingSandboxIDs[sandboxID] = struct{}{}
	}

	// Check all routes and delete orphaned ones
	routes := i.Proxy.ListRoutes()
	deletedCount := 0
	for _, route := range routes {
		if _, exists := existingSandboxIDs[route.ID]; !exists {
			i.Proxy.DeleteRoute(route.ID)
			deletedCount++
			managerutils.ResourceVersionExpectationDelete(&metav1.ObjectMeta{
				UID: route.UID,
			})
			log.Info("reconciler deleted orphaned route", "sandboxID", route.ID)
		}
	}

	if deletedCount > 0 {
		log.Info("route reconciliation completed", "orphanedRoutesDeleted", deletedCount, "totalRoutes", len(routes))
	}
}
