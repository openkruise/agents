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
	"net/http"
	"reflect"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	webhookutils "github.com/openkruise/agents/pkg/webhook/utils"
)

type SandboxDefaulter struct {
	Client  client.Client
	Decoder admission.Decoder
}

// +kubebuilder:webhook:path=/default-sandbox,mutating=true,failurePolicy=fail,sideEffects=None,admissionReviewVersions=v1;v1beta1,groups=agents.kruise.io,resources=sandboxes,verbs=create,versions=v1alpha1,name=md-sbx.kb.io

func (h *SandboxDefaulter) Path() string {
	return "/default-sandbox"
}

func (h *SandboxDefaulter) Enabled() bool {
	return true
}

func (h *SandboxDefaulter) Handle(_ context.Context, req admission.Request) admission.Response {
	obj := &agentsv1alpha1.Sandbox{}
	if err := h.Decoder.Decode(req, obj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	clone := obj.DeepCopy()
	webhookutils.SetDefaultPodTemplate(obj.Spec.Template)
	webhookutils.SetDefaultVolumeClaimTemplates(obj.Spec.VolumeClaimTemplates)

	if !reflect.DeepEqual(obj, clone) {
		marshal, err := json.Marshal(obj)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
		return admission.PatchResponseFromRaw(req.Object.Raw, marshal)
	}
	return admission.Allowed("")
}
