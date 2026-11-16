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
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	api "github.com/openkruise/agents/api/v1alpha1"
)

func TestManagedAndLegacyTemplateUpdateAdmission(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	handler := ValidatingHandler{Decoder: admission.NewDecoder(scheme)}
	original := &api.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Name: "a7f2k9m4p1q8r3s6t0v5", Namespace: "team-a", OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplateClaim", Name: "python-agent", UID: "claim-uid", Controller: ptr.To(true)}}}, Spec: api.SandboxTemplateSpec{Template: minimalPodTemplate()}}
	tests := []struct {
		name    string
		old     *api.SandboxTemplate
		mutate  func(*api.SandboxTemplate)
		allowed bool
	}{
		{"managed unchanged", original, nil, true},
		{"managed changed spec", original, func(t *api.SandboxTemplate) { t.Spec.Template.Spec.Containers[0].Image = "other" }, false},
		{"managed owner removed", original, func(t *api.SandboxTemplate) { t.OwnerReferences = nil }, false},
		{"managed status only", original, func(t *api.SandboxTemplate) { t.Status.Phase = api.SandboxTemplatePhaseReady }, true},
	}
	legacy := original.DeepCopy()
	legacy.OwnerReferences = nil
	tests = append(tests, struct {
		name    string
		old     *api.SandboxTemplate
		mutate  func(*api.SandboxTemplate)
		allowed bool
	}{"legacy update unchanged contract", legacy, func(t *api.SandboxTemplate) { t.Spec.Template = nil }, true})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			next := test.old.DeepCopy()
			if test.mutate != nil {
				test.mutate(next)
			}
			oldRaw, err := json.Marshal(test.old)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(next)
			if err != nil {
				t.Fatal(err)
			}
			response := handler.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Update, Object: runtime.RawExtension{Raw: raw}, OldObject: runtime.RawExtension{Raw: oldRaw}}})
			if response.Allowed != test.allowed {
				t.Fatalf("allowed=%v want=%v result=%v", response.Allowed, test.allowed, response.Result)
			}
		})
	}
}
