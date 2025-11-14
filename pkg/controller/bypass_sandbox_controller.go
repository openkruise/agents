package controller

import (
	"context"
	"fmt"
	"strconv"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// BypassPodReconciler 用于处理通过更新 pod 上的深休眠协议 annotation 触发旁路 Sandbox 创建的语法糖
type BypassPodReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// Reconcile 用于将启用了旁路 Sandbox 语法糖的 Pod 上的 sandbox-pause annotation 同步到同名 Sandbox 资源的 spec.paused 上
func (r *BypassPodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	pod := &corev1.Pod{}
	if err := r.Get(ctx, req.NamespacedName, pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log := logf.FromContext(ctx).WithValues("pod", klog.KObj(pod))
	if !NeedsBypassSandbox(pod) {
		log.Info("skip process for pod doesn't need bypass sandbox")
		return ctrl.Result{}, nil
	}
	box := &agentsv1alpha1.Sandbox{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: pod.Name}, box)
	if apierrors.IsNotFound(err) {
		log.Info("will create sandbox for pod")
		return ctrl.Result{Requeue: true}, r.createSandbox(ctx, pod)
	}
	if err != nil {
		log.Error(err, "get sandbox failed")
		return ctrl.Result{}, err
	}
	log.Info("will patch sandbox and pod")
	return ctrl.Result{}, r.patchSandboxAndPod(ctx, pod, box)
}

func (r *BypassPodReconciler) createSandbox(ctx context.Context, newPod *corev1.Pod) error {
	log := logf.FromContext(ctx).WithValues("pod", klog.KObj(newPod))
	clone := newPod.DeepCopy()
	clone.ManagedFields = nil

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clone.Name,
			Namespace: clone.Namespace,
			Annotations: map[string]string{
				utils.SandboxAnnotationDisablePodCreation: utils.True,
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: clone.ObjectMeta,
				Spec:       clone.Spec,
			},
		},
	}

	if newPod.Annotations[utils.PodAnnotationDeleteOnPaused] != utils.True {
		box.Annotations[utils.SandboxAnnotationDisablePodDeletion] = utils.True
	}

	if controller := metav1.GetControllerOf(newPod); controller != nil {
		box.OwnerReferences = []metav1.OwnerReference{*controller}
	}

	if err := r.Client.Create(ctx, box); err != nil {
		log.Error(err, "create Sandbox failed")
		return err
	}

	r.Recorder.Event(newPod, corev1.EventTypeNormal, "SandboxCreated",
		"create sandbox resource successfully")
	return nil
}

func (r *BypassPodReconciler) patchSandboxAndPod(ctx context.Context, pod *corev1.Pod, box *agentsv1alpha1.Sandbox) error {
	log := logf.FromContext(ctx).WithValues("pod", klog.KObj(pod), "sandbox", klog.KObj(box))
	if pod.Annotations[utils.PodAnnotationCreatedBy] == "" {
		err := r.Client.Patch(ctx, pod, client.RawPatch(types.MergePatchType, []byte(fmt.Sprintf(
			`{"metadata":{"annotations":{"%s":"%s"}}}`, utils.PodAnnotationCreatedBy, utils.CreatedByExternal))))
		if err != nil {
			log.Error(err, "patch pod created-by annotation failed")
			return err
		}
		log.Info("patch pod created-by annotation success")
	}

	var expectPaused bool
	if pod.Annotations[utils.PodAnnotationRecreating] == utils.True {
		// 重建中的 Pod，强制设置 paused=false
		expectPaused = false
	} else {
		expectPaused = pod.Annotations[utils.PodAnnotationSandboxPause] == utils.True
	}
	if box.Spec.Paused == expectPaused {
		log.Info("no need to patch sandbox", "expect", expectPaused, "spec", box.Spec.Paused)
		return nil
	}

	if err := r.Client.Patch(ctx, box, client.RawPatch(types.MergePatchType,
		[]byte(fmt.Sprintf(`{"spec":{"paused":%s}}`, strconv.FormatBool(expectPaused))))); err != nil {
		log.Error(err, "patch pod and sandbox failed")
		return err
	}
	log.Info("patch sandbox spec.paused success")
	if expectPaused {
		r.Recorder.Event(pod, corev1.EventTypeNormal, "SandboxPaused", "patch sandbox spec.paused=true successfully")
	} else {
		r.Recorder.Event(pod, corev1.EventTypeNormal, "SandboxResumed", "patch sandbox spec.paused=false successfully")
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *BypassPodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	controllerName := "bypass-sandbox-pod"
	r.Recorder = mgr.GetEventRecorderFor(controllerName)
	return ctrl.NewControllerManagedBy(mgr).
		Named(controllerName).
		Watches(&corev1.Pod{}, &BypassPodEventHandler{}).
		Complete(r)
}
