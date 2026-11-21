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

package mutating

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// Defaulter inlines a SandboxTemplate into the Sandbox at creation time.
type Defaulter struct {
	Client  client.Client
	Decoder admission.Decoder
}

// +kubebuilder:webhook:path=/default-sandbox,mutating=true,failurePolicy=fail,sideEffects=None,admissionReviewVersions=v1;v1beta1,groups=agents.kruise.io,resources=sandboxes,verbs=create,versions=v1alpha1,name=md-sbx.kb.io
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxtemplates,verbs=get

func (h *Defaulter) Path() string {
	return "/default-sandbox"
}

func (h *Defaulter) Enabled() bool {
	return true
}

func (h *Defaulter) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.SubResource == "status" {
		return admission.Allowed("")
	}
	obj := &agentsv1alpha1.Sandbox{}
	if err := h.Decoder.Decode(req, obj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if obj.Spec.Template != nil || obj.Spec.TemplateRef == nil {
		return admission.Allowed("")
	}
	// Inline the referenced template at creation, like the pool and delivery
	// paths already do. The sandbox spec then survives template deletion:
	// pause deletes the pod and resume rebuilds it from this spec, so a live
	// templateRef would make a sleeping sandbox unresumable.
	template := &agentsv1alpha1.SandboxTemplate{}
	err := h.Client.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: obj.Spec.TemplateRef.Name}, template)
	if meta.IsNoMatchError(err) {
		return admission.Allowed("")
	}
	if apierrors.IsNotFound(err) {
		return admission.Denied(fmt.Sprintf("sandbox template %s/%s not found", obj.Namespace, obj.Spec.TemplateRef.Name))
	}
	if err != nil {
		return admission.Errored(http.StatusServiceUnavailable, fmt.Errorf("resolve sandbox template: %w", err))
	}
	if template.Spec.Template == nil {
		return admission.Denied(fmt.Sprintf("sandbox template %s/%s has no pod template", obj.Namespace, obj.Spec.TemplateRef.Name))
	}
	obj.Spec.Template = template.Spec.Template.DeepCopy()
	obj.Spec.TemplateRef = nil
	marshal, err := json.Marshal(obj)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshal)
}
