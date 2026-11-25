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
	"errors"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/openkruise/agents/api/v1alpha1"
)

func TestSandboxTemplateInlining(t *testing.T) {
	require.NoError(t, corev1.AddToScheme(scheme.Scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme.Scheme))

	podTemplate := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runtime", Image: "test/image"}}}}
	for _, tt := range []struct {
		name     string
		template *v1alpha1.SandboxTemplate
		inline   bool
		ref      bool
		getError error
		denied   string
	}{
		{name: "template reference is inlined at creation", template: &v1alpha1.SandboxTemplate{Spec: v1alpha1.SandboxTemplateSpec{Template: podTemplate}}, ref: true, inline: true},
		{name: "inline template passes through", inline: true},
		{name: "template without pod template is rejected", template: &v1alpha1.SandboxTemplate{}, ref: true, denied: "has no pod template"},
		{name: "missing template is rejected", ref: true, denied: "not found"},
		{name: "missing kind passes through", ref: true, getError: &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: v1alpha1.GroupVersion.Group, Kind: "SandboxTemplate"}}},
		{name: "template read failure rejects creation", ref: true, getError: errors.New("read unavailable"), denied: "resolve sandbox template"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &v1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"}}
			if tt.ref {
				sandbox.Spec.EmbeddedSandboxTemplate.TemplateRef = &v1alpha1.SandboxTemplateRef{Name: "tpl"}
			}
			if tt.inline && tt.template == nil {
				sandbox.Spec.EmbeddedSandboxTemplate.Template = podTemplate.DeepCopy()
			}
			builder := fake.NewClientBuilder().WithScheme(scheme.Scheme)
			if tt.template != nil {
				tt.template.Name, tt.template.Namespace = "tpl", "default"
				builder = builder.WithObjects(tt.template)
			}
			if tt.getError != nil {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
						return tt.getError
					},
				})
			}
			raw, err := json.Marshal(sandbox)
			require.NoError(t, err)
			handler := &Defaulter{Client: builder.Build(), Decoder: admission.NewDecoder(scheme.Scheme)}
			response := handler.Handle(t.Context(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: admissionv1.Create,
				Object:    runtime.RawExtension{Raw: raw},
			}})
			if tt.denied != "" {
				require.False(t, response.Allowed)
				require.Contains(t, response.Result.Message, tt.denied)
				return
			}
			require.True(t, response.Allowed, "%v", response.Result)
			if tt.name == "inline template passes through" || tt.name == "missing kind passes through" {
				require.Empty(t, response.Patches)
				return
			}
			require.NotEmpty(t, response.Patches)
			data, err := json.Marshal(response.Patches)
			require.NoError(t, err)
			patches, err := jsonpatch.DecodePatch(data)
			require.NoError(t, err)
			raw, err = patches.Apply(raw)
			require.NoError(t, err)
			var decoded v1alpha1.Sandbox
			require.NoError(t, json.Unmarshal(raw, &decoded))
			require.Equal(t, "runtime", decoded.Spec.Template.Spec.Containers[0].Name)
			require.Nil(t, decoded.Spec.TemplateRef)
		})
	}
}

func TestSandboxStatusSubresource(t *testing.T) {
	handler := &Defaulter{}
	response := handler.Handle(t.Context(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{SubResource: "status"}})
	require.True(t, response.Allowed)
	require.Empty(t, response.Patches)
}
