package mutating

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"

	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/defaults"
)

type Defaulter struct {
	Client  client.Client
	Decoder admission.Decoder
}

// +kubebuilder:webhook:path=/default-sandboxtemplate,mutating=true,failurePolicy=fail,sideEffects=None,admissionReviewVersions=v1;v1beta1,groups=agents.kruise.io,resources=sandboxtemplates,verbs=create,versions=v1alpha1,name=md-sbt.kb.io

func (h *Defaulter) Path() string {
	return "/default-sandboxtemplate"
}

func (h *Defaulter) Enabled() bool {
	return true
}

func (h *Defaulter) Handle(_ context.Context, req admission.Request) admission.Response {
	obj := &agentsv1alpha1.SandboxTemplate{}
	err := h.Decoder.Decode(req, obj)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	clone := obj.DeepCopy()
	setDefaultPodTemplate(obj.Spec.Template)

	// Apply defaulting logic to volume claim templates
	setDefaultVolumeClaimTemplates(obj.Spec.VolumeClaimTemplates)

	if !reflect.DeepEqual(obj, clone) {
		marshal, err := json.Marshal(obj)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
		return admission.PatchResponseFromRaw(req.Object.Raw, marshal)
	}
	return admission.Allowed("")
}

func setDefaultPodTemplate(template *v1.PodTemplateSpec) {
	if template == nil {
		return
	}
	if ptr.Deref(template.Spec.AutomountServiceAccountToken, true) {
		template.Spec.AutomountServiceAccountToken = ptr.To(false)
	}
	defaults.SetDefaultPodSpec(&template.Spec)
}

// setDefaultVolumeClaimTemplates applies default values to the volume claim templates
func setDefaultVolumeClaimTemplates(templates []v1.PersistentVolumeClaim) {
	for i := range templates {
		vct := &templates[i]
		// Set default access modes if not specified
		if len(vct.Spec.AccessModes) == 0 {
			vct.Spec.AccessModes = []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}
		}

		// Set default volume mode if not specified
		if vct.Spec.VolumeMode == nil {
			volumeMode := v1.PersistentVolumeFilesystem
			vct.Spec.VolumeMode = &volumeMode
		}
	}
}
