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
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	api "github.com/openkruise/agents/api/v1alpha1"
)

type ClaimValidatingHandler struct {
	Client  client.Client
	Decoder admission.Decoder
}

// +kubebuilder:webhook:path=/validate-sandboxtemplateclaim,mutating=false,failurePolicy=fail,sideEffects=None,admissionReviewVersions=v1;v1beta1,groups=agents.kruise.io,resources=sandboxtemplateclaims,verbs=create;update;delete,versions=v1alpha1,name=v-sbtc.kb.io

func (h *ClaimValidatingHandler) Path() string  { return "/validate-sandboxtemplateclaim" }
func (h *ClaimValidatingHandler) Enabled() bool { return true }

func (h *ClaimValidatingHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation == admissionv1.Delete {
		options := metav1.DeleteOptions{}
		if len(req.Options.Raw) > 0 {
			if err := json.Unmarshal(req.Options.Raw, &options); err != nil {
				return admission.Errored(http.StatusBadRequest, err)
			}
		}
		if options.PropagationPolicy != nil && *options.PropagationPolicy != metav1.DeletePropagationBackground {
			return admission.Errored(http.StatusBadRequest, fmt.Errorf("Claim cleanup requires background propagation to preserve ordered deletion"))
		}
		return admission.Allowed("")
	}
	obj := &api.SandboxTemplateClaim{}
	if err := h.Decoder.Decode(req, obj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	errs := validation.ValidateObjectMeta(&obj.ObjectMeta, true, validation.NameIsDNSLabel, field.NewPath("metadata"))
	if obj.Name == "tags" {
		errs = append(errs, field.Invalid(field.NewPath("metadata", "name"), obj.Name, "name is reserved"))
	}
	if len(errs) > 0 {
		return admission.Errored(http.StatusUnprocessableEntity, errs.ToAggregate())
	}
	if req.Operation == admissionv1.Update {
		return admission.Allowed("")
	}
	for _, resource := range []struct {
		kind string
		obj  client.Object
	}{{"SandboxSet", &api.SandboxSet{}}, {"Checkpoint", &api.Checkpoint{}}} {
		err := h.Client.Get(ctx, client.ObjectKeyFromObject(obj), resource.obj)
		if err == nil {
			return admission.Errored(http.StatusConflict, fmt.Errorf("template name %q is occupied by %s", obj.Name, resource.kind))
		}
		if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			return admission.Errored(http.StatusServiceUnavailable, fmt.Errorf("check template name: %w", err))
		}
	}
	return admission.Allowed("")
}
