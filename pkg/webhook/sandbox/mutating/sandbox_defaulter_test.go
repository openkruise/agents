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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/openkruise/agents/api/v1alpha1"
)

func TestSandboxDefaulter_PathAndEnabled(t *testing.T) {
	handler := &SandboxDefaulter{}
	assert.Equal(t, "/default-sandbox", handler.Path())
	assert.True(t, handler.Enabled())
}

func TestSandboxDefaulter_Handle(t *testing.T) {
	err := v1alpha1.AddToScheme(scheme.Scheme)
	require.NoError(t, err)

	handler := &SandboxDefaulter{
		Client:  fake.NewClientBuilder().WithScheme(scheme.Scheme).Build(),
		Decoder: admission.NewDecoder(scheme.Scheme),
	}

	t.Run("malformed raw object is denied with HTTP 400", func(t *testing.T) {
		resp := handler.Handle(context.TODO(), admission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: admissionv1.Create,
				Object:    runtime.RawExtension{Raw: []byte(`{invalid-json}`)},
			},
		})
		require.False(t, resp.Allowed)
		require.NotNil(t, resp.Result)
		assert.Equal(t, int32(400), resp.Result.Code)
	})

	t.Run("unchanged sandbox is allowed without patches", func(t *testing.T) {
		sandbox := &v1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-sbx",
				Namespace: "default",
			},
			Spec: v1alpha1.SandboxSpec{},
		}
		raw, err := json.Marshal(sandbox)
		require.NoError(t, err)

		resp := handler.Handle(context.TODO(), admission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: admissionv1.Create,
				Object:    runtime.RawExtension{Raw: raw},
			},
		})

		require.True(t, resp.Allowed)
		assert.Empty(t, resp.Patches)
	})

	t.Run("sandbox with sparse template is defaulted", func(t *testing.T) {
		sandbox := &v1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-sbx",
				Namespace: "default",
			},
			Spec: v1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "test", Image: "nginx:latest"},
							},
						},
					},
					VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
						{ObjectMeta: metav1.ObjectMeta{Name: "data"}},
					},
				},
			},
		}
		raw, err := json.Marshal(sandbox)
		require.NoError(t, err)

		resp := handler.Handle(context.TODO(), admission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: admissionv1.Create,
				Object:    runtime.RawExtension{Raw: raw},
			},
		})

		require.True(t, resp.Allowed)
		require.NotEmpty(t, resp.Patches)

		patchPaths := map[string]bool{}
		for _, p := range resp.Patches {
			patchPaths[p.Path] = true
		}
		assert.Contains(t, patchPaths, "/spec/template/spec/automountServiceAccountToken")
		assert.Contains(t, patchPaths, "/spec/template/spec/dnsPolicy")
		assert.Contains(t, patchPaths, "/spec/template/spec/restartPolicy")
		assert.Contains(t, patchPaths, "/spec/volumeClaimTemplates/0/spec/accessModes")
		assert.Contains(t, patchPaths, "/spec/volumeClaimTemplates/0/spec/volumeMode")
	})
}
