package validating

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

func TestPodValidatingHandler_Path(t *testing.T) {
	handler := &PodValidatingHandler{}
	require.Equal(t, "/validate-pod-delete", handler.Path())
}

func TestPodValidatingHandler_Enabled(t *testing.T) {
	handler := &PodValidatingHandler{}
	require.True(t, handler.Enabled())
}

func TestPodValidatingHandler_Handle(t *testing.T) {
	// Add v1alpha1 to scheme
	err := agentsv1alpha1.AddToScheme(scheme.Scheme)
	require.NoError(t, err)

	now := metav1.Now()
	sandboxControllerUser := utils.GetSandboxControllerUsername()

	tests := []struct {
		name        string
		pod         *corev1.Pod
		sandbox     *agentsv1alpha1.Sandbox
		operation   admissionv1.Operation
		subResource string
		username    string
		expectAllow bool
	}{
		{
			name: "sandbox controller user - should allow regardless",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sandbox-pod",
					Namespace: "default",
					Labels: map[string]string{
						utils.PodLabelCreatedBy: utils.CreatedBySandbox,
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: agentsv1alpha1.SchemeGroupVersion.String(),
							Kind:       "Sandbox",
							Name:       "sandbox-pod",
							UID:        "test-uid",
						},
					},
				},
			},
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "sandbox-pod",
					Namespace:         "default",
					CreationTimestamp: now,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			operation:   admissionv1.Delete,
			username:    sandboxControllerUser,
			expectAllow: true,
		},
		{
			name: "non-delete operation - should allow",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sandbox-pod",
					Namespace: "default",
					Labels: map[string]string{
						utils.PodLabelCreatedBy: utils.CreatedBySandbox,
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: agentsv1alpha1.SchemeGroupVersion.String(),
							Kind:       "Sandbox",
							Name:       "sandbox-pod",
							UID:        "test-uid",
						},
					},
				},
			},
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "sandbox-pod",
					Namespace:         "default",
					CreationTimestamp: now,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			operation:   admissionv1.Create,
			username:    "regular-user",
			expectAllow: true,
		},
		{
			name: "non-sandbox pod deletion - should allow",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "normal-pod",
					Namespace: "default",
				},
			},
			operation:   admissionv1.Delete,
			username:    "regular-user",
			expectAllow: true,
		},
		{
			name: "sandbox pod without owner reference - should allow",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sandbox-pod",
					Namespace: "default",
					Labels: map[string]string{
						utils.PodLabelCreatedBy: utils.CreatedBySandbox,
					},
				},
			},
			operation:   admissionv1.Delete,
			username:    "regular-user",
			expectAllow: true,
		},
		{
			name: "pod already being deleted - should allow",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "sandbox-pod",
					Namespace:         "default",
					DeletionTimestamp: &now, // Pod is already being deleted
					Finalizers:        []string{"test-finalizer"},
					Labels: map[string]string{
						utils.PodLabelCreatedBy: utils.CreatedBySandbox,
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: agentsv1alpha1.SchemeGroupVersion.String(),
							Kind:       "Sandbox",
							Name:       "sandbox-pod",
							UID:        "test-uid",
						},
					},
				},
			},
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "sandbox-pod",
					Namespace:         "default",
					CreationTimestamp: now,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			operation:   admissionv1.Delete,
			username:    "regular-user",
			expectAllow: true,
		},
		{
			name: "sandbox pod with sandbox not found - should allow",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sandbox-pod",
					Namespace: "default",
					Labels: map[string]string{
						utils.PodLabelCreatedBy: utils.CreatedBySandbox,
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: agentsv1alpha1.SchemeGroupVersion.String(),
							Kind:       "Sandbox",
							Name:       "sandbox-pod",
							UID:        "test-uid",
						},
					},
				},
			},
			operation:   admissionv1.Delete,
			username:    "regular-user",
			expectAllow: true,
		},
		{
			name: "sandbox pod with sandbox exists and not deleting - should deny",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sandbox-pod",
					Namespace: "default",
					Labels: map[string]string{
						utils.PodLabelCreatedBy: utils.CreatedBySandbox,
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: agentsv1alpha1.SchemeGroupVersion.String(),
							Kind:       "Sandbox",
							Name:       "sandbox-pod",
							UID:        "test-uid",
						},
					},
				},
			},
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "sandbox-pod",
					Namespace:         "default",
					CreationTimestamp: now,
					// DeletionTimestamp is nil, sandbox is not being deleted
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			operation:   admissionv1.Delete,
			username:    "regular-user",
			expectAllow: false,
		},
		{
			name: "sandbox pod with sandbox exists and deleting - should allow",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sandbox-pod",
					Namespace: "default",
					Labels: map[string]string{
						utils.PodLabelCreatedBy: utils.CreatedBySandbox,
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: agentsv1alpha1.SchemeGroupVersion.String(),
							Kind:       "Sandbox",
							Name:       "sandbox-pod",
							UID:        "test-uid",
						},
					},
				},
			},
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "sandbox-pod",
					Namespace:         "default",
					CreationTimestamp: now,
					DeletionTimestamp: &now,                                 // Sandbox is being deleted
					Finalizers:        []string{"agents.kruise.io/sandbox"}, // Required by fake client when DeletionTimestamp is set
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			operation:   admissionv1.Delete,
			username:    "regular-user",
			expectAllow: true,
		},
		{
			name: "sandbox pod with wrong owner kind - should allow",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sandbox-pod",
					Namespace: "default",
					Labels: map[string]string{
						utils.PodLabelCreatedBy: utils.CreatedBySandbox,
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "apps/v1",
							Kind:       "ReplicaSet",
							Name:       "rs-123",
							UID:        "test-uid",
						},
					},
				},
			},
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "sandbox-pod",
					Namespace:         "default",
					CreationTimestamp: now,
				},
			},
			operation:   admissionv1.Delete,
			username:    "regular-user",
			expectAllow: true,
		},
		// Eviction test cases
		{
			name: "eviction - non-sandbox pod - should allow",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "normal-pod",
					Namespace: "default",
				},
			},
			operation:   admissionv1.Create,
			subResource: "eviction",
			username:    "regular-user",
			expectAllow: true,
		},
		{
			name: "eviction - sandbox pod with sandbox exists - should deny",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sandbox-pod",
					Namespace: "default",
					Labels: map[string]string{
						utils.PodLabelCreatedBy: utils.CreatedBySandbox,
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: agentsv1alpha1.SchemeGroupVersion.String(),
							Kind:       "Sandbox",
							Name:       "sandbox-pod",
							UID:        "test-uid",
						},
					},
				},
			},
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "sandbox-pod",
					Namespace:         "default",
					CreationTimestamp: now,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			operation:   admissionv1.Create,
			subResource: "eviction",
			username:    "regular-user",
			expectAllow: false,
		},
		{
			name: "eviction - sandbox pod with sandbox deleting - should allow",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sandbox-pod",
					Namespace: "default",
					Labels: map[string]string{
						utils.PodLabelCreatedBy: utils.CreatedBySandbox,
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: agentsv1alpha1.SchemeGroupVersion.String(),
							Kind:       "Sandbox",
							Name:       "sandbox-pod",
							UID:        "test-uid",
						},
					},
				},
			},
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "sandbox-pod",
					Namespace:         "default",
					CreationTimestamp: now,
					DeletionTimestamp: &now,
					Finalizers:        []string{"agents.kruise.io/sandbox"},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			operation:   admissionv1.Create,
			subResource: "eviction",
			username:    "regular-user",
			expectAllow: true,
		},
		{
			name: "eviction - create operation without eviction subresource - should allow",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sandbox-pod",
					Namespace: "default",
					Labels: map[string]string{
						utils.PodLabelCreatedBy: utils.CreatedBySandbox,
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: agentsv1alpha1.SchemeGroupVersion.String(),
							Kind:       "Sandbox",
							Name:       "sandbox-pod",
							UID:        "test-uid",
						},
					},
				},
			},
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "sandbox-pod",
					Namespace:         "default",
					CreationTimestamp: now,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			operation:   admissionv1.Create,
			subResource: "", // Not eviction
			username:    "regular-user",
			expectAllow: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewGomegaWithT(t)

			// Create fake client
			objs := []runtime.Object{}
			if tt.sandbox != nil {
				objs = append(objs, tt.sandbox)
			}
			// For eviction tests, pod needs to be in the client
			if tt.subResource == "eviction" && tt.pod != nil {
				objs = append(objs, tt.pod)
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(objs...).Build()

			// Create decoder
			decoder := admission.NewDecoder(scheme.Scheme)

			// Create handler
			handler := &PodValidatingHandler{
				Client:  fakeClient,
				Decoder: decoder,
			}

			// Construct admission request
			var req admission.Request
			if tt.subResource == "eviction" {
				// For eviction, the object is an Eviction resource
				eviction := &policyv1.Eviction{
					ObjectMeta: metav1.ObjectMeta{
						Name:      tt.pod.Name,
						Namespace: tt.pod.Namespace,
					},
				}
				evictionRaw, err := json.Marshal(eviction)
				require.NoError(t, err)

				req = admission.Request{
					AdmissionRequest: admissionv1.AdmissionRequest{
						Operation:   tt.operation,
						SubResource: tt.subResource,
						Namespace:   tt.pod.Namespace,
						Name:        tt.pod.Name,
						UserInfo: authenticationv1.UserInfo{
							Username: tt.username,
						},
						Object: runtime.RawExtension{
							Raw: evictionRaw,
						},
					},
				}
			} else {
				// For delete, the object is in OldObject
				podRaw, err := json.Marshal(tt.pod)
				require.NoError(t, err)

				req = admission.Request{
					AdmissionRequest: admissionv1.AdmissionRequest{
						Operation: tt.operation,
						UserInfo: authenticationv1.UserInfo{
							Username: tt.username,
						},
						Object: runtime.RawExtension{
							Raw: podRaw,
						},
						OldObject: runtime.RawExtension{
							Raw: podRaw,
						},
					},
				}
			}

			response := handler.Handle(context.TODO(), req)

			// Verify results
			if tt.expectAllow {
				g.Expect(response.Allowed).To(gomega.BeTrue())
			} else {
				g.Expect(response.Allowed).To(gomega.BeFalse())
			}
		})
	}
}

func TestPodValidatingHandler_Handle_DecodeError(t *testing.T) {
	// Create decoder
	decoder := admission.NewDecoder(scheme.Scheme)

	// Create handler
	handler := &PodValidatingHandler{
		Client:  fake.NewClientBuilder().WithScheme(scheme.Scheme).Build(),
		Decoder: decoder,
	}

	// Construct admission request with invalid JSON
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Delete,
			UserInfo: authenticationv1.UserInfo{
				Username: "regular-user",
			},
			OldObject: runtime.RawExtension{
				Raw: []byte("invalid json"),
			},
		},
	}

	response := handler.Handle(context.TODO(), req)

	// Should return error when decode fails
	require.False(t, response.Allowed)
	require.NotNil(t, response.Result)
}

func TestPodValidatingHandler_Handle_EvictionDecodeError(t *testing.T) {
	// Create decoder
	decoder := admission.NewDecoder(scheme.Scheme)

	// Create handler
	handler := &PodValidatingHandler{
		Client:  fake.NewClientBuilder().WithScheme(scheme.Scheme).Build(),
		Decoder: decoder,
	}

	// Construct admission request with invalid eviction JSON
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation:   admissionv1.Create,
			SubResource: "eviction",
			Namespace:   "default",
			Name:        "test-pod",
			UserInfo: authenticationv1.UserInfo{
				Username: "regular-user",
			},
			Object: runtime.RawExtension{
				Raw: []byte("invalid json"),
			},
		},
	}

	response := handler.Handle(context.TODO(), req)

	// Should return error when decode fails
	require.False(t, response.Allowed)
	require.NotNil(t, response.Result)
}

func TestPodValidatingHandler_Handle_EvictionPodNotFound(t *testing.T) {
	// Add v1alpha1 to scheme
	err := agentsv1alpha1.AddToScheme(scheme.Scheme)
	require.NoError(t, err)

	// Create decoder
	decoder := admission.NewDecoder(scheme.Scheme)

	// Create handler without any pod in the client
	handler := &PodValidatingHandler{
		Client:  fake.NewClientBuilder().WithScheme(scheme.Scheme).Build(),
		Decoder: decoder,
	}

	// Construct eviction request for non-existent pod
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "non-existent-pod",
			Namespace: "default",
		},
	}
	evictionRaw, err := json.Marshal(eviction)
	require.NoError(t, err)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation:   admissionv1.Create,
			SubResource: "eviction",
			Namespace:   "default",
			Name:        "non-existent-pod",
			UserInfo: authenticationv1.UserInfo{
				Username: "regular-user",
			},
			Object: runtime.RawExtension{
				Raw: evictionRaw,
			},
		},
	}

	response := handler.Handle(context.TODO(), req)

	// Should allow when pod not found
	require.True(t, response.Allowed)
}

func TestPodValidatingHandler_Handle_OtherOperation(t *testing.T) {
	// Create decoder
	decoder := admission.NewDecoder(scheme.Scheme)

	// Create handler
	handler := &PodValidatingHandler{
		Client:  fake.NewClientBuilder().WithScheme(scheme.Scheme).Build(),
		Decoder: decoder,
	}

	// Construct admission request with UPDATE operation
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
	}
	podRaw, err := json.Marshal(pod)
	require.NoError(t, err)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			UserInfo: authenticationv1.UserInfo{
				Username: "regular-user",
			},
			Object: runtime.RawExtension{
				Raw: podRaw,
			},
		},
	}

	response := handler.Handle(context.TODO(), req)

	// Should allow other operations
	require.True(t, response.Allowed)
}
