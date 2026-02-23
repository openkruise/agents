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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestDefaulter_Handle(t *testing.T) {
	err := v1alpha1.AddToScheme(scheme.Scheme)
	require.NoError(t, err)

	tests := []struct {
		name        string
		sandboxTemplate  *v1alpha1.SandboxTemplate
		expectAllow bool
		expectPatch bool
	}{
		{
			name: "AutomountServiceAccountToken is nil, should be set to false",
			sandboxTemplate: &v1alpha1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxTemplateSpec{
					Template: &corev1.PodTemplateSpec{
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
			sandboxTemplate: &v1alpha1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxTemplateSpec{
					Template: &corev1.PodTemplateSpec{
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
			name: "No containers, AutomountServiceAccountToken is nil, should be set to false",
			sandboxTemplate: &v1alpha1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxTemplateSpec{
					Template: &corev1.PodTemplateSpec{
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

			// Create fake client
			var objs []runtime.Object
			fakeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(objs...).Build()

			decoder := admission.NewDecoder(scheme.Scheme)

			defaulter := &Defaulter{
				Client:  fakeClient,
				Decoder: decoder,
			}

			sbsRaw, err := json.Marshal(tt.sandboxTemplate)
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

func TestSandboxTemplateDefaulter_HandleUpdate(t *testing.T) {
	err := v1alpha1.AddToScheme(scheme.Scheme)
	require.NoError(t, err)

	tests := []struct {
		name        string
		sandboxTemplate  *v1alpha1.SandboxTemplate
		expectAllow bool
		expectPatch bool
	}{
		{
			name: "Update with nil AutomountServiceAccountToken, should be set to false",
			sandboxTemplate: &v1alpha1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxTemplateSpec{
					Template: &corev1.PodTemplateSpec{
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

			defaulter := &Defaulter{
				Client:  fakeClient,
				Decoder: decoder,
			}

			sbsRaw, err := json.Marshal(tt.sandboxTemplate)
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

func TestSetDefaultPodTemplate(t *testing.T) {
	tests := []struct {
		name     string
		template *corev1.PodTemplateSpec
		expected *corev1.PodTemplateSpec
	}{
		{
			name:     "nil template",
			template: nil,
			expected: nil,
		},
		{
			name: "automount service account token is true",
			template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr.To(true),
				},
			},
			expected: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr.To(false), // should be set to false
				},
			},
		},
		{
			name: "automount service account token is false",
			template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr.To(false),
				},
			},
			expected: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr.To(false), // should remain false
				},
			},
		},
		{
			name: "automount service account token is nil (defaults to true)",
			template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: nil,
				},
			},
			expected: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr.To(false), // should be set to false
				},
			},
		},
		{
			name: "pod with containers and volumes",
			template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr.To(true),
					Volumes: []corev1.Volume{
						{
							Name: "test-volume",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "nginx:latest",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("100m"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
							},
						},
					},
					InitContainers: []corev1.Container{
						{
							Name:  "init-container",
							Image: "busybox:latest",
						},
					},
				},
			},
			expected: &corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{}, // corev1.SetDefaults_PodSpec may set default values
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr.To(false), // should be set to false
					// Other defaults will be set by corev1.SetDefaults_PodSpec
					// Volumes will have defaults applied by corev1.SetDefaults_Volume
					// Containers will have defaults applied by corev1.SetDefaults_Container and resource defaults
					// InitContainers will have defaults applied by corev1.SetDefaults_Container and resource defaults
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := deepCopyPodTemplateSpec(tt.template)
			setDefaultPodTemplate(tt.template)

			// Check if automount service account token is properly defaulted
			if tt.template != nil && tt.expected != nil {
				if ptr.Deref(tt.template.Spec.AutomountServiceAccountToken, true) !=
					ptr.Deref(tt.expected.Spec.AutomountServiceAccountToken, true) {
					t.Errorf("Expected AutomountServiceAccountToken to be %v, got %v",
						ptr.Deref(tt.expected.Spec.AutomountServiceAccountToken, true),
						ptr.Deref(tt.template.Spec.AutomountServiceAccountToken, true))
				}

				// Verify that if original was true or nil, it's now false
				if original != nil &&
					(ptr.Deref(original.Spec.AutomountServiceAccountToken, true)) &&
					!ptr.Deref(tt.template.Spec.AutomountServiceAccountToken, true) {
					// This is expected behavior - the function should set it to false
				}
			}

			// Additional checks for the nil case
			if tt.template == nil && tt.expected != nil {
				t.Errorf("Expected nil template to remain nil")
			}
			if tt.template != nil && tt.expected == nil {
				t.Errorf("Expected non-nil template but got nil")
			}
		})
	}
}

func TestSetDefaultVolumeClaimTemplates(t *testing.T) {
	// Define common volume mode values
	filesystemMode := corev1.PersistentVolumeFilesystem
	blockMode := corev1.PersistentVolumeBlock

	tests := []struct {
		name      string
		templates []corev1.PersistentVolumeClaim
		expected  []corev1.PersistentVolumeClaim
	}{
		{
			name:      "empty templates slice",
			templates: []corev1.PersistentVolumeClaim{},
			expected:  []corev1.PersistentVolumeClaim{},
		},
		{
			name:      "nil templates slice",
			templates: nil,
			expected:  nil,
		},
		{
			name: "PVC with no access modes - should default to ReadWriteOnce",
			templates: []corev1.PersistentVolumeClaim{
				{
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{},
					},
				},
			},
			expected: []corev1.PersistentVolumeClaim{
				{
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						VolumeMode:  &filesystemMode,
					},
				},
			},
		},
		{
			name: "PVC with existing access modes - should not change",
			templates: []corev1.PersistentVolumeClaim{
				{
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany},
					},
				},
			},
			expected: []corev1.PersistentVolumeClaim{
				{
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany},
						VolumeMode:  &filesystemMode,
					},
				},
			},
		},
		{
			name: "PVC with no volume mode - should default to Filesystem",
			templates: []corev1.PersistentVolumeClaim{
				{
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					},
				},
			},
			expected: []corev1.PersistentVolumeClaim{
				{
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						VolumeMode:  &filesystemMode,
					},
				},
			},
		},
		{
			name: "PVC with existing volume mode - should not change",
			templates: []corev1.PersistentVolumeClaim{
				{
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						VolumeMode:  &blockMode,
					},
				},
			},
			expected: []corev1.PersistentVolumeClaim{
				{
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						VolumeMode:  &blockMode,
					},
				},
			},
		},
		{
			name: "PVC with both access modes and volume mode set - should not change",
			templates: []corev1.PersistentVolumeClaim{
				{
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
						VolumeMode:  &filesystemMode,
					},
				},
			},
			expected: []corev1.PersistentVolumeClaim{
				{
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
						VolumeMode:  &filesystemMode,
					},
				},
			},
		},
		{
			name: "multiple PVCs with different configurations",
			templates: []corev1.PersistentVolumeClaim{
				{
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{},
					},
				},
				{
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany},
						VolumeMode:  &blockMode,
					},
				},
				{
					Spec: corev1.PersistentVolumeClaimSpec{},
				},
			},
			expected: []corev1.PersistentVolumeClaim{
				{
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						VolumeMode:  &filesystemMode,
					},
				},
				{
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany},
						VolumeMode:  &blockMode,
					},
				},
				{
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						VolumeMode:  &filesystemMode,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setDefaultVolumeClaimTemplates(tt.templates)

			// If expected is nil, actual should also be nil
			if tt.expected == nil {
				if tt.templates != nil {
					t.Errorf("Expected nil slice, got non-nil slice")
				}
				return
			}

			// Compare slice length
			if len(tt.templates) != len(tt.expected) {
				t.Errorf("Expected slice length %d, got %d", len(tt.expected), len(tt.templates))
				return
			}

			// Compare each PVC
			for i := range tt.templates {
				actualPVC := tt.templates[i]
				expectedPVC := tt.expected[i]

				// Compare AccessModes
				if len(actualPVC.Spec.AccessModes) != len(expectedPVC.Spec.AccessModes) {
					t.Errorf("PVC %d: Expected AccessModes length %d, got %d. Actual AccessModes: %v, Expected AccessModes: %v",
						i, len(expectedPVC.Spec.AccessModes), len(actualPVC.Spec.AccessModes),
						actualPVC.Spec.AccessModes, expectedPVC.Spec.AccessModes)
					continue
				}
				for j, actualMode := range actualPVC.Spec.AccessModes {
					if actualMode != expectedPVC.Spec.AccessModes[j] {
						t.Errorf("PVC %d: Expected AccessMode %v at index %d, got %v",
							i, expectedPVC.Spec.AccessModes[j], j, actualMode)
					}
				}

				// Compare VolumeMode - need special handling for pointers
				actualModeNil := actualPVC.Spec.VolumeMode == nil
				expectedModeNil := expectedPVC.Spec.VolumeMode == nil

				if actualModeNil != expectedModeNil {
					t.Errorf("PVC %d: VolumeMode nil mismatch - actual is nil: %v, expected is nil: %v",
						i, actualModeNil, expectedModeNil)
					continue
				}

				if !actualModeNil && *actualPVC.Spec.VolumeMode != *expectedPVC.Spec.VolumeMode {
					t.Errorf("PVC %d: Expected VolumeMode %v, got %v",
						i, *expectedPVC.Spec.VolumeMode, *actualPVC.Spec.VolumeMode)
				}
			}
		})
	}
}

// Helper function to deep copy PodTemplateSpec for testing
func deepCopyPodTemplateSpec(template *corev1.PodTemplateSpec) *corev1.PodTemplateSpec {
	if template == nil {
		return nil
	}

	result := &corev1.PodTemplateSpec{}
	result.ObjectMeta = *template.ObjectMeta.DeepCopy()
	result.Spec = *template.Spec.DeepCopy()
	return result
}
