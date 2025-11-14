package mutating

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/webhook/types"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// PodBypassSandboxHandler 用于实现 ACS Sandbox 旁路语法糖。考虑到未来扩展可能性不大，先避免过度设计，直接在 Handle 方法中实现功能。
type PodBypassSandboxHandler struct {
	Client  client.Client
	Decoder admission.Decoder
}

// +kubebuilder:webhook:path=/pod-bypass-sandbox,mutating=true,failurePolicy=fail,sideEffects=None,admissionReviewVersions=v1;v1beta1,groups="",resources=pods,verbs=create,versions=v1,name=mbypass-sbx.kb.io

func (h *PodBypassSandboxHandler) Path() string {
	return "/pod-bypass-sandbox"
}

func (h *PodBypassSandboxHandler) Enabled() bool {
	return true
}

func (h *PodBypassSandboxHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	switch req.AdmissionRequest.Operation {
	case admissionv1.Create:
		return h.HandleCreate(ctx, req)
	default:
		return admission.Allowed("")
	}
}

func (h *PodBypassSandboxHandler) HandleCreate(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx)
	pod := &corev1.Pod{}
	if err := h.Decoder.DecodeRaw(req.Object, pod); err != nil {
		log.Error(err, "unable to decode pod")
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if pod.Labels[utils.PodLabelEnableAutoCreateSandbox] != utils.True {
		log.Info("pod not labeled bypass-sandbox")
		return admission.Allowed("")
	}
	response, err := h.HandleBypassSandboxCreate(ctx, pod)
	return h.SendResponse(ctx, pod, req, response, err)
}

func (h *PodBypassSandboxHandler) SendResponse(ctx context.Context, pod *corev1.Pod, req admission.Request, response types.Response, err error) admission.Response {
	log := logf.FromContext(ctx).V(utils.DebugLogLevel).WithValues("pod", klog.KObj(pod))
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	switch response.Result {
	case types.Skip:
		return admission.Allowed(response.Message)
	case types.Deny:
		return admission.Denied(response.Message)
	case types.Patch:
		log.Info("will patch mutated pod")
		marshal, err := json.Marshal(pod)
		if err != nil {
			log.Error(err, "marshal mutated pod failed")
			return admission.Errored(http.StatusInternalServerError, err)
		}
		return admission.PatchResponseFromRaw(req.AdmissionRequest.Object.Raw, marshal)
	default:
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("unknown response result: %d", response.Result))
	}
}

// HandleBypassSandboxCreate 处理通过重建 Pod 的方式触发旁路唤醒
func (h *PodBypassSandboxHandler) HandleBypassSandboxCreate(ctx context.Context, pod *corev1.Pod) (result types.Response, err error) {
	log := logf.FromContext(ctx).WithValues("pod", klog.KObj(pod), "webhook", "bypass-sandbox-create-handler")
	box := &agentsv1alpha1.Sandbox{}
	err = h.Client.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: pod.Name}, box)
	if err != nil {
		log.Info("skip processing: cannot get Sandbox", "reason", err)
		return types.Response{
			Message: "skip processing: cannot get Sandbox",
		}, client.IgnoreNotFound(err)
	}
	if !box.Spec.Paused {
		log.Info("existing sandbox spec is not paused")
		return types.Response{
			Message: fmt.Sprintf("existing sandbox %s/%s spec is not paused", box.Namespace, box.Name),
		}, nil
	}
	if box.Status.Phase != agentsv1alpha1.SandboxPaused {
		log.Info("existing sandbox phase is not paused")
		return types.Response{
			Message: fmt.Sprintf("existing sandbox %s/%s phase is not paused", box.Namespace, box.Name),
		}, nil
	}
	pod.Spec = box.Spec.Template.Spec
	utils.InjectResumedPod(box, pod)
	pod.Annotations[utils.PodAnnotationEnablePaused] = "true"
	pod.Annotations[utils.PodAnnotationCreatedBy] = "" // this will be patched by controller
	return types.Response{
		Result:  types.Patch,
		Message: "pod is overwritten by sandbox",
	}, nil
}
