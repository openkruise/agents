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

	"github.com/onsi/gomega"
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

// minimalPodTemplate builds a pod template that passes the unrelated template
// validations, so a case can fail only on what it is testing.
func minimalPodTemplate() *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyAlways,
			DNSPolicy:                     corev1.DNSClusterFirst,
			TerminationGracePeriodSeconds: new(int64),
			Containers: []corev1.Container{
				{
					Name:                     "test",
					Image:                    "nginx:latest",
					ImagePullPolicy:          corev1.PullAlways,
					TerminationMessagePolicy: corev1.TerminationMessageReadFile,
				},
			},
		},
	}
}

// invalidPodTemplate has a container without a name, which the core pod
// template validation rejects with a "Required value" error.
func invalidPodTemplate() *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "", Image: "nginx:latest"},
			},
		},
	}
}

func TestSandboxValidatingHandler_PathAndEnabled(t *testing.T) {
	handler := &SandboxValidatingHandler{}
	assert.Equal(t, "/validate-sandbox", handler.Path())
	assert.True(t, handler.Enabled())
}

func TestSandboxValidatingHandler_Handle(t *testing.T) {
	err := v1alpha1.AddToScheme(scheme.Scheme)
	require.NoError(t, err)

	tests := []struct {
		name         string
		sandbox      *v1alpha1.Sandbox
		operation    admissionv1.Operation
		expectAllow  bool
		expectError  bool
		errorMessage string
	}{
		{
			name: "user-created sandbox with valid pod template allowed",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbx",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: minimalPodTemplate(),
					},
				},
			},
			operation:   admissionv1.Create,
			expectAllow: true,
		},
		{
			name: "sandbox with templateRef and no inline template allowed",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbx",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "my-template"},
					},
				},
			},
			operation:   admissionv1.Create,
			expectAllow: true,
		},
		{
			name: "user-created sandbox with invalid pod template denied",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbx",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: invalidPodTemplate(),
					},
				},
			},
			operation:    admissionv1.Create,
			expectAllow:  false,
			expectError:  true,
			errorMessage: "Required value",
		},
		{
			name: "user-created sandbox with unmounted volume claim template denied",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbx",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: minimalPodTemplate(),
						VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
							{
								ObjectMeta: metav1.ObjectMeta{Name: "data-vol"},
								Spec: corev1.PersistentVolumeClaimSpec{
									AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
								},
							},
						},
					},
				},
			},
			operation:    admissionv1.Create,
			expectAllow:  false,
			expectError:  true,
			errorMessage: "must be mounted by at least one container",
		},
		{
			// Scope selection is done by the ValidatingWebhookConfiguration's
			// objectSelector, not by this handler: a Sandbox carrying an
			// internal managed-by label is still validated when it reaches
			// the handler.
			name: "sandbox with managed-by label is still validated by handler",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbx",
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.LabelManagedBy: v1alpha1.ManagedBySandboxSetController,
					},
				},
				Spec: v1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: invalidPodTemplate(),
					},
				},
			},
			operation:    admissionv1.Create,
			expectAllow:  false,
			expectError:  true,
			errorMessage: "Required value",
		},
		{
			name: "user-created sandbox with e2b-prefixed metadata label denied",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbx",
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.E2BPrefix + "test": "value",
					},
				},
				Spec: v1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: minimalPodTemplate(),
					},
				},
			},
			operation:    admissionv1.Create,
			expectAllow:  false,
			expectError:  true,
			errorMessage: "label cannot start with " + v1alpha1.E2BPrefix,
		},
		{
			name: "user-created sandbox with e2b-prefixed template label denied",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbx",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									v1alpha1.E2BPrefix + "test": "value",
								},
							},
							Spec: minimalPodTemplate().Spec,
						},
					},
				},
			},
			operation:    admissionv1.Create,
			expectAllow:  false,
			expectError:  true,
			errorMessage: "label cannot start with " + v1alpha1.E2BPrefix,
		},
		{
			name: "user-created sandbox update with invalid pod template allowed",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbx",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: invalidPodTemplate(),
					},
				},
			},
			operation:   admissionv1.Update,
			expectAllow: true,
		},
		{
			name: "user-created sandbox with e2b-prefixed metadata annotation denied",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbx",
					Namespace: "default",
					Annotations: map[string]string{
						v1alpha1.E2BPrefix + "test": "value",
					},
				},
				Spec: v1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: minimalPodTemplate(),
					},
				},
			},
			operation:    admissionv1.Create,
			expectAllow:  false,
			expectError:  true,
			errorMessage: "annotation cannot start with " + v1alpha1.E2BPrefix,
		},
		{
			name: "user-created sandbox with e2b-prefixed template annotation denied",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbx",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Annotations: map[string]string{
									v1alpha1.E2BPrefix + "test": "value",
								},
							},
							Spec: minimalPodTemplate().Spec,
						},
					},
				},
			},
			operation:    admissionv1.Create,
			expectAllow:  false,
			expectError:  true,
			errorMessage: "annotation cannot start with " + v1alpha1.E2BPrefix,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewGomegaWithT(t)

			fakeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
			handler := &SandboxValidatingHandler{
				Client:  fakeClient,
				Decoder: admission.NewDecoder(scheme.Scheme),
			}

			raw, err := json.Marshal(tt.sandbox)
			require.NoError(t, err)

			req := admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					Operation: tt.operation,
					Object:    runtime.RawExtension{Raw: raw},
				},
			}

			resp := handler.Handle(context.TODO(), req)

			if tt.expectAllow {
				g.Expect(resp.Allowed).To(gomega.BeTrue())
			} else {
				g.Expect(resp.Allowed).To(gomega.BeFalse())
			}
			if tt.expectError {
				g.Expect(resp.Result).NotTo(gomega.BeNil())
				g.Expect(resp.Result.Message).To(gomega.ContainSubstring(tt.errorMessage))
			}
		})
	}
}

func TestSandboxValidatingHandler_Handle_MalformedObject(t *testing.T) {
	err := v1alpha1.AddToScheme(scheme.Scheme)
	require.NoError(t, err)

	handler := &SandboxValidatingHandler{
		Client:  fake.NewClientBuilder().WithScheme(scheme.Scheme).Build(),
		Decoder: admission.NewDecoder(scheme.Scheme),
	}

	resp := handler.Handle(context.TODO(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Object:    runtime.RawExtension{Raw: []byte(`{invalid-json}`)},
		},
	})

	require.False(t, resp.Allowed)
	require.NotNil(t, resp.Result)
	assert.Equal(t, int32(400), resp.Result.Code)
}
