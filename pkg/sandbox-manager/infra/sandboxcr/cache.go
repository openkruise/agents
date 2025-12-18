package sandboxcr

import (
	"context"
	"fmt"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	informers "github.com/openkruise/agents/client/informers/externalversions"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/utils"
	managerutils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type checkFunc func(sbx *agentsv1alpha1.Sandbox) (bool, error)

type Cache struct {
	informerFactory    informers.SharedInformerFactory
	sandboxInformer    cache.SharedIndexInformer
	sandboxSetInformer cache.SharedIndexInformer
	stopCh             chan struct{}
}

func NewCache(informerFactory informers.SharedInformerFactory, sandboxInformer, sandboxSetInformer cache.SharedIndexInformer) (*Cache, error) {
	if err := AddLabelSelectorIndexerToInformer(sandboxInformer); err != nil {
		return nil, err
	}
	c := &Cache{
		informerFactory:    informerFactory,
		sandboxInformer:    sandboxInformer,
		sandboxSetInformer: sandboxSetInformer,
		stopCh:             make(chan struct{}),
	}
	return c, nil
}

func (c *Cache) Run(done chan<- struct{}) {
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

func (c *Cache) ListAvailableSandboxes(pool string) ([]*agentsv1alpha1.Sandbox, error) {
	return managerutils.SelectObjectWithIndex[*agentsv1alpha1.Sandbox](c.sandboxInformer, IndexPoolAvailable, pool)
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

type satisfiedResult struct {
	ok  bool
	err error
}

func (c *Cache) WaitForSandboxSatisfied(ctx context.Context, key client.ObjectKey, satisfiedFunc checkFunc, timeout time.Duration) error {
	log := klog.FromContext(ctx).V(consts.DebugLogLevel)
	ch := make(chan satisfiedResult, 1)
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	handler, err := c.sandboxInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			watchSandboxSatisfied(ctx, key, ch, satisfiedFunc, newObj)
		},
		AddFunc: func(obj interface{}) {
			watchSandboxSatisfied(ctx, key, ch, satisfiedFunc, obj)
		},
		DeleteFunc: func(obj interface{}) {
			watchSandboxSatisfied(ctx, key, ch, satisfiedFunc, obj)
		},
	})
	log.Info("temp event handler added to wait for sandbox satisfied")

	if err != nil {
		return err
	}

	defer func() {
		if err := c.sandboxInformer.RemoveEventHandler(handler); err != nil {
			log.Error(err, "failed to remove sandbox event handler")
		} else {
			log.Info("temp event handler removed")
		}
	}()

	select {
	case <-timer.C:
		return fmt.Errorf("timeout waiting for sandbox satisfied")
	case result := <-ch:
		if result.err != nil {
			return result.err
		}
		return nil
	}
}

func watchSandboxSatisfied(ctx context.Context, key client.ObjectKey, ch chan satisfiedResult, satisfiedFunc checkFunc, obj interface{}) {
	log := klog.FromContext(ctx).V(consts.DebugLogLevel)
	sbx, ok := obj.(*agentsv1alpha1.Sandbox)
	if !ok {
		return
	}
	gotKey := client.ObjectKeyFromObject(sbx)
	if gotKey != key {
		return
	}
	satisfied, err := satisfiedFunc(sbx)
	log.Info("watch sandbox satisfied result", "sandbox", gotKey, "satisfied", satisfied,
		"err", err, "resourceVersion", sbx.GetResourceVersion())
	if err != nil {
		utils.WriteChannelSafely(ch, satisfiedResult{false, err})
		return
	}
	if satisfied {
		utils.WriteChannelSafely(ch, satisfiedResult{true, nil})
		return
	}
}
