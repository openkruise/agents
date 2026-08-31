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
	"regexp"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/kubernetes/pkg/apis/core"
	corev1conv "k8s.io/kubernetes/pkg/apis/core/v1"
	corevalidation "k8s.io/kubernetes/pkg/apis/core/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/autopause"
	webhookutils "github.com/openkruise/agents/pkg/webhook/utils"
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

func validateSandboxSetSpec(spec agentsv1alpha1.SandboxSetSpec, fldPath *field.Path) field.ErrorList {
	var errList field.ErrorList
	if spec.Replicas < 0 {
		errList = append(errList, field.Invalid(fldPath.Child("replicas"), spec.Replicas, "replicas cannot be negative"))
	}

	if spec.TemplateRef != nil && spec.EmbeddedSandboxTemplate.Template != nil {
		errList = append(errList, field.Invalid(fldPath.Child("templateRef"), spec.TemplateRef, "templateRef and podtemplate is mutual exclusive"))
	}

	if spec.EmbeddedSandboxTemplate.Template != nil {
		errList = append(errList, validateLabelsAndAnnotations(spec.Template.ObjectMeta, fldPath.Child("template"))...)
		errList = append(errList, validateSandboxSetPodTemplateSpec(spec, fldPath)...)
	}

	errList = append(errList,
		validateMaxUnavailable(spec.ScaleStrategy.MaxUnavailable, fldPath.Child("scaleStrategy.maxUnavailable"))...)
	errList = append(errList,
		validateMaxUnavailable(spec.UpdateStrategy.MaxUnavailable, fldPath.Child("updateStrategy.maxUnavailable"))...)

	errList = append(errList, autopause.ValidateProbes(spec.Probes, fldPath.Child("probes"))...)
	errList = append(errList, autopause.ValidateAutoPausePolicy(spec.AutoPausePolicy, spec.Probes, fldPath.Child("autoPausePolicy"))...)

	return errList
}

// maxUnavailablePercentPattern matches percentage strings such as "70%". A
// leading sign, decimals, or extra whitespace are rejected so both the
// controller and defaulter can rely on a normalized form.
var maxUnavailablePercentPattern = regexp.MustCompile(`^([0-9]+)%$`)

// validateMaxUnavailable enforces that maxUnavailable is either a non-negative
// integer or a percentage string in the closed range [0%, 100%]. With this in
// place the controller can call intstr helpers without an error branch, so
// runtime spec validation and event emission can be removed.
func validateMaxUnavailable(v *intstrutil.IntOrString, fldPath *field.Path) field.ErrorList {
	if v == nil {
		return nil
	}
	var errList field.ErrorList
	switch v.Type {
	case intstrutil.Int:
		if v.IntVal < 0 {
			errList = append(errList, field.Invalid(fldPath, v.IntVal, "must be >= 0"))
		}
	case intstrutil.String:
		matches := maxUnavailablePercentPattern.FindStringSubmatch(v.StrVal)
		if matches == nil {
			errList = append(errList, field.Invalid(fldPath, v.StrVal,
				`must be a percentage in the form "<number>%" (e.g. "20%")`))
			return errList
		}
		percent, err := strconv.Atoi(matches[1])
		if err != nil || percent > 100 {
			errList = append(errList, field.Invalid(fldPath, v.StrVal, "must be within [0%, 100%]"))
		}
	default:
		errList = append(errList, field.Invalid(fldPath, v, "unsupported IntOrString type"))
	}
	return errList
}

func validateSandboxSetPodTemplateSpec(spec agentsv1alpha1.SandboxSetSpec, fldPath *field.Path) field.ErrorList {
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
