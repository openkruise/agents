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

	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/poolautoscalervalidation"
)

// PoolAutoscalerValidatingHandler handles admission validation for PoolAutoscaler.
type PoolAutoscalerValidatingHandler struct {
	Client  client.Client
	Decoder admission.Decoder
}

// +kubebuilder:webhook:path=/validate-poolautoscaler,mutating=false,failurePolicy=fail,sideEffects=None,admissionReviewVersions=v1;v1beta1,groups=agents.kruise.io,resources=poolautoscalers,verbs=create;update,versions=v1alpha1,name=v-pa.kb.io

func (h *PoolAutoscalerValidatingHandler) Path() string {
	return "/validate-poolautoscaler"
}

func (h *PoolAutoscalerValidatingHandler) Enabled() bool {
	return true
}

func (h *PoolAutoscalerValidatingHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	obj := &agentsv1alpha1.PoolAutoscaler{}
	if err := h.Decoder.Decode(req, obj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	var errList field.ErrorList
	errList = append(errList, validatePoolAutoscalerSpec(obj.Spec, field.NewPath("spec"))...)
	errList = append(errList, h.validateOneToOne(ctx, obj)...)
	if len(errList) > 0 {
		return admission.Errored(http.StatusUnprocessableEntity, errList.ToAggregate())
	}
	return admission.Allowed("")
}

func validatePoolAutoscalerSpec(spec agentsv1alpha1.PoolAutoscalerSpec, fldPath *field.Path) field.ErrorList {
	return poolautoscalervalidation.ValidateSpec(spec, fldPath)
}

// validateOneToOne ensures at most one PoolAutoscaler targets a given SandboxSet.
func (h *PoolAutoscalerValidatingHandler) validateOneToOne(ctx context.Context, obj *agentsv1alpha1.PoolAutoscaler) field.ErrorList {
	var errList field.ErrorList
	list := &agentsv1alpha1.PoolAutoscalerList{}
	// List all PoolAutoscalers in the namespace and filter manually. The webhook
	// must not rely on the field index registered by the controller: when the
	// PoolAutoscaler feature gate is disabled the index does not exist.
	if err := h.Client.List(ctx, list, client.InNamespace(obj.Namespace)); err != nil {
		errList = append(errList, field.InternalError(field.NewPath("spec").Child("scaleTargetRef"),
			fmt.Errorf("failed to list PoolAutoscalers: %w", err)))
		return errList
	}

	for i := range list.Items {
		existing := &list.Items[i]
		if existing.Spec.ScaleTargetRef.Name != obj.Spec.ScaleTargetRef.Name {
			continue
		}
		// Skip self on update
		if existing.Name == obj.Name && existing.Namespace == obj.Namespace {
			continue
		}
		if existing.Spec.ScaleTargetRef.Kind == obj.Spec.ScaleTargetRef.Kind {
			errList = append(errList, field.Invalid(field.NewPath("spec").Child("scaleTargetRef").Child("name"),
				obj.Spec.ScaleTargetRef.Name,
				fmt.Sprintf("SandboxSet %q is already managed by PoolAutoscaler %q", obj.Spec.ScaleTargetRef.Name, existing.Name)))
		}
	}
	return errList
}
