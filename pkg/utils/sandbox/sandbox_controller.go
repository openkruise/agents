package sandbox

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned"
	informer "github.com/openkruise/agents/client/informers/externalversions/api/v1alpha1"
	lister "github.com/openkruise/agents/client/listers/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

var (
	controllerKind  = v1alpha1.SchemeGroupVersion.WithKind("Sandbox")
	errKindNotFound = fmt.Errorf("kind not found in group version resources")
	backOff         = wait.Backoff{
		Steps:    4,
		Duration: 500 * time.Millisecond,
		Factor:   5.0,
		Jitter:   0.1,
	}
)

const (
	controllerName = "virtual-kubelet/sandbox-controller"
)

type Instance struct {
	InstanceId  string
	Namespace   string
	Name        string
	UUID        string
	Annotations map[string]string
}

// Sandbox 休眠时，Pod不存在，但是 PLM Instance 存在，这个 Controller 是用于 VK 调用，实现上述情况的 Instance 清理。
type Controller struct {
	workQueue workqueue.RateLimitingInterface

	sandboxListerSynced cache.InformerSynced
	sandboxLister       lister.SandboxLister
	kubeClient          versioned.Interface
	discoveryClient     discovery.DiscoveryInterface

	// vk function
	deleteInstanceFunc     DeleteInstance
	listPausedInstanceFunc ListPausedInstance
}

// DeleteInstance 删除 Instance 回调函数，能够保持幂等性，可以删除多次。
type DeleteInstance func(ctx context.Context, acsId string) (deleted bool, err error)

// ListPausedInstance 获取 Paused 或 Pausing 状态Instance的回调函数
// TODO, 当前只考虑都是sandbox的场景，如果有非sandbox的使用界面，这个方法可能会误删除用户的Instance。
type ListPausedInstance func(ctx context.Context) ([]Instance, error)

type SandboxControllerFuncs struct {
	DeleteFn DeleteInstance
	ListFn   ListPausedInstance
}

func NewSandboxController(kubeClient versioned.Interface, sandboxInformer informer.SandboxInformer, funcs SandboxControllerFuncs) (*Controller, error) {
	ctrl := &Controller{
		kubeClient:             kubeClient,
		workQueue:              workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(1*time.Second, 5*time.Second), "sandbox-queue"),
		deleteInstanceFunc:     funcs.DeleteFn,
		listPausedInstanceFunc: funcs.ListFn,
	}

	_, err := sandboxInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			box := obj.(*v1alpha1.Sandbox)
			ctrl.queueSandbox(box)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			box := newObj.(*v1alpha1.Sandbox)
			ctrl.queueSandbox(box)
		},
		DeleteFunc: func(obj interface{}) {
			box := obj.(*v1alpha1.Sandbox)
			ctrl.queueSandbox(box)
		},
	})
	if err != nil {
		return nil, err
	}

	ctrl.sandboxLister = sandboxInformer.Lister()
	ctrl.sandboxListerSynced = sandboxInformer.Informer().HasSynced
	return ctrl, nil
}

func (ctrl *Controller) queueSandbox(box *v1alpha1.Sandbox) {
	if box.DeletionTimestamp.IsZero() || !containsFinalizer(box, utils.SandboxFinalizer) {
		return
	}
	if value, ok := box.Status.PodInfo.Annotations[utils.PodAnnotationAcsInstanceId]; !ok || value == "" {
		return
	}
	if value, ok := box.Annotations[utils.SandboxAnnotationEnableVKDeleteInstance]; !ok || value != "true" {
		return
	}
	namespace := box.Namespace
	name := box.Name
	if namespace == "" {
		namespace = "default"
	}
	ctrl.workQueue.Add(fmt.Sprintf("%s/%s", namespace, name))
}

func (ctrl *Controller) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer ctrl.workQueue.ShuttingDown()

	klog.Infof("Check whether CRD sandbox.agents.kruise.io exist")
	if exist := ctrl.waitForSandboxExist(stopCh); !exist {
		return
	}

	klog.Infoln("Starting sandbox controller ...")
	defer klog.Infoln("Shutting down sandbox controller ...")
	if ok := cache.WaitForNamedCacheSync(controllerName, stopCh, ctrl.sandboxListerSynced); !ok {
		return
	}
	klog.Infof("Finished sandbox controller wait for cache synced")

	// gc dangling instance
	go ctrl.gcDanglingInstances(stopCh)

	// Start workers.
	for i := 0; i < workers; i++ {
		go wait.Until(ctrl.worker, 2*time.Second, stopCh)
	}
	<-stopCh
}

// waitForSandboxExist 检查 v1alpha1.Sandbox 是否存在
// 如果存在，立即返回 true；
// 如果不存在，则每 5 秒重试一次，直到 stopCh 被关闭。
func (ctrl *Controller) waitForSandboxExist(stopCh <-chan struct{}) bool {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		if exist := ctrl.discoverGVK(controllerKind); exist {
			return true
		}
		// 等待下一次检查，或被 stopCh 中断
		select {
		case <-stopCh:
			return false // 被主动停止
		case <-ticker.C:
			// 继续下一轮循环
		}
	}
}

func (ctrl *Controller) worker() {
	for {
		key, quit := ctrl.workQueue.Get()
		if quit {
			return
		}
		done, err := ctrl.handleSandboxDeletion(key.(string))
		if err != nil || !done {
			ctrl.workQueue.AddRateLimited(key)
		} else {
			ctrl.workQueue.Forget(key)
		}
		ctrl.workQueue.Done(key)
	}
}

// HandleSandboxDeletion 用于处理 Sandbox Update 事件
// 1. 调用 PLM 接口删除 Instance 实例
// 2. 去掉 sandbox finalizer
func (ctrl *Controller) handleSandboxDeletion(key string) (done bool, err error) {
	ctx := context.TODO()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("")
		return true, nil
	}
	box, err := ctrl.sandboxLister.Sandboxes(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			return true, nil
		}
		klog.Errorf("Get sandbox(%s) failed: %s", key, err.Error())
		return false, err
	}
	// 回调函数：调用 PLM 接口删除 Instance 实例
	// 异常情况：如果 acsId 已经不存在了，也不要报错
	acsId := box.Status.PodInfo.Annotations[utils.PodAnnotationAcsInstanceId]
	deleted, err := ctrl.deleteInstanceFunc(ctx, acsId)
	if err != nil {
		klog.Errorf("deleteInstanceFunc instance(%s) failed: %s", acsId, err.Error())
		return false, err
	} else if !deleted {
		klog.V(3).Infof("deleteInstanceFunc instance(%s) is deleting", acsId)
		return false, nil
	}

	removeFinalizer(box, utils.SandboxFinalizer)
	_, err = ctrl.kubeClient.ApiV1alpha1().Sandboxes(box.Namespace).Update(ctx, box, metav1.UpdateOptions{})
	if err != nil {
		klog.Errorf("update sandbox(%s) finalizer failed: %s", key, err.Error())
		return false, err
	}
	klog.Infof("remove sandbox(%s) finalizer success", key)
	return true, nil
}

const GcDanglingInstanceThread = 100

func (ctrl *Controller) gcDanglingInstances(stopCh <-chan struct{}) {
	klog.Infof("start gc sandbox dangling instance")
	// reconcile periodically to avold undeleted eci pods
	reconcileTick := time.NewTicker(20 * time.Minute)
	go func() {
		// Perform a reconciliation step that deletes any dangling pods from the provider.
		// This happens only when the virtual-kubelet is starting, and operates on a "best-effort" basis.
		// If by any reason the provider fails to delete a dangling pod, it will stay in the provider and deletion won't be retried.
		ctrl.deleteDanglingInstances()
		for {
			select {
			case <-stopCh:
				reconcileTick.Stop()
				return
			case <-reconcileTick.C:
				ctrl.deleteDanglingInstances()
			}
		}
	}()
}

// deleteDanglingInstances checks whether the provider knows about any pods which Kubernetes doesn't know about, and deletes them.
func (ctrl *Controller) deleteDanglingInstances() {
	ctx := context.TODO()
	// Grab the list of pods known to the provider.
	instances, err := ctrl.listPausedInstanceFunc(ctx)
	if err != nil {
		klog.Errorf("listPausedInstanceFunc failed: %s", err.Error())
		return
	}

	// Create a slice to hold the pods we will be deleting from the provider.
	var deleteInstanceList []Instance
	var deleteInstanceIDs []string
	// Iterate over the pods known to the provider, marking for deletion those that don't exist in Kubernetes.
	// Take on this opportunity to populate the list of key that correspond to pods known to the provider.
	for i := range instances {
		instance := instances[i]
		instanceId := instance.InstanceId
		box, err := ctrl.sandboxLister.Sandboxes(instance.Namespace).Get(instance.Name)
		if err != nil {
			if !errors.IsNotFound(err) {
				// For some reason we couldn't fetch the pod from the lister, so we propagate the error.
				klog.Errorf("get sandbox(%s/%s) failed: %s", instance.Namespace, instance.Name, err.Error())
				continue
			}
			// The current pod does not exist in Kubernetes, so we mark it for deletion.
			deleteInstanceList = append(deleteInstanceList, instance)
			deleteInstanceIDs = append(deleteInstanceIDs, instance.InstanceId)
			continue
		}

		// The current sandbox exists in kubernetes, check it.
		if k8sInstanceId := box.Status.PodInfo.Annotations[utils.PodAnnotationAcsInstanceId]; k8sInstanceId != "" && k8sInstanceId != instanceId {
			// This is duplicated and leaked eci with same pod name
			ctrl.deleteInstanceFunc(ctx, instanceId)
			klog.Infof("Delete duplicated and leaked eci instance %s %s %s", instanceId, instance.Namespace, instance.Name)
			continue
		}
	}
	if len(deleteInstanceList) == 0 {
		return
	}

	klog.Infof("[deleteDanglingPods] Need clean instanceIds: %#v", deleteInstanceIDs)
	// We delete each instance in its own goroutine, allowing a maximum of "threadiness" concurrent deletions.
	limiter := rate.NewLimiter(100, 100)
	semaphore := make(chan struct{}, GcDanglingInstanceThread)
	var wg sync.WaitGroup
	for _, instance := range deleteInstanceList {
		limiter.Wait(ctx)
		semaphore <- struct{}{}

		wg.Add(1)
		go func(ctx context.Context, instance Instance) {
			defer func() {
				wg.Done()
				<-semaphore
			}()

			// delete the eci instance.
			_, err := ctrl.deleteInstanceFunc(ctx, instance.InstanceId)
			if err != nil {
				klog.Errorf("failed to delete pod(acsId=%s) in provider, err %v", instance.InstanceId, err)
			} else {
				klog.Infof("deleted leaked pod(acsId=%s) in provider success", instance.InstanceId)
			}
		}(ctx, instance)
	}
	// Wait for all pods to be deleted.
	wg.Wait()
}

func containsFinalizer(box *v1alpha1.Sandbox, finalizer string) bool {
	f := box.GetFinalizers()
	for _, e := range f {
		if e == finalizer {
			return true
		}
	}
	return false
}

func removeFinalizer(box *v1alpha1.Sandbox, finalizer string) {
	f := box.GetFinalizers()
	length := len(f)

	index := 0
	for i := 0; i < length; i++ {
		if f[i] == finalizer {
			continue
		}
		f[index] = f[i]
		index++
	}
	box.SetFinalizers(f[:index])
}

func (ctrl *Controller) discoverGVK(gvk schema.GroupVersionKind) bool {
	startTime := time.Now()
	err := retry.OnError(backOff, func(err error) bool { return true }, func() error {
		resourceList, err := ctrl.discoveryClient.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
		if err != nil {
			return err
		}
		for _, r := range resourceList.APIResources {
			if r.Kind == gvk.Kind {
				return nil
			}
		}
		return errKindNotFound
	})

	if err != nil {
		if err == errKindNotFound {
			klog.InfoS("Not found kind in group version", "kind", gvk.Kind, "groupVersion", gvk.GroupVersion().String(), "cost", time.Since(startTime))
			return false
		}

		// This might be caused by abnormal apiserver or etcd, ignore it
		klog.ErrorS(err, "Failed to find resources in group version", "groupVersion", gvk.GroupVersion().String(), "cost", time.Since(startTime))
	}

	return true
}
