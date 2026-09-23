/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package validating

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/kubernetes/pkg/apis/core"
	corev1conv "k8s.io/kubernetes/pkg/apis/core/v1"
	corevalidation "k8s.io/kubernetes/pkg/apis/core/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	webhookutils "github.com/openkruise/agents/pkg/webhook/utils"
)

type SandboxValidatingHandler struct {
	Client  client.Client
	Decoder admission.Decoder
}

// +kubebuilder:webhook:path=/validate-sandbox,mutating=false,failurePolicy=fail,sideEffects=None,admissionReviewVersions=v1;v1beta1,groups=agents.kruise.io,resources=sandboxes,verbs=create,versions=v1alpha1,name=v-sbx.kb.io

func (h *SandboxValidatingHandler) Path() string {
	return "/validate-sandbox"
}

func (h *SandboxValidatingHandler) Enabled() bool {
	return true
}

// Handle validates the metadata and pod template of a Sandbox on creation.
// Scope selection (user-created vs. internally-created) is performed by the
// ValidatingWebhookConfiguration's objectSelector on the
// agents.kruise.io/managed-by label; this handler validates every Sandbox
// creation it receives.
func (h *SandboxValidatingHandler) Handle(_ context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create {
		return admission.Allowed("")
	}

	sbx := &agentsv1alpha1.Sandbox{}
	if err := h.Decoder.Decode(req, sbx); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	var errList field.ErrorList
	errList = append(errList, validateSandboxMetadata(sbx.ObjectMeta, field.NewPath("metadata"))...)
	errList = append(errList, validateSandboxSpec(sbx.Spec, field.NewPath("spec"))...)

	if len(errList) > 0 {
		return admission.Errored(http.StatusUnprocessableEntity, errList.ToAggregate())
	}
	return admission.Allowed("")
}

func validateSandboxMetadata(metadata metav1.ObjectMeta, fldPath *field.Path) field.ErrorList {
	var errList field.ErrorList
	errList = append(errList, validation.ValidateObjectMeta(&metadata, true, validation.NameIsDNSSubdomain, fldPath)...)
	errList = append(errList, validateLabelsAndAnnotations(metadata, fldPath)...)
	return errList
}

func validateLabelsAndAnnotations(metadata metav1.ObjectMeta, fldPath *field.Path) field.ErrorList {
	var errList field.ErrorList
	labelFld := fldPath.Child("labels")
	for k := range metadata.Labels {
		if strings.HasPrefix(k, agentsv1alpha1.E2BPrefix) {
			errList = append(errList, field.Invalid(labelFld.Key(k), k, "label cannot start with "+agentsv1alpha1.E2BPrefix))
		}
	}
	annoFld := fldPath.Child("annotations")
	for k := range metadata.Annotations {
		if strings.HasPrefix(k, agentsv1alpha1.E2BPrefix) {
			errList = append(errList, field.Invalid(annoFld.Key(k), k, "annotation cannot start with "+agentsv1alpha1.E2BPrefix))
		}
	}
	return errList
}

func validateSandboxSpec(spec agentsv1alpha1.SandboxSpec, fldPath *field.Path) field.ErrorList {
	var errList field.ErrorList
	if spec.Template == nil {
		return errList
	}
	errList = append(errList, validateLabelsAndAnnotations(spec.Template.ObjectMeta, fldPath.Child("template"))...)
	errList = append(errList, validateSandboxPodTemplateSpec(spec, fldPath)...)
	return errList
}

func validateSandboxPodTemplateSpec(spec agentsv1alpha1.SandboxSpec, fldPath *field.Path) field.ErrorList {
	errList := field.ErrorList{}
	template := spec.Template.DeepCopy()
	coreTemplate := &core.PodTemplateSpec{}

	if len(spec.VolumeClaimTemplates) != 0 {
		errList = append(errList, webhookutils.ValidateVolumeClaimTemplateMounts(spec.Template, spec.VolumeClaimTemplates, fldPath)...)
		webhookutils.AppendVolumeClaimTemplateVolumes(template, spec.VolumeClaimTemplates)
	}
	if err := corev1conv.Convert_v1_PodTemplateSpec_To_core_PodTemplateSpec(template, coreTemplate, nil); err != nil {
		errList = append(errList, field.Invalid(fldPath.Child("template"), spec.Template, fmt.Sprintf("Convert_v1_PodTemplateSpec_To_core_PodTemplateSpec failed: %v", err)))
		return errList
	}
	errList = append(errList, corevalidation.ValidatePodTemplateSpec(coreTemplate, fldPath.Child("template"), webhookutils.DefaultPodValidationOptions)...)
	return errList
}
