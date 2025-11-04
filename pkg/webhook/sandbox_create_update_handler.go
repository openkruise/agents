package webhook

import (
	"context"
	"net/http"

	agentsv1alpha1 "gitlab.alibaba-inc.com/serverlessinfra/agents/api/v1alpha1"
	addmissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// SandboxCreateUpdateHandler handles Rollout
type SandboxCreateUpdateHandler struct {
	// To use the client, you need to do the following:
	// - uncomment it
	// - import sigs.k8s.io/controller-runtime/pkg/client
	// - uncomment the InjectClient method at the bottom of this file.
	Client client.Client

	// Decoder decodes objects
	Decoder admission.Decoder
}

var _ admission.Handler = &SandboxCreateUpdateHandler{}

// Handle handles admission requests.
func (h *SandboxCreateUpdateHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	switch req.Operation {
	case addmissionv1.Create, addmissionv1.Update:
		obj := &agentsv1alpha1.Sandbox{}
		if err := h.Decoder.Decode(req, obj); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		errList := h.validateSandbox(obj)
		if len(errList) != 0 {
			return admission.Errored(http.StatusUnprocessableEntity, errList.ToAggregate())
		}
	}

	return admission.ValidationResponse(true, "")
}

func (h *SandboxCreateUpdateHandler) validateSandbox(box *agentsv1alpha1.Sandbox) field.ErrorList {

	return nil
}
