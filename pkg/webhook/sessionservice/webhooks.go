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

package sessionservice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"

	admissionv1 "k8s.io/api/admission/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/kubernetes/pkg/apis/core"
	corev1 "k8s.io/kubernetes/pkg/apis/core/v1"
	corevalidation "k8s.io/kubernetes/pkg/apis/core/validation"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agents "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/discovery"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/defaults"
	"github.com/openkruise/agents/pkg/webhook/types"
	webhookutils "github.com/openkruise/agents/pkg/webhook/utils"
)

func GetHandlerGetters() []types.HandlerGetter {
	return []types.HandlerGetter{
		func(mgr manager.Manager) types.Handler {
			return &Defaulter{Decoder: admission.NewDecoder(mgr.GetScheme())}
		},
		func(mgr manager.Manager) types.Handler {
			return &ValidatingHandler{Decoder: admission.NewDecoder(mgr.GetScheme())}
		},
	}
}

type Defaulter struct{ Decoder admission.Decoder }

// +kubebuilder:webhook:path=/default-sessionservice,mutating=true,failurePolicy=fail,sideEffects=None,admissionReviewVersions=v1;v1beta1,groups=agents.kruise.io,resources=sessionservices,verbs=create;update,versions=v1alpha1,name=md-ssvc.kb.io

func (*Defaulter) Path() string { return "/default-sessionservice" }
func (*Defaulter) Enabled() bool {
	return discovery.DiscoverGVK(agents.SchemeGroupVersion.WithKind("SessionService"))
}
func (h *Defaulter) Handle(_ context.Context, req admission.Request) admission.Response {
	obj := &agents.SessionService{}
	if err := h.Decoder.Decode(req, obj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	original := obj.DeepCopy()
	if obj.Spec.Template != nil {
		defaults.SetDefaultPodSpec(&obj.Spec.Template.Spec)
	}
	for i := range obj.Spec.VolumeClaimTemplates {
		claim := &obj.Spec.VolumeClaimTemplates[i]
		if len(claim.Spec.AccessModes) == 0 {
			claim.Spec.AccessModes = []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}
		}
		if claim.Spec.VolumeMode == nil {
			mode := v1.PersistentVolumeFilesystem
			claim.Spec.VolumeMode = &mode
		}
	}
	if reflect.DeepEqual(original, obj) {
		return admission.Allowed("")
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, data)
}

type ValidatingHandler struct{ Decoder admission.Decoder }

// +kubebuilder:webhook:path=/validate-sessionservice,mutating=false,failurePolicy=fail,sideEffects=None,admissionReviewVersions=v1;v1beta1,groups=agents.kruise.io,resources=sessionservices,verbs=create;update,versions=v1alpha1,name=v-ssvc.kb.io

func (*ValidatingHandler) Path() string { return "/validate-sessionservice" }
func (*ValidatingHandler) Enabled() bool {
	return discovery.DiscoverGVK(agents.SchemeGroupVersion.WithKind("SessionService"))
}
func (h *ValidatingHandler) Handle(_ context.Context, req admission.Request) admission.Response {
	obj := &agents.SessionService{}
	if err := h.Decoder.Decode(req, obj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	errs := validateTemplate(&obj.Spec.SandboxTemplateSpec)
	// Reserve five decimal port digits and the separator in the first DNS label.
	if len(obj.Name) > 57 {
		errs = append(errs, field.TooLong(field.NewPath("metadata", "name"), obj.Name, 57))
	}
	for _, message := range validation.IsDNS1123Label(obj.Name) {
		errs = append(errs, field.Invalid(field.NewPath("metadata", "name"), obj.Name, message))
	}
	if _, err := utils.SessionServiceMaxSurge(obj); err != nil {
		errs = append(errs, field.Invalid(field.NewPath("spec", "updateStrategy", "maxSurge"), obj.Spec.UpdateStrategy.MaxSurge, err.Error()))
	}
	if req.Operation == admissionv1.Update {
		old := &agents.SessionService{}
		if err := h.Decoder.DecodeRaw(req.OldObject, old); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if old.Spec.Backend != obj.Spec.Backend {
			errs = append(errs, field.Forbidden(field.NewPath("spec", "backend"), "field is immutable"))
		}
	}
	if obj.Spec.Template != nil {
		template := obj.Spec.Template.DeepCopy()
		webhookutils.AppendVolumeClaimTemplateVolumes(template, obj.Spec.VolumeClaimTemplates)
		internal := &core.PodTemplateSpec{}
		if err := corev1.Convert_v1_PodTemplateSpec_To_core_PodTemplateSpec(template, internal, nil); err != nil {
			errs = append(errs, field.Invalid(field.NewPath("spec", "template"), obj.Spec.Template, fmt.Sprint(err)))
		} else {
			errs = append(errs, corevalidation.ValidatePodTemplateSpec(internal, field.NewPath("spec", "template"), webhookutils.DefaultPodValidationOptions)...)
		}
	}
	if len(errs) > 0 {
		return admission.Errored(http.StatusUnprocessableEntity, errs.ToAggregate())
	}
	return admission.Allowed("")
}
