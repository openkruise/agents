package validating

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/onsi/gomega"
	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestSandboxSetValidatingHandler_Handle(t *testing.T) {
	// Add v1alpha1 to scheme
	err := v1alpha1.AddToScheme(scheme.Scheme)
	require.NoError(t, err)

	tests := []struct {
		name         string
		sandboxSet   *v1alpha1.SandboxSet
		expectAllow  bool
		expectError  bool
		errorMessage string
	}{
		{
			name: "Valid SandboxSet",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": "test",
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			},
			expectAllow: true,
			expectError: false,
		},
		{
			name: "Invalid name",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "TEST-SBS",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "subdomain must consist of lower case alphanumeric characters, '-' or '.'",
		},
		{
			name: "Negative replicas",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: -1, // Negative replicas are invalid
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "replicas cannot be negative",
		},
		{
			name: "Label with internal prefix",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.InternalPrefix + "test": "value", // Internal prefix labels are invalid
					},
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "label cannot start with " + v1alpha1.InternalPrefix,
		},
		{
			name: "Annotation with internal prefix",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
					Annotations: map[string]string{
						v1alpha1.InternalPrefix + "test": "value", // Internal prefix annotations are invalid
					},
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "annotation cannot start with " + v1alpha1.InternalPrefix,
		},
		{
			name: "Template label with internal prefix",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								v1alpha1.InternalPrefix + "test": "value", // Template internal prefix labels are invalid
							},
						},
					},
				},
			},
			expectAllow:  false,
			expectError:  true,
			errorMessage: "label cannot start with " + v1alpha1.InternalPrefix,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewGomegaWithT(t)

			// Create fake client
			objs := []runtime.Object{}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(objs...).Build()

			// Create decoder
			decoder := admission.NewDecoder(scheme.Scheme)

			// Create handler
			handler := &SandboxSetValidatingHandler{
				Client:  fakeClient,
				Decoder: decoder,
			}

			// Construct admission request
			sbsRaw, err := json.Marshal(tt.sandboxSet)
			require.NoError(t, err)

			req := admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					Operation: admissionv1.Create,
					Object: runtime.RawExtension{
						Raw: sbsRaw,
					},
				},
			}

			response := handler.Handle(context.TODO(), req)

			// Verify results
			if tt.expectAllow {
				t.Log(response.String())
				g.Expect(response.Allowed).To(gomega.BeTrue())
			} else {
				g.Expect(response.Allowed).To(gomega.BeFalse())
			}

			if tt.expectError {
				g.Expect(response.Result).NotTo(gomega.BeNil())
				g.Expect(response.Result.Message).To(gomega.ContainSubstring(tt.errorMessage))
			}
		})
	}
}
