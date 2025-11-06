package k8s

import (
	"fmt"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type Cache struct {
	namespace          string
	informerFactory    informers.SharedInformerFactory
	podInformer        cache.SharedIndexInformer
	deploymentInformer cache.SharedIndexInformer
	stopCh             chan struct{}
}

func NewCache(client kubernetes.Interface, namespace string) (*Cache, error) {
	informerFactory := informers.NewSharedInformerFactoryWithOptions(client, time.Minute*10, informers.WithNamespace(namespace))
	podInformer := informerFactory.Core().V1().Pods().Informer()
	deploymentInformer := informerFactory.Apps().V1().Deployments().Informer()
	if err := utils.AddLabelSelectorIndexerToInformer[*corev1.Pod](podInformer); err != nil {
		return nil, err
	}
	c := &Cache{
		namespace:          namespace,
		informerFactory:    informerFactory,
		podInformer:        podInformer,
		deploymentInformer: deploymentInformer,
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

func (c *Cache) AddDeploymentEventHandler(handler cache.ResourceEventHandlerFuncs) {
	_, err := c.deploymentInformer.AddEventHandler(handler)
	if err != nil {
		panic(err)
	}
}

func (c *Cache) AddPodEventHandler(handler cache.ResourceEventHandlerFuncs) {
	_, err := c.podInformer.AddEventHandler(handler)
	if err != nil {
		panic(err)
	}
}

func (c *Cache) GetDeployment(name string) (*appsv1.Deployment, error) {
	key := c.namespace + "/" + name
	obj, exists, err := c.deploymentInformer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("deployment %s not found in informer cache", key)
	}

	deploy, ok := obj.(*appsv1.Deployment)
	if !ok {
		return nil, fmt.Errorf("object in informer cache is not a deployment")
	}
	return deploy, nil
}

// SelectPods returns managed pods that match the given label selector
func (c *Cache) SelectPods(keysAndValues ...string) ([]*corev1.Pod, error) {
	return utils.SelectObjectFromInformerWithLabelSelector[*corev1.Pod](c.podInformer, c.namespace, keysAndValues...)
}

func (c *Cache) GetPod(name string) (*corev1.Pod, error) {
	key := c.namespace + "/" + name
	obj, exists, err := c.podInformer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("pod %s not found in informer cache", key)
	}

	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, fmt.Errorf("object in informer cache is not a pod")
	}
	return pod, nil
}

func (c *Cache) GetAllPods(namespace string) ([]*corev1.Pod, error) {
	objs, err := c.podInformer.GetIndexer().ByIndex("namespace", namespace)
	if err != nil {
		return nil, err
	}
	pods := make([]*corev1.Pod, 0, len(objs))
	for _, obj := range objs {
		if pod, ok := obj.(*corev1.Pod); ok {
			if pod.Namespace == namespace {
				pods = append(pods, pod)
			}
		}
	}
	return pods, nil
}

func (c *Cache) Refresh() {
	c.informerFactory.WaitForCacheSync(c.stopCh)
}
