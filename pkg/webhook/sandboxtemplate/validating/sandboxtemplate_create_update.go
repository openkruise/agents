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
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/templateindex"
	"github.com/openkruise/agents/pkg/templatevalidation"
)

type ValidatingHandler struct {
	Client  client.Client
	Decoder admission.Decoder
}

// +kubebuilder:webhook:path=/validate-sandboxtemplate,mutating=false,failurePolicy=ignore,sideEffects=None,admissionReviewVersions=v1;v1beta1,groups=agents.kruise.io,resources=sandboxtemplates,verbs=create;update,versions=v1alpha1,name=v-sbt.kb.io

func (h *ValidatingHandler) Path() string {
	return "/validate-sandboxtemplate"
}

func (h *ValidatingHandler) Enabled() bool {
	return true
}

func (h *ValidatingHandler) Handle(_ context.Context, req admission.Request) admission.Response {
	obj := &agentsv1alpha1.SandboxTemplate{}
	err := h.Decoder.Decode(req, obj)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	var errList field.ErrorList
	if req.Operation == admissionv1.Update {
		oldObject := &agentsv1alpha1.SandboxTemplate{}
		if err := h.Decoder.DecodeRaw(req.OldObject, oldObject); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		errList = templatevalidation.ValidateManagedUpdate(oldObject, obj, field.NewPath("spec"))
	} else {
		errList = append(errList, validateSandboxTemplateMetadata(obj.ObjectMeta, field.NewPath("metadata"))...)
		if templateindex.ClaimOwnerReference(obj) != nil {
			errList = append(errList, templatevalidation.ValidateID(obj.Name, field.NewPath("metadata", "name"))...)
			errList = append(errList, templatevalidation.ValidateCustomSpec(obj.Spec, field.NewPath("spec"))...)
		} else {
			errList = append(errList, templatevalidation.ValidateSpec(obj.Spec, field.NewPath("spec"))...)
		}
	}
	if len(errList) > 0 {
		return admission.Errored(http.StatusUnprocessableEntity, errList.ToAggregate())
	}
	return admission.Allowed("")
}

func validateSandboxTemplateMetadata(metadata metav1.ObjectMeta, fldPath *field.Path) field.ErrorList {
	var errList field.ErrorList
	errList = append(errList, validation.ValidateObjectMeta(&metadata, true, validation.NameIsDNSSubdomain, fldPath)...)
	return errList
}
