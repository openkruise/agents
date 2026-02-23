package sandboxcr

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8sinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	informers "github.com/openkruise/agents/client/informers/externalversions"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	managerutils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
)

type checkFunc func(sbx *agentsv1alpha1.Sandbox) (bool, error)

type Cache struct {
	informerFactory                informers.SharedInformerFactory
	sandboxInformer                cache.SharedIndexInformer
	sandboxSetInformer             cache.SharedIndexInformer
	coreInformerFactory            k8sinformers.SharedInformerFactory
	coreInformerFactorySpecifiedNs k8sinformers.SharedInformerFactory
	persistentVolumeInformer       cache.SharedIndexInformer
	secretInformer                 cache.SharedIndexInformer
	stopCh                         chan struct{}
	waitHooks                      *sync.Map // Key: client.ObjectKey; Value: *waitEntry
}

func NewCache(informerFactory informers.SharedInformerFactory, sandboxInformer, sandboxSetInformer cache.SharedIndexInformer,
	coreInformerFactorySpecifiedNs k8sinformers.SharedInformerFactory, secretInformer cache.SharedIndexInformer,
	coreInformerFactory k8sinformers.SharedInformerFactory, informers ...cache.SharedIndexInformer) (*Cache, error) {
	if err := AddLabelSelectorIndexerToInformer(sandboxInformer); err != nil {
		return nil, err
	}
	c := &Cache{
		informerFactory:    informerFactory,
		sandboxInformer:    sandboxInformer,
		sandboxSetInformer: sandboxSetInformer,
		stopCh:             make(chan struct{}),
		waitHooks:          &sync.Map{},
	}

	// import core informers with specified namespace
	if coreInformerFactorySpecifiedNs != nil {
		c.coreInformerFactorySpecifiedNs = coreInformerFactorySpecifiedNs
		c.secretInformer = secretInformer
	}

	// import core informers with all namespaces
	if coreInformerFactory != nil {
		c.coreInformerFactory = coreInformerFactory
		if len(informers) >= 1 {
			pvInformer := informers[0]
			c.persistentVolumeInformer = pvInformer
		}
	}
	return c, nil
}

func (c *Cache) Run(ctx context.Context) error {
	log := klog.FromContext(ctx)
	_, err := c.sandboxInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			c.watchSandboxSatisfied(newObj)
		},
		AddFunc: func(obj interface{}) {
			c.watchSandboxSatisfied(obj)
		},
		DeleteFunc: func(obj interface{}) {
			c.watchSandboxSatisfied(obj)
		},
	})
	if err != nil {
		log.Error(err, "failed to create waiter handler")
		return err
	}
	c.informerFactory.Start(c.stopCh)
	if c.coreInformerFactory != nil {
		c.coreInformerFactory.Start(c.stopCh)
	}
	if c.coreInformerFactorySpecifiedNs != nil {
		c.coreInformerFactorySpecifiedNs.Start(c.stopCh)
	}
	log.Info("Cache informer started")
	c.informerFactory.WaitForCacheSync(c.stopCh)
	if c.coreInformerFactory != nil {
		c.coreInformerFactory.WaitForCacheSync(c.stopCh)
	}
	if c.coreInformerFactorySpecifiedNs != nil {
		c.coreInformerFactorySpecifiedNs.WaitForCacheSync(c.stopCh)
	}
	log.Info("Cache informer synced")
	return nil
}

func (c *Cache) Stop() {
	close(c.stopCh)
	klog.Info("Cache informer stopped")
}

func (c *Cache) AddSandboxEventHandler(handler cache.ResourceEventHandlerFuncs) {
	_, err := c.sandboxInformer.AddEventHandler(handler)
	if err != nil {
		panic(err)
	}
}

func (c *Cache) ListSandboxWithUser(user string) ([]*agentsv1alpha1.Sandbox, error) {
	return managerutils.SelectObjectWithIndex[*agentsv1alpha1.Sandbox](c.sandboxInformer, IndexUser, user)
}

func (c *Cache) ListAvailableSandboxes(template string) ([]*agentsv1alpha1.Sandbox, error) {
	return managerutils.SelectObjectWithIndex[*agentsv1alpha1.Sandbox](c.sandboxInformer, IndexTemplateAvailable, template)
}

func (c *Cache) GetSandbox(sandboxID string) (*agentsv1alpha1.Sandbox, error) {
	list, err := managerutils.SelectObjectWithIndex[*agentsv1alpha1.Sandbox](c.sandboxInformer, IndexSandboxID, sandboxID)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("sandbox %s not found in cache", sandboxID)
	}
	if len(list) > 1 {
		return nil, fmt.Errorf("multiple sandboxes found with id %s", sandboxID)
	}
	return list[0], nil
}

func (c *Cache) AddSandboxSetEventHandler(handler cache.ResourceEventHandlerFuncs) {
	if c.sandboxSetInformer == nil {
		panic("SandboxSet is not cached")
	}
	_, err := c.sandboxSetInformer.AddEventHandler(handler)
	if err != nil {
		panic(err)
	}
}

func (c *Cache) Refresh() {
	c.informerFactory.WaitForCacheSync(c.stopCh)
}

type WaitAction string

const (
	WaitActionResume        WaitAction = "Resume"
	WaitActionInplaceUpdate WaitAction = "InplaceUpdate"
)

type waitEntry struct {
	ctx       context.Context
	done      chan struct{}
	action    WaitAction
	checker   checkFunc
	closeOnce sync.Once
}

func (c *Cache) WaitForSandboxSatisfied(ctx context.Context, sbx *agentsv1alpha1.Sandbox, action WaitAction,
	satisfiedFunc checkFunc, timeout time.Duration) error {
	key := client.ObjectKeyFromObject(sbx)
	log := klog.FromContext(ctx).V(consts.DebugLogLevel).WithValues("key", key)
	satisfied, err := satisfiedFunc(sbx)
	if satisfied || err != nil {
		log.Info("no need to wait for satisfied", "satisfied", satisfied, "error", err)
		return err
	}
	value, exists := c.waitHooks.LoadOrStore(key, &waitEntry{
		ctx:     ctx,
		done:    make(chan struct{}),
		action:  action,
		checker: satisfiedFunc,
	})
	if exists {
		log.Info("reuse existing wait hook")
	} else {
		log.Info("wait hook created")
	}
	entry := value.(*waitEntry)
	if entry.action != action {
		err := fmt.Errorf("another action(%s)'s wait task already exists", entry.action)
		log.Error(err, "wait hook conflict", "existing", entry.action, "new", action)
		return err
	}

	timer := time.NewTimer(timeout)
	defer func() {
		timer.Stop()
		c.waitHooks.Delete(key)
		log.Info("wait hook deleted")
	}()

	select {
	case <-timer.C:
		log.Info("timeout waiting for sandbox satisfied")
		return c.doubleCheckSandboxSatisfied(ctx, sbx, satisfiedFunc)
	case <-entry.done:
		log.Info("satisfied signal received")
		return c.doubleCheckSandboxSatisfied(ctx, sbx, satisfiedFunc)
	case <-ctx.Done():
		log.Info("context canceled")
		return c.doubleCheckSandboxSatisfied(ctx, sbx, satisfiedFunc)
	}
}

func (c *Cache) doubleCheckSandboxSatisfied(ctx context.Context, sbx *agentsv1alpha1.Sandbox, satisfiedFunc checkFunc) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
	updated, err := c.GetSandbox(sandboxutils.GetSandboxID(sbx))
	if err != nil {
		log.Error(err, "failed to get sandbox while double checking")
		return err
	}
	satisfied, err := satisfiedFunc(updated)
	if err != nil {
		log.Error(err, "failed to check sandbox satisfied")
		return err
	}
	if !satisfied {
		err = fmt.Errorf("sandbox is not satisfied during double check")
		log.Error(err, "sandbox not satisfied")
		return err
	}
	return nil
}

func (c *Cache) watchSandboxSatisfied(obj interface{}) {
	sbx, ok := obj.(*agentsv1alpha1.Sandbox)
	if !ok {
		return
	}
	key := client.ObjectKeyFromObject(sbx)
	value, ok := c.waitHooks.Load(key)
	if !ok {
		return
	}
	entry := value.(*waitEntry)
	log := klog.FromContext(entry.ctx).V(consts.DebugLogLevel).WithValues("key", key)
	satisfied, err := entry.checker(sbx)
	log.Info("watch sandbox satisfied result",
		"satisfied", satisfied, "err", err, "resourceVersion", sbx.GetResourceVersion())
	if satisfied || err != nil {
		entry.closeOnce.Do(func() {
			close(entry.done)
		})
		return
	}
}

// GetPersistentVolume retrieves a PersistentVolume from the cache by name
func (c *Cache) GetPersistentVolume(name string) (*corev1.PersistentVolume, error) {
	obj, exists, err := c.persistentVolumeInformer.GetStore().GetByKey(name)
	if err != nil {
		return nil, fmt.Errorf("failed to get persistentvolume %s from cache: %w", name, err)
	}
	if !exists {
		return nil, fmt.Errorf("persistentvolume %s not found in cache", name)
	}
	if pv, ok := obj.(*corev1.PersistentVolume); ok {
		return pv, nil
	}
	return nil, fmt.Errorf("object with key %s is not a PersistentVolume", name)
}

// GetSecret retrieves a Secret from the cache by namespace and name
func (c *Cache) GetSecret(namespace, name string) (*corev1.Secret, error) {
	key := fmt.Sprintf("%s/%s", namespace, name)
	obj, exists, err := c.secretInformer.GetStore().GetByKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to get secret %s/%s from cache: %w", namespace, name, err)
	}
	if !exists {
		return nil, fmt.Errorf("secret %s/%s not found in cache", namespace, name)
	}
	if secret, ok := obj.(*corev1.Secret); ok {
		return secret, nil
	}
	return nil, fmt.Errorf("object with key %s is not a Secret", key)
}
