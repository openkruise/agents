package sandboxcr

import (
	"fmt"
	"reflect"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/utils"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// SharedInformerFactory 是简化的 Sandbox SharedInformerFactory 接口，用于统一各分支 Sandbox CR 实现
type SharedInformerFactory interface {
	// Start initializes all requested informers. They are handled in goroutines
	// which run until the stop channel gets closed.
	// Warning: Start does not block. When run in a go-routine, it will race with a later WaitForCacheSync.
	Start(stopCh <-chan struct{})

	// WaitForCacheSync blocks until all started informers' caches were synced
	// or the stop channel gets closed.
	WaitForCacheSync(stopCh <-chan struct{}) map[reflect.Type]bool
}

type cacheImpl[T SandboxCR] struct {
	namespace       string
	informerFactory SharedInformerFactory
	sandboxInformer cache.SharedIndexInformer
	stopCh          chan struct{}
}

func NewCache[T SandboxCR](
	namespace string, informerFactory SharedInformerFactory, sandboxInformer cache.SharedIndexInformer) (Cache[T], error) {

	if err := utils.AddLabelSelectorIndexerToInformer[T](sandboxInformer); err != nil {
		return nil, err
	}
	c := &cacheImpl[T]{
		namespace:       namespace,
		informerFactory: informerFactory,
		sandboxInformer: sandboxInformer,
		stopCh:          make(chan struct{}),
	}
	return c, nil
}

func (c *cacheImpl[T]) Run(done chan<- struct{}) {
	c.informerFactory.Start(c.stopCh)
	klog.Info("Cache informer started")
	go func() {
		c.informerFactory.WaitForCacheSync(c.stopCh)
		if done != nil {
			done <- struct{}{}
		}
		klog.Info("Cache informer synced")
	}()
}

func (c *cacheImpl[T]) Stop() {
	close(c.stopCh)
	klog.Info("Cache informer stopped")
}

func (c *cacheImpl[T]) AddSandboxEventHandler(handler cache.ResourceEventHandlerFuncs) {
	_, err := c.sandboxInformer.AddEventHandler(handler)
	if err != nil {
		panic(err)
	}
}

// SelectSandboxes returns managed pods that match the given label selector
func (c *cacheImpl[T]) SelectSandboxes(keysAndValues ...string) ([]T, error) {
	return utils.SelectObjectFromInformerWithLabelSelector[T](
		c.sandboxInformer, c.namespace, keysAndValues...)
}

func (c *cacheImpl[T]) GetSandbox(name string) (T, error) {
	key := c.namespace + "/" + name
	obj, exists, err := c.sandboxInformer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("object %s not found in informer cache", key)
	}

	sandbox, ok := obj.(T)
	if !ok {
		return nil, fmt.Errorf("object in informer cache is not a sandbox")
	}
	return sandbox, nil
}

func (c *cacheImpl[T]) GetAllSandboxes() ([]T, error) {
	objs, err := c.sandboxInformer.GetIndexer().ByIndex("namespace", c.namespace)
	if err != nil {
		return nil, err
	}
	sandboxes := make([]T, 0, len(objs))
	for _, obj := range objs {
		if sbx, ok := obj.(T); ok {
			sandboxes = append(sandboxes, sbx)
		}
	}
	return sandboxes, nil
}

func (c *cacheImpl[T]) Refresh() {
	c.informerFactory.WaitForCacheSync(c.stopCh)
}

type Cache[T SandboxCR] interface {
	Run(done chan<- struct{})
	Stop()
	AddSandboxEventHandler(handler cache.ResourceEventHandlerFuncs)
	SelectSandboxes(keysAndValues ...string) ([]T, error)
	GetSandbox(name string) (T, error)
	GetAllSandboxes() ([]T, error)
	Refresh()
}
