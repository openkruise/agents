package mutating

import (
	"context"
	"encoding/json"
	"net/http"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type SandboxSetDefaulter struct {
	Client  client.Client
	Decoder admission.Decoder
}

// +kubebuilder:webhook:path=/default-sandboxset,mutating=true,failurePolicy=fail,sideEffects=None,admissionReviewVersions=v1;v1beta1,groups=agents.kruise.io,resources=sandboxsets,verbs=create;update,versions=v1alpha1,name=md-sbs.kb.io

func (h *SandboxSetDefaulter) Path() string {
	return "/default-sandboxset"
}

func (h *SandboxSetDefaulter) Enabled() bool {
	return true
}

func (h *SandboxSetDefaulter) Handle(_ context.Context, req admission.Request) admission.Response {
	obj := &agentsv1alpha1.SandboxSet{}
	err := h.Decoder.Decode(req, obj)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	modified := false
	if ptr.Deref(obj.Spec.Template.Spec.AutomountServiceAccountToken, true) {
		obj.Spec.Template.Spec.AutomountServiceAccountToken = ptr.To(false)
		modified = true
	}
	if modified {
		marshal, err := json.Marshal(obj)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
		return admission.PatchResponseFromRaw(req.AdmissionRequest.Object.Raw, marshal)
	}
	return admission.Allowed("")
}
