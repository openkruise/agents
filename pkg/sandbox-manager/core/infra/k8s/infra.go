package k8s

import (
	"context"
	"fmt"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/logs"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	k8scache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type Infra struct {
	infra.BaseInfra

	Eventer *events.Eventer

	Cache      *Cache
	Client     kubernetes.Interface
	restConfig *rest.Config
}

// NewInfra Creates a new sandbox Pool Manager
func NewInfra(namespace string, templateDir string, eventer *events.Eventer, client kubernetes.Interface, restConfig *rest.Config) (*Infra, error) {

	cache, err := NewCache(client, namespace)
	if err != nil {
		return nil, err
	}

	instance := &Infra{
		BaseInfra: infra.BaseInfra{
			Namespace:   namespace,
			TemplateDir: templateDir,
		},
		Eventer:    eventer,
		Cache:      cache,
		Client:     client,
		restConfig: restConfig,
	}

	cache.AddDeploymentEventHandler(k8scache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			instance.handleDeploymentUpdate(oldObj, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			instance.handleDeploymentDelete(obj)
		},
	})

	cache.AddPodEventHandler(k8scache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			instance.handlePodUpdate(oldObj, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			instance.handlePodDelete(obj)
		},
	})

	return instance, nil
}

func (i *Infra) Run(ctx context.Context) error {
	done := make(chan struct{})
	defer close(done)
	i.Cache.Run(done)
	<-done
	if err := infra.LoadBuiltinTemplates(ctx, i, i.TemplateDir, i.Namespace); err != nil {
		return err
	}
	if err := i.syncWithCluster(ctx); err != nil {
		return err
	}
	klog.FromContext(ctx).Info("Pool manager synced with cluster")

	return nil
}

func (i *Infra) Stop() {
	return
}

func (i *Infra) NewPoolFromTemplate(template *infra.SandboxTemplate) infra.SandboxPool {
	return &Pool{
		template: template,
		client:   i.Client,
		cache:    i.Cache,
		eventer:  i.Eventer,
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

func (i *Infra) GetSandbox(sandboxID string) (infra.Sandbox, error) {
	pod, err := i.Cache.GetPod(sandboxID)
	if err != nil {
		return nil, err
	}
	return i.AsSandbox(pod), nil
}

func (i *Infra) handlePodUpdate(oldObj, newObj interface{}) {
	ctx := logs.NewContext()
	oldPod, ok1 := oldObj.(*corev1.Pod)
	newPod, ok2 := newObj.(*corev1.Pod)
	if !ok1 || !ok2 {
		return
	}

	if pool, ok := i.GetPoolByObject(newPod); ok {
		pool.(*Pool).onPodUpdate(ctx, oldPod, newPod)
	}
}

func (i *Infra) handlePodDelete(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		tombstone, ok := obj.(k8scache.DeletedFinalStateUnknown)
		if !ok {
			klog.ErrorS(nil, "Error decoding object, invalid type")
			return
		}
		pod, ok = tombstone.Obj.(*corev1.Pod)
		if !ok {
			klog.ErrorS(nil, "Error decoding object tombstone, invalid type")
			return
		}
	}
	if pool, ok := i.GetPoolByObject(pod); ok {
		pool.(*Pool).onPodDelete(pod)
	}
	i.Eventer.OnSandboxDelete(i.AsSandbox(pod))
}

func (i *Infra) handleDeploymentUpdate(_, newObj interface{}) {
	newDeploy, ok := newObj.(*appsv1.Deployment)
	if !ok {
		return
	}
	pool, ok := i.GetPoolByTemplate(newDeploy.Name)
	if !ok {
		return
	}
	pool.(*Pool).onDeploymentStatusUpdate(newDeploy)
}

func (i *Infra) handleDeploymentDelete(obj interface{}) {
	deploy, ok := obj.(*appsv1.Deployment)
	if !ok {
		return
	}
	i.Pools.Delete(deploy.Name)
}

func (i *Infra) syncWithCluster(ctx context.Context) error {
	log := klog.FromContext(ctx)
	var err error
	i.Pools.Range(func(key, value any) bool {
		pool := value.(*Pool)
		if syncErr := pool.SyncWithCluster(ctx); syncErr != nil {
			err = fmt.Errorf("failed to sync pool %s: %v", pool.template.Name, syncErr)
			return false
		}
		go func(pool *Pool) {
			ticker := time.NewTicker(time.Minute)
			// refresh every 1 minute
			for {
				select {
				case <-ctx.Done():
					ticker.Stop()
					return
				case <-ticker.C:
					if err := pool.Refresh(ctx); err != nil {
						log.Error(err, "failed to refresh pool", "pool", pool.template.Name)
					}
					if err := pool.Scale(ctx); err != nil {
						log.Error(err, "failed to scale pool", "pool", pool.template.Name)
					}
				}
			}
		}(pool)
		return true
	})
	return err
}

func (i *Infra) AsSandbox(pod *corev1.Pod) *Sandbox {
	return &Sandbox{
		Pod:    pod,
		Client: i.Client,
		Cache:  i.Cache,
	}
}

func (i *Infra) InjectTemplateMetadata() metav1.ObjectMeta {
	return metav1.ObjectMeta{}
}

func (i *Infra) listSandboxWithState(state string, labels map[string]string) ([]infra.Sandbox, error) {
	selectors := make([]string, 0, len(labels)+2)
	selectors = append(selectors, consts.LabelSandboxState, state)
	for k, v := range labels {
		selectors = append(selectors, k, v)
	}
	pods, err := i.Cache.SelectPods(selectors...)
	if err != nil {
		return nil, err
	}
	sandboxes := make([]infra.Sandbox, 0, len(pods))
	for _, pod := range pods {
		sandboxes = append(sandboxes, i.AsSandbox(pod))
	}
	return sandboxes, nil
}
