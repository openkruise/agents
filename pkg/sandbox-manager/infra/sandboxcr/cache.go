package sandboxcr

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	informers "github.com/openkruise/agents/client/informers/externalversions"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/utils"
	managerutils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
)

type checkFunc[T client.Object] func(sbx T) (bool, error)
type updateFunc[T client.Object] func(obj T) (T, error)

type Cache struct {
	client                         *clients.ClientSet
	informerFactory                informers.SharedInformerFactory
	k8sInformerFactory             k8sinformers.SharedInformerFactory
	k8sInformerFactoryWithSystemNs k8sinformers.SharedInformerFactory
	sandboxInformer                cache.SharedIndexInformer
	sandboxSetInformer             cache.SharedIndexInformer
	checkpointInformer             cache.SharedIndexInformer
	sandboxTemplateInformer        cache.SharedIndexInformer
	persistentVolumeInformer       cache.SharedIndexInformer
	secretInformer                 cache.SharedIndexInformer
	configmapInformer              cache.SharedIndexInformer
	stopCh                         chan struct{}
	waitHooks                      *sync.Map // Key: client.ObjectKey; Value: *waitEntry
	listSandboxesGroup             singleflight.Group
}

func NewCache(client *clients.ClientSet, opts config.SandboxManagerOptions) (*Cache, error) {
	// Create informer factory for custom Sandbox resources
	informerOptions := []informers.SharedInformerOption{}
	if opts.SandboxNamespace != "" {
		informerOptions = append(informerOptions, informers.WithNamespace(opts.SandboxNamespace))
	}
	if opts.SandboxLabelSelector != "" {
		informerOptions = append(informerOptions, informers.WithTweakListOptions(func(lo *metav1.ListOptions) {
			lo.LabelSelector = opts.SandboxLabelSelector
		}))
	}
	informerFactory := informers.NewSharedInformerFactoryWithOptions(client.SandboxClient, time.Minute*10, informerOptions...)
	sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
	sandboxSetInformer := informerFactory.Api().V1alpha1().SandboxSets().Informer()
	checkpointInformer := informerFactory.Api().V1alpha1().Checkpoints().Informer()
	sandboxTemplateInformer := informerFactory.Api().V1alpha1().SandboxTemplates().Informer()

	// Create informer factory for native Kubernetes resources (PersistentVolume)
	k8sInformerFactory := k8sinformers.NewSharedInformerFactory(client.K8sClient, time.Minute*10)
	persistentVolumeInformer := k8sInformerFactory.Core().V1().PersistentVolumes().Informer()

	// Create informer factory with specified namespace for native Kubernetes resources (Secret)
	k8sInformerFactoryWithSystemNs := k8sinformers.NewSharedInformerFactoryWithOptions(client.K8sClient, time.Minute*10, k8sinformers.WithNamespace(opts.SystemNamespace))
	// to generate informers only for the specified namespace to avoid potential security privilege escalation risks.
	secretInformer := k8sInformerFactoryWithSystemNs.Core().V1().Secrets().Informer()
	configmapInformer := k8sInformerFactoryWithSystemNs.Core().V1().ConfigMaps().Informer()

	if err := AddIndexersToSandboxInformer(sandboxInformer); err != nil {
		return nil, err
	}
	if err := AddIndexersToSandboxSetInformer(sandboxSetInformer); err != nil {
		return nil, err
	}
	if err := AddIndexersToCheckpointInformer(checkpointInformer); err != nil {
		return nil, err
	}
	c := &Cache{
		client:                         client,
		informerFactory:                informerFactory,
		k8sInformerFactory:             k8sInformerFactory,
		k8sInformerFactoryWithSystemNs: k8sInformerFactoryWithSystemNs,
		secretInformer:                 secretInformer,
		configmapInformer:              configmapInformer,
		sandboxInformer:                sandboxInformer,
		sandboxSetInformer:             sandboxSetInformer,
		checkpointInformer:             checkpointInformer,
		sandboxTemplateInformer:        sandboxTemplateInformer,
		persistentVolumeInformer:       persistentVolumeInformer,
		stopCh:                         make(chan struct{}),
		waitHooks:                      &sync.Map{},
	}
	return c, nil
}

func NewTestCache(t *testing.T) (*Cache, *clients.ClientSet, error) {
	return NewTestCacheWithOptions(t, config.SandboxManagerOptions{
		SystemNamespace: utils.DefaultSandboxDeployNamespace,
	})
}

func NewTestCacheWithOptions(t *testing.T, opts config.SandboxManagerOptions) (*Cache, *clients.ClientSet, error) {
	t.Helper()
	clientSet := clients.NewFakeClientSet(t)
	c, err := NewCache(clientSet, opts)
	if err != nil {
		return nil, nil, err
	}
	return c, clientSet, c.Run(t.Context())
}

func (c *Cache) Run(ctx context.Context) error {
	log := klog.FromContext(ctx)
	if err := addWaiterHandler[*agentsv1alpha1.Sandbox](c, c.sandboxInformer); err != nil {
		log.Error(err, "failed to create sandbox waiter handler")
		return err
	}
	if err := addWaiterHandler[*agentsv1alpha1.Checkpoint](c, c.checkpointInformer); err != nil {
		log.Error(err, "failed to create checkpoint waiter handler")
		return err
	}
	c.informerFactory.Start(c.stopCh)
	c.k8sInformerFactory.Start(c.stopCh)
	c.k8sInformerFactoryWithSystemNs.Start(c.stopCh)
	log.Info("Cache informer started")

	// Wait for all informers to sync
	if !cache.WaitForCacheSync(c.stopCh,
		c.sandboxInformer.HasSynced,
		c.sandboxSetInformer.HasSynced,
		c.sandboxTemplateInformer.HasSynced,
		c.persistentVolumeInformer.HasSynced,
		c.secretInformer.HasSynced,
		c.configmapInformer.HasSynced,
		c.checkpointInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
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

func (c *Cache) ListSandboxesInPool(template string) ([]*agentsv1alpha1.Sandbox, error) {
	result, err, _ := c.listSandboxesGroup.Do(template, func() (any, error) {
		return managerutils.SelectObjectWithIndex[*agentsv1alpha1.Sandbox](c.sandboxInformer, IndexSandboxPool, template)
	})
	if err != nil {
		return nil, err
	}
	return result.([]*agentsv1alpha1.Sandbox), nil
}

func (c *Cache) GetClaimedSandbox(sandboxID string) (*agentsv1alpha1.Sandbox, error) {
	list, err := managerutils.SelectObjectWithIndex[*agentsv1alpha1.Sandbox](c.sandboxInformer, IndexClaimedSandboxID, sandboxID)
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

// GetSandboxSet gets a SandboxSet with given name randomly
func (c *Cache) GetSandboxSet(name string) (*agentsv1alpha1.SandboxSet, error) {
	list, err := managerutils.SelectObjectWithIndex[*agentsv1alpha1.SandboxSet](c.sandboxSetInformer, IndexTemplateID, name)
	if err != nil {
		return nil, fmt.Errorf("failed to get sandboxset %s from cache: %w", name, err)
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("sandboxset %s not found in cache", name)
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
	WaitActionResume     WaitAction = "Resume"
	WaitActionPause      WaitAction = "Pause"
	WaitActionWaitReady  WaitAction = "WaitReady"
	WaitActionCheckpoint WaitAction = "Checkpoint"
)

type waitEntry[T client.Object] struct {
	ctx       context.Context
	done      chan struct{}
	action    WaitAction
	checker   checkFunc[T]
	closeOnce sync.Once
}

func addWaiterHandler[T client.Object](c *Cache, informer cache.SharedIndexInformer) error {
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			watchObjectSatisfied[T](c, newObj)
		},
		AddFunc: func(obj interface{}) {
			watchObjectSatisfied[T](c, obj)
		},
		DeleteFunc: func(obj interface{}) {
			watchObjectSatisfied[T](c, obj)
		},
	})
	return err
}

func (c *Cache) WaitForSandboxSatisfied(ctx context.Context, sbx *agentsv1alpha1.Sandbox, action WaitAction,
	satisfiedFunc checkFunc[*agentsv1alpha1.Sandbox], timeout time.Duration) error {

	ns := sbx.Namespace
	sandboxName := sbx.Name
	sandboxID := sandboxutils.GetSandboxID(sbx)

	return waitForObjectSatisfied[*agentsv1alpha1.Sandbox](ctx, c, sbx, action,
		func(_ *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
			got, err := c.GetClaimedSandbox(sandboxID)
			if err == nil {
				return got, nil
			}

			return c.client.SandboxClient.ApiV1alpha1().Sandboxes(ns).Get(ctx, sandboxName, metav1.GetOptions{})
		},
		satisfiedFunc, timeout)
}

func (c *Cache) refreshCheckpoint(cp *agentsv1alpha1.Checkpoint) (*agentsv1alpha1.Checkpoint, error) {
	item, exists, err := c.checkpointInformer.GetStore().Get(cp)
	if err != nil {
		return nil, fmt.Errorf("failed to get checkpoint %s from cache: %w", cp.Name, err)
	}
	if !exists {
		return nil, fmt.Errorf("checkpoint %s not found in cache", cp.Name)
	}
	return item.(*agentsv1alpha1.Checkpoint), nil
}

func (c *Cache) WaitForCheckpointSatisfied(ctx context.Context, checkpoint *agentsv1alpha1.Checkpoint, action WaitAction,
	satisfiedFunc checkFunc[*agentsv1alpha1.Checkpoint], timeout time.Duration) (*agentsv1alpha1.Checkpoint, error) {
	err := waitForObjectSatisfied[*agentsv1alpha1.Checkpoint](ctx, c, checkpoint, action, c.refreshCheckpoint, satisfiedFunc, timeout)
	if err != nil {
		return nil, err
	}
	return c.refreshCheckpoint(checkpoint)
}

func waitForObjectSatisfied[T client.Object](ctx context.Context, c *Cache, obj T, action WaitAction,
	update updateFunc[T], satisfiedFunc checkFunc[T], timeout time.Duration) error {
	key := client.ObjectKeyFromObject(obj)
	log := klog.FromContext(ctx).V(consts.DebugLogLevel).WithValues("key", key)
	satisfied, err := satisfiedFunc(obj)
	if satisfied || err != nil {
		log.Info("no need to wait for satisfied", "satisfied", satisfied, "error", err)
		return err
	}
	if timeout <= 0 {
		log.Info("waiting is skipped due to zero timeout")
		return fmt.Errorf("sandbox is not satisfied")
	}
	value, exists := c.waitHooks.LoadOrStore(key, &waitEntry[T]{
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
	entry := value.(*waitEntry[T])
	if entry.action != action {
		err := fmt.Errorf("another action(%s)'s wait task already exists", entry.action)
		log.Error(err, "wait hook conflict", "existing", entry.action, "new", action)
		return err
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer func() {
		cancel()
		c.waitHooks.Delete(key)
		log.Info("wait hook deleted")
	}()

	select {
	case <-entry.done:
		log.Info("satisfied signal received")
		return doubleCheckObjectSatisfied(ctx, obj, update, satisfiedFunc)
	case <-waitCtx.Done():
		log.Info("stop waiting for sandbox satisfied: context canceled", "reason", waitCtx.Err())
		return doubleCheckObjectSatisfied(ctx, obj, update, satisfiedFunc)
	}
}

func doubleCheckObjectSatisfied[T client.Object](ctx context.Context, obj T, update updateFunc[T], satisfiedFunc checkFunc[T]) error {
	log := klog.FromContext(ctx).WithValues("object", klog.KObj(obj))
	updated, err := update(obj)
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

func watchObjectSatisfied[T client.Object](c *Cache, obj interface{}) {
	typedObj, ok := obj.(T)
	if !ok {
		return
	}
	key := client.ObjectKeyFromObject(typedObj)
	value, ok := c.waitHooks.Load(key)
	if !ok {
		return
	}
	entry, ok := value.(*waitEntry[T])
	if !ok {
		return
	}
	log := klog.FromContext(entry.ctx).V(consts.DebugLogLevel).WithValues("key", key)
	satisfied, err := entry.checker(typedObj)
	log.Info("watch sandbox satisfied result",
		"satisfied", satisfied, "err", err, "resourceVersion", typedObj.GetResourceVersion())
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

// GetConfigmap retrieves a Configmap from the cache by namespace and name
func (c *Cache) GetConfigmap(namespace, name string) (*corev1.ConfigMap, error) {
	key := fmt.Sprintf("%s/%s", namespace, name)
	obj, exists, err := c.configmapInformer.GetStore().GetByKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to get configmap %s/%s from cache: %v", namespace, name, err)
	}
	if !exists {
		return nil, fmt.Errorf("configmap %s/%s not found in cache", namespace, name)
	}
	if configmap, ok := obj.(*corev1.ConfigMap); ok {
		return configmap, nil
	}
	return nil, fmt.Errorf("object with key %s is not a Configmap object", key)
}

func (c *Cache) GetCheckpoint(checkpointID string) (*agentsv1alpha1.Checkpoint, error) {
	list, err := managerutils.SelectObjectWithIndex[*agentsv1alpha1.Checkpoint](c.checkpointInformer, IndexCheckpointID, checkpointID)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("checkpoint %s not found in cache", checkpointID)
	}
	if len(list) > 1 {
		return nil, fmt.Errorf("multiple checkpoints found with id %s", checkpointID)
	}
	return list[0], nil
}

func (c *Cache) GetSandboxTemplate(namespace, name string) (*agentsv1alpha1.SandboxTemplate, error) {
	key := fmt.Sprintf("%s/%s", namespace, name)
	obj, exists, err := c.sandboxTemplateInformer.GetStore().GetByKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to get sandboxtemplate %s/%s from cache: %w", namespace, name, err)
	}
	if !exists {
		return nil, fmt.Errorf("sandboxtemplate %s/%s not found in cache", namespace, name)
	}
	if template, ok := obj.(*agentsv1alpha1.SandboxTemplate); ok {
		return template, nil
	}
	return nil, fmt.Errorf("object with key %s is not a SandboxTemplate", key)
}
