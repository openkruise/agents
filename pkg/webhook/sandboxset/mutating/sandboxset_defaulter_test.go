package mutating

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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestSandboxSetDefaulter_Handle(t *testing.T) {
	err := v1alpha1.AddToScheme(scheme.Scheme)
	require.NoError(t, err)

	tests := []struct {
		name        string
		sandboxSet  *v1alpha1.SandboxSet
		expectAllow bool
		expectPatch bool
	}{
		{
			name: "AutomountServiceAccountToken is nil, should be set to false",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-container",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			},
			expectAllow: true,
			expectPatch: true,
		},
		{
			name: "AutomountServiceAccountToken is true, should be set to false",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							AutomountServiceAccountToken: ptr.To(true),
							Containers: []corev1.Container{
								{
									Name:  "test-container",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			},
			expectAllow: true,
			expectPatch: true,
		},
		{
			name: "AutomountServiceAccountToken is false, should remain false",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							AutomountServiceAccountToken: ptr.To(false),
							Containers: []corev1.Container{
								{
									Name:  "test-container",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			},
			expectAllow: true,
			expectPatch: false,
		},
		{
			name: "No containers, AutomountServiceAccountToken is nil, should be set to false",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{},
					},
				},
			},
			expectAllow: true,
			expectPatch: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewGomegaWithT(t)

			// 创建 fake client
			var objs []runtime.Object
			fakeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(objs...).Build()

			decoder := admission.NewDecoder(scheme.Scheme)

			defaulter := &SandboxSetDefaulter{
				Client:  fakeClient,
				Decoder: decoder,
			}

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

			response := defaulter.Handle(context.TODO(), req)

			g.Expect(response.Allowed).To(gomega.BeTrue())

			if tt.expectPatch {
				g.Expect(response.Patches).NotTo(gomega.BeEmpty())
			} else {
				g.Expect(response.Patches).To(gomega.BeEmpty())
			}
		})
	}
}

func TestSandboxSetDefaulter_HandleUpdate(t *testing.T) {
	err := v1alpha1.AddToScheme(scheme.Scheme)
	require.NoError(t, err)

	tests := []struct {
		name        string
		sandboxSet  *v1alpha1.SandboxSet
		expectAllow bool
		expectPatch bool
	}{
		{
			name: "Update with nil AutomountServiceAccountToken, should be set to false",
			sandboxSet: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 5, // Changed replicas
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-container",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			},
			expectAllow: true,
			expectPatch: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewGomegaWithT(t)

			var objs []runtime.Object
			fakeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(objs...).Build()

			decoder := admission.NewDecoder(scheme.Scheme)

			defaulter := &SandboxSetDefaulter{
				Client:  fakeClient,
				Decoder: decoder,
			}

			sbsRaw, err := json.Marshal(tt.sandboxSet)
			require.NoError(t, err)

			req := admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					Operation: admissionv1.Update,
					Object: runtime.RawExtension{
						Raw: sbsRaw,
					},
				},
			}

			response := defaulter.Handle(context.TODO(), req)

			g.Expect(response.Allowed).To(gomega.BeTrue())

			if tt.expectPatch {
				g.Expect(response.Patches).NotTo(gomega.BeEmpty())
			} else {
				g.Expect(response.Patches).To(gomega.BeEmpty())
			}
		})
	}
}
