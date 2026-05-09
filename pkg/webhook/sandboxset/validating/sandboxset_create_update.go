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
	"math"
	"net/http"
	"strings"

	apicorev1 "k8s.io/api/core/v1"
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

	if _, err := intstrutil.GetScaledValueFromIntOrPercent(
		intstrutil.ValueOrDefault(spec.ScaleStrategy.MaxUnavailable, intstrutil.FromInt32(math.MaxInt32)), int(spec.Replicas), true); err != nil {
		errList = append(errList, field.Invalid(fldPath.Child("scaleStrategy.maxUnavailable"), spec.ScaleStrategy.MaxUnavailable, "maxUnavailable is invalid"))
	}

	// Validate UpdateStrategy.MaxUnavailable if specified
	if spec.UpdateStrategy.MaxUnavailable != nil {
		if _, err := intstrutil.GetScaledValueFromIntOrPercent(
			intstrutil.ValueOrDefault(spec.UpdateStrategy.MaxUnavailable, intstrutil.FromInt(0)), int(spec.Replicas), true); err != nil {
			errList = append(errList, field.Invalid(fldPath.Child("updateStrategy.maxUnavailable"), spec.UpdateStrategy.MaxUnavailable, "maxUnavailable is invalid"))
		}
	}

	return errList
}

func validateSandboxSetPodTemplateSpec(spec agentsv1alpha1.SandboxSetSpec, fldPath *field.Path) field.ErrorList {
	errList := field.ErrorList{}
	coreTemplate := &core.PodTemplateSpec{}

	if err := corev1conv.Convert_v1_PodTemplateSpec_To_core_PodTemplateSpec(spec.Template.DeepCopy(), coreTemplate, nil); err != nil {
		errList = append(errList, field.Invalid(fldPath.Child("template"), spec.Template, fmt.Sprintf("Convert_v1_PodTemplateSpec_To_core_PodTemplateSpec failed: %v", err)))
		return errList
	}
	if len(spec.VolumeClaimTemplates) != 0 {
		errList = append(errList, validateVolumeClaimTemplateMounts(spec, fldPath)...)
		for _, template := range spec.VolumeClaimTemplates {
			coreTemplate.Spec.Volumes = append(coreTemplate.Spec.Volumes, core.Volume{
				Name: template.Name,
				VolumeSource: core.VolumeSource{
					PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{
						ClaimName: template.Name,
					},
				},
			})
		}
	}
	errList = append(errList, corevalidation.ValidatePodTemplateSpec(coreTemplate, fldPath.Child("template"), webhookutils.DefaultPodValidationOptions)...)
	return errList
}

func validateVolumeClaimTemplateMounts(spec agentsv1alpha1.SandboxSetSpec, fldPath *field.Path) field.ErrorList {
	errList := field.ErrorList{}
	mountedVolumeNames := map[string]struct{}{}

	recordMounts := func(containers []apicorev1.Container) {
		for i := range containers {
			for j := range containers[i].VolumeMounts {
				mountedVolumeNames[containers[i].VolumeMounts[j].Name] = struct{}{}
			}
		}
	}
	recordMounts(spec.Template.Spec.InitContainers)
	recordMounts(spec.Template.Spec.Containers)
	for i := range spec.Template.Spec.EphemeralContainers {
		for j := range spec.Template.Spec.EphemeralContainers[i].VolumeMounts {
			mountedVolumeNames[spec.Template.Spec.EphemeralContainers[i].VolumeMounts[j].Name] = struct{}{}
		}
	}

	for i, template := range spec.VolumeClaimTemplates {
		if template.Name == "" {
			continue
		}
		if _, mounted := mountedVolumeNames[template.Name]; !mounted {
			errList = append(errList, field.Required(
				fldPath.Child("template").Child("spec").Child("containers").Child("volumeMounts"),
				fmt.Sprintf("volumeClaimTemplates[%d] %q must be mounted by at least one container, init container, or ephemeral container", i, template.Name),
			))
		}
	}
	return errList
}
