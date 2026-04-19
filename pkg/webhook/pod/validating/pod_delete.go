package validating

import (
	"context"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

const subResourceEviction = "eviction"

type PodValidatingHandler struct {
	Client  client.Client
	Decoder admission.Decoder
}

// +kubebuilder:webhook:path=/validate-pod-delete,mutating=false,failurePolicy=ignore,sideEffects=None,admissionReviewVersions=v1;v1beta1,groups=core,resources=pods,verbs=delete,versions=v1,name=v-pod-delete.kb.io
// +kubebuilder:webhook:path=/validate-pod-delete,mutating=false,failurePolicy=ignore,sideEffects=None,admissionReviewVersions=v1;v1beta1,groups=core,resources=pods/eviction,verbs=create,versions=v1,name=v-pod-eviction.kb.io

func (h *PodValidatingHandler) Path() string {
	return "/validate-pod-delete"
}

func (h *PodValidatingHandler) Enabled() bool {
	return true
}

func (h *PodValidatingHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	// Allow sandbox controller to delete pods without restriction
	if req.UserInfo.Username == utils.GetSandboxControllerUsername() {
		return admission.Allowed("")
	}

	var pod *corev1.Pod
	var err error

	switch req.Operation {
	case admissionv1.Delete:
		pod = &corev1.Pod{}
		if err = h.Decoder.DecodeRaw(req.OldObject, pod); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}

	case admissionv1.Create:
		// Only handle eviction subresource
		if req.SubResource != subResourceEviction {
			return admission.Allowed("")
		}
		// Decode eviction request
		eviction := &policyv1.Eviction{}
		if err = h.Decoder.Decode(req, eviction); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		// Get the pod being evicted
		pod = &corev1.Pod{}
		podKey := types.NamespacedName{
			Namespace: req.Namespace,
			Name:      req.Name,
		}
		if err = h.Client.Get(ctx, podKey, pod); err != nil {
			// If pod not found, allow eviction
			return admission.Allowed("")
		}
	default:
		// Allow other operations
		return admission.Allowed("")
	}
	// If pod is already being deleted, allow
	if !pod.DeletionTimestamp.IsZero() {
		return admission.Allowed("")
	}
	// Check if this pod was created by sandbox controller
	if pod.Labels[utils.PodLabelCreatedBy] != utils.CreatedBySandbox {
		// Not created by sandbox, allow deletion/eviction
		return admission.Allowed("")
	}
	// Find the owner reference to sandbox
	var sandboxOwner *types.UID
	for _, ownerRef := range pod.OwnerReferences {
		if ownerRef.Kind == "Sandbox" && ownerRef.APIVersion == agentsv1alpha1.SchemeGroupVersion.String() {
			sandboxOwner = &ownerRef.UID
			break
		}
	}
	if sandboxOwner == nil {
		// No sandbox owner reference found, allow deletion/eviction
		return admission.Allowed("")
	}
	// Check if the sandbox exists and is not being deleted
	sandbox := &agentsv1alpha1.Sandbox{}
	sandboxKey := types.NamespacedName{
		Namespace: pod.Namespace,
		Name:      pod.Name,
	}
	if err = h.Client.Get(ctx, sandboxKey, sandbox); err != nil {
		// Sandbox not found, allow deletion/eviction
		return admission.Allowed("")
	}
	if sandbox.DeletionTimestamp.IsZero() {
		// Sandbox exists and is not being deleted, deny pod deletion/eviction
		return admission.Denied(fmt.Sprintf(
			"cannot delete/evict pod %s/%s: corresponding sandbox exists. "+
				"Please delete the sandbox instead",
			pod.Namespace, pod.Name,
		))
	}
	return admission.Allowed("")
}
