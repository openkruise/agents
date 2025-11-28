package validating

import (
	"context"
	"net/http"
	"strings"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/webhook/utils/extravalidation"
	"k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type SandboxSetValidatingHandler struct {
	Client  client.Client
	Decoder admission.Decoder
}

// +kubebuilder:webhook:path=/validate-sandboxset,mutating=false,failurePolicy=fail,sideEffects=None,admissionReviewVersions=v1;v1beta1,groups=agents.kruise.io,resources=sandboxsets,verbs=create;update,versions=v1alpha1,name=v-sbs.kb.io

func (h *SandboxSetValidatingHandler) Path() string {
	return "/validate-sandboxset"
}

func (h *SandboxSetValidatingHandler) Enabled() bool {
	return true
}

func (h *SandboxSetValidatingHandler) Handle(_ context.Context, req admission.Request) admission.Response {
	obj := &agentsv1alpha1.SandboxSet{}
	err := h.Decoder.Decode(req, obj)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	var errList field.ErrorList
	errList = append(errList, validateSandboxSetMetadata(obj.ObjectMeta, field.NewPath("metadata"))...)
	errList = append(errList, validateSandboxSetSpec(obj.Spec, field.NewPath("spec"))...)
	if len(errList) > 0 {
		return admission.Errored(http.StatusUnprocessableEntity, errList.ToAggregate())
	}
	return admission.Allowed("")
}

func validateSandboxSetMetadata(metadata metav1.ObjectMeta, fldPath *field.Path) field.ErrorList {
	var errList field.ErrorList
	errList = append(errList, validation.ValidateObjectMeta(&metadata, true, validation.NameIsDNSSubdomain, fldPath)...)
	errList = append(errList, validateLabelsAndAnnotations(metadata, fldPath)...)
	return errList
}

func validateLabelsAndAnnotations(metadata metav1.ObjectMeta, fldPath *field.Path) field.ErrorList {
	var errList field.ErrorList
	labelFld := fldPath.Child("labels")
	for k := range metadata.Labels {
		if strings.HasPrefix(k, agentsv1alpha1.InternalPrefix) {
			errList = append(errList, field.Invalid(labelFld.Key(k), k, "label cannot start with "+agentsv1alpha1.InternalPrefix))
		}
	}
	annoFld := fldPath.Child("annotations")
	for k := range metadata.Annotations {
		if strings.HasPrefix(k, agentsv1alpha1.InternalPrefix) {
			errList = append(errList, field.Invalid(annoFld.Key(k), k, "annotation cannot start with "+agentsv1alpha1.InternalPrefix))
		}
	}
	return errList
}

func validateSandboxSetSpec(spec agentsv1alpha1.SandboxSetSpec, fldPath *field.Path) field.ErrorList {
	var errList field.ErrorList
	if spec.Replicas < 0 {
		errList = append(errList, field.Invalid(fldPath.Child("replicas"), spec.Replicas, "replicas cannot be negative"))
	}
	errList = append(errList, validateLabelsAndAnnotations(spec.Template.ObjectMeta, fldPath.Child("template"))...)
	for _, validator := range extravalidation.GetExtraPodTemplateValidators() {
		errList = append(errList, validator(spec.Template, fldPath.Child("template"))...)
	}
	return errList
}
