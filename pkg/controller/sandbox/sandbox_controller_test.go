/*
Copyright 2025.

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

package sandbox

import (
	"context"
	"testing"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/sandbox/core"
	"github.com/openkruise/agents/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSandboxReconciler_Reconcile(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name               string
		sandbox            *agentsv1alpha1.Sandbox
		pod                *corev1.Pod
		expectedPhase      agentsv1alpha1.SandboxPhase
		expectRequeue      bool
		expectRequeueAfter time.Duration
		wantErr            bool
	}{
		{
			name: "sandbox not found - should return nil",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "nonexistent-sandbox",
					Namespace: "default",
				},
			},
			pod:           nil,
			expectedPhase: "", // No update expected
			wantErr:       false,
		},
		{
			name: "sandbox template is nil - should return nil",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "nil-template-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: nil,
					},
				},
			},
			pod:           nil,
			expectedPhase: "", // No update expected
			wantErr:       false,
		},
		{
			name: "sandbox in failed state - should return nil",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "failed-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxFailed,
				},
			},
			pod:           nil,
			expectedPhase: agentsv1alpha1.SandboxFailed,
			wantErr:       false,
		},
		{
			name: "sandbox in succeeded state - should return nil",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "succeeded-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxSucceeded,
				},
			},
			pod:           nil,
			expectedPhase: agentsv1alpha1.SandboxSucceeded,
			wantErr:       false,
		},
		{
			name: "new sandbox - should set to pending",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "new-sandbox",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
			},
			pod:           nil,
			expectedPhase: "",
			wantErr:       false,
		},
		{
			name: "sandbox with deletion timestamp - should set to terminating",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "terminating-sandbox",
					Namespace:         "default",
					DeletionTimestamp: &metav1.Time{Time: time.Now()},
					Finalizers:        []string{utils.SandboxFinalizer},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "terminating-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			expectedPhase: agentsv1alpha1.SandboxRunning,
			wantErr:       false,
		},
		{
			name: "pod succeeded - should set sandbox to succeeded",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "succeeded-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "succeeded-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodSucceeded,
				},
			},
			expectedPhase: agentsv1alpha1.SandboxSucceeded,
			wantErr:       false,
		},
		{
			name: "pod failed - should set sandbox to failed",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "failed-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "failed-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
				},
			},
			expectedPhase: agentsv1alpha1.SandboxFailed,
			wantErr:       false,
		},
		{
			name: "sandbox paused - should set to paused",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "paused-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: true,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "paused-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			expectedPhase: agentsv1alpha1.SandboxPaused,
			wantErr:       false,
		},
		{
			name: "sandbox resuming - should set to resuming",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "resuming-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: false,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionPaused),
							Status:             metav1.ConditionTrue,
							Reason:             agentsv1alpha1.SandboxPausedReasonDeletePod,
							LastTransitionTime: metav1.Now(),
						},
					},
				},
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "resuming-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			expectedPhase: agentsv1alpha1.SandboxResuming,
			wantErr:       false,
		},
		{
			name: "sandbox with shutdownTime in past - should be deleted",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "shutdown-sandbox",
					Namespace:  "default",
					Finalizers: []string{utils.SandboxFinalizer},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					ShutdownTime: &metav1.Time{Time: time.Now().Add(-1 * time.Hour)},
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			pod:           nil,
			expectedPhase: agentsv1alpha1.SandboxRunning,
			wantErr:       false,
		},
		{
			name: "sandbox with pauseTime in past - should be paused",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "pause-time-sandbox",
					Namespace:  "default",
					Finalizers: []string{utils.SandboxFinalizer},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					PauseTime: &metav1.Time{Time: time.Now().Add(-1 * time.Hour)},
					Paused:    false,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pause-time-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			expectedPhase: agentsv1alpha1.SandboxRunning,
			wantErr:       false,
		},
		{
			name: "sandbox with shutdownTime in future - should requeue",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "future-shutdown-sandbox",
					Namespace:  "default",
					Finalizers: []string{utils.SandboxFinalizer},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					ShutdownTime: &metav1.Time{Time: time.Now().Add(10 * time.Minute)},
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPending,
				},
			},
			pod:                nil,
			expectedPhase:      agentsv1alpha1.SandboxPending,
			wantErr:            false,
			expectRequeue:      false,
			expectRequeueAfter: time.Minute,
		},
		{
			name: "pod not found but sandbox running - should set to failed",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "pod-missing-sandbox",
					Namespace:  "default",
					Finalizers: []string{utils.SandboxFinalizer},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			pod:           nil,
			expectedPhase: agentsv1alpha1.SandboxFailed,
			wantErr:       false,
		},
		{
			name: "sandbox with volumeClaimTemplates - should create PVCs",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pvc-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
						VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: "data",
								},
								Spec: corev1.PersistentVolumeClaimSpec{
									AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
									Resources: corev1.VolumeResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceStorage: resource.MustParse("1Gi"),
										},
									},
								},
							},
						},
					},
				},
			},
			pod:           nil,
			expectedPhase: "",
			wantErr:       false,
		},
		{
			name: "sandbox with invalid phase - should log and return",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "invalid-phase-sandbox",
					Namespace:  "default",
					Finalizers: []string{utils.SandboxFinalizer},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPhase("InvalidPhase"),
				},
			},
			pod:           nil,
			expectedPhase: agentsv1alpha1.SandboxPhase("InvalidPhase"),
			wantErr:       false,
		},
		{
			name: "sandbox with both shutdownTime and pauseTime - should use minimum requeue time",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "both-times-sandbox",
					Namespace:  "default",
					Finalizers: []string{utils.SandboxFinalizer},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					ShutdownTime: &metav1.Time{Time: time.Now().Add(10 * time.Minute)},
					PauseTime:    &metav1.Time{Time: time.Now().Add(5 * time.Minute)},
					Paused:       false,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPending,
				},
			},
			pod:                nil,
			expectedPhase:      agentsv1alpha1.SandboxPending,
			wantErr:            false,
			expectRequeueAfter: time.Minute,
		},
		{
			name: "sandbox with annotations is nil - should create empty map",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "no-annotations-sandbox",
					Namespace:   "default",
					Annotations: nil,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
			},
			pod:           nil,
			expectedPhase: "",
			wantErr:       false,
		},
		{
			name: "sandbox with volumeClaimTemplates error - should return error",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "error-pvc-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
						VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: "", // Empty name will cause error
								},
								Spec: corev1.PersistentVolumeClaimSpec{
									AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
									Resources: corev1.VolumeResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceStorage: resource.MustParse("1Gi"),
										},
									},
								},
							},
						},
					},
				},
			},
			pod:           nil,
			expectedPhase: "",
			wantErr:       true,
		},
		{
			name: "sandbox pending with pod completed - shouldRequeue true",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "completed-pod-sandbox",
					Namespace:  "default",
					Finalizers: []string{utils.SandboxFinalizer},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPending,
				},
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "completed-pod-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodSucceeded,
				},
			},
			expectedPhase: agentsv1alpha1.SandboxSucceeded,
			wantErr:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := []client.Object{}
			if tt.sandbox != nil {
				objects = append(objects, tt.sandbox)
			}
			if tt.pod != nil {
				objects = append(objects, tt.pod)
			}
			fakeRecorder := record.NewFakeRecorder(100)
			client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Sandbox{}).WithObjects(objects...).Build()
			reconciler := &SandboxReconciler{
				Client:   client,
				Scheme:   scheme,
				controls: core.NewSandboxControl(client, fakeRecorder),
			}
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      tt.sandbox.Name,
					Namespace: tt.sandbox.Namespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			if (err != nil) != tt.wantErr {
				t.Errorf("SandboxReconciler.Reconcile() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			// Check if error expectations match
			if tt.wantErr && err == nil {
				t.Errorf("Expected error but got none")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			// If no error and sandbox exists, verify status was updated
			if !tt.wantErr && tt.sandbox != nil {
				updatedSandbox := &agentsv1alpha1.Sandbox{}
				err = client.Get(context.TODO(), req.NamespacedName, updatedSandbox)
				if err != nil {
					t.Errorf("Failed to get updated sandbox: %v", err)
				} else if tt.expectedPhase != "" && updatedSandbox.Status.Phase != tt.expectedPhase {
					t.Errorf("Expected sandbox phase %v, got %v", tt.expectedPhase, updatedSandbox.Status.Phase)
				}
			}

			// Verify requeue expectations if applicable
			if tt.expectRequeue && result.Requeue == false {
				t.Errorf("Expected requeue but got no requeue")
			}
			if tt.expectRequeueAfter > 0 && result.RequeueAfter <= 0 {
				t.Errorf("Expected requeue after %v, but got %v", tt.expectRequeueAfter, result.RequeueAfter)
			}
		})
	}
}

func TestSandboxReconciler_updateSandboxStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Sandbox{}).Build()
	reconciler := &SandboxReconciler{
		Client: client,
		Scheme: scheme,
	}

	originalSandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxPending,
		},
	}

	// Add the sandbox to the client
	err := client.Create(context.TODO(), originalSandbox)
	if err != nil {
		t.Fatalf("Failed to create sandbox: %v", err)
	}

	newStatus := agentsv1alpha1.SandboxStatus{
		Phase: agentsv1alpha1.SandboxRunning,
	}

	err = reconciler.updateSandboxStatus(context.Background(), newStatus, originalSandbox)
	if err != nil {
		t.Errorf("updateSandboxStatus() error = %v", err)
		return
	}

	// Verify the status was updated
	updatedSandbox := &agentsv1alpha1.Sandbox{}
	err = client.Get(context.TODO(), types.NamespacedName{Name: originalSandbox.Name, Namespace: originalSandbox.Namespace}, updatedSandbox)
	if err != nil {
		t.Errorf("Failed to get updated sandbox: %v", err)
	} else if updatedSandbox.Status.Phase != agentsv1alpha1.SandboxRunning {
		t.Errorf("Expected sandbox phase %v, got %v", agentsv1alpha1.SandboxRunning, updatedSandbox.Status.Phase)
	}
}

func TestSandboxReconciler_updateSandboxStatusNoChange(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &SandboxReconciler{
		Client: client,
		Scheme: scheme,
	}

	originalSandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxPending,
		},
	}

	// Add the sandbox to the client
	err := client.Create(context.TODO(), originalSandbox)
	if err != nil {
		t.Fatalf("Failed to create sandbox: %v", err)
	}

	// Try to update with the same status (should not update)
	sameStatus := agentsv1alpha1.SandboxStatus{
		Phase: agentsv1alpha1.SandboxPending,
	}

	err = reconciler.updateSandboxStatus(context.Background(), sameStatus, originalSandbox)
	if err != nil {
		t.Errorf("updateSandboxStatus() error = %v", err)
		return
	}

	// Status should remain the same
	updatedSandbox := &agentsv1alpha1.Sandbox{}
	err = client.Get(context.TODO(), types.NamespacedName{Name: originalSandbox.Name, Namespace: originalSandbox.Namespace}, updatedSandbox)
	if err != nil {
		t.Errorf("Failed to get updated sandbox: %v", err)
	} else if updatedSandbox.Status.Phase != agentsv1alpha1.SandboxPending {
		t.Errorf("Expected sandbox phase %v, got %v", agentsv1alpha1.SandboxPending, updatedSandbox.Status.Phase)
	}
}

func TestSandboxReconciler_updateSandboxStatusWithPendingPhase(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Sandbox{}).Build()
	reconciler := &SandboxReconciler{
		Client: client,
		Scheme: scheme,
	}

	originalSandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
		},
	}

	// Add the sandbox to the client
	err := client.Create(context.TODO(), originalSandbox)
	if err != nil {
		t.Fatalf("Failed to create sandbox: %v", err)
	}

	// Try to update with Pending phase (should not update because of early return)
	pendingStatus := agentsv1alpha1.SandboxStatus{
		Phase: agentsv1alpha1.SandboxPending,
	}

	err = reconciler.updateSandboxStatus(context.Background(), pendingStatus, originalSandbox)
	if err != nil {
		t.Errorf("updateSandboxStatus() error = %v, want nil", err)
		return
	}

	// Status should remain Running because Pending phase updates are skipped
	updatedSandbox := &agentsv1alpha1.Sandbox{}
	err = client.Get(context.TODO(), types.NamespacedName{Name: originalSandbox.Name, Namespace: originalSandbox.Namespace}, updatedSandbox)
	if err != nil {
		t.Errorf("Failed to get updated sandbox: %v", err)
	} else if updatedSandbox.Status.Phase != agentsv1alpha1.SandboxRunning {
		t.Errorf("Expected sandbox phase to remain %v, got %v", agentsv1alpha1.SandboxRunning, updatedSandbox.Status.Phase)
	}
}

func TestSandboxReconciler_ShutdownTime(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	fakeRecorder := record.NewFakeRecorder(100)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &SandboxReconciler{
		Client:   client,
		Scheme:   scheme,
		controls: core.NewSandboxControl(client, fakeRecorder),
	}

	// Create a sandbox with a shutdown time in the past
	pastTime := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shutdown-sandbox",
			Namespace: "default",
		},
		Spec: agentsv1alpha1.SandboxSpec{
			ShutdownTime: &pastTime,
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
	}
	// Add the sandbox to the client
	err := client.Create(context.TODO(), sandbox)
	if err != nil {
		t.Fatalf("Failed to create sandbox: %v", err)
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		},
	}

	// This should delete the sandbox since shutdown time has passed
	_, err = reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile() error = %v", err)
	}
	// Verify the sandbox was deleted
	updatedSandbox := &agentsv1alpha1.Sandbox{}
	err = client.Get(context.TODO(), req.NamespacedName, updatedSandbox)
	if err == nil && updatedSandbox.DeletionTimestamp.IsZero() {
		t.Errorf("Expected sandbox to be deleted, but it still exists")
	}
}

func TestSandboxReconcile_WithVolumeClaimTemplates(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name           string
		sandbox        *agentsv1alpha1.Sandbox
		existingPVCs   []client.Object
		expectPVCCount int
		expectPVCNames []string
		wantErr        bool
	}{
		{
			name: "no volume claim templates - should not create any PVCs",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
			},
			expectPVCCount: 0,
			expectPVCNames: []string{},
			wantErr:        false,
		},
		{
			name: "single volume claim template - should create one PVC",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
						VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: "www",
								},
								Spec: corev1.PersistentVolumeClaimSpec{
									AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
									Resources: corev1.VolumeResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceStorage: resource.MustParse("1Gi"),
										},
									},
								},
							},
						},
					},
				},
			},
			expectPVCCount: 1,
			expectPVCNames: []string{"www-test-sandbox"},
			wantErr:        false,
		},
		{
			name: "multiple volume claim templates - should create multiple PVCs",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
						VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: "www",
								},
								Spec: corev1.PersistentVolumeClaimSpec{
									AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
									Resources: corev1.VolumeResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceStorage: resource.MustParse("1Gi"),
										},
									},
								},
							},
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: "data",
								},
								Spec: corev1.PersistentVolumeClaimSpec{
									AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
									Resources: corev1.VolumeResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceStorage: resource.MustParse("5Gi"),
										},
									},
								},
							},
						},
					},
				},
			},
			expectPVCCount: 2,
			expectPVCNames: []string{"www-test-sandbox", "data-test-sandbox"},
			wantErr:        false,
		},
		{
			name: "PVC already exists - should not create duplicate",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
						VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: "www",
								},
								Spec: corev1.PersistentVolumeClaimSpec{
									AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
									Resources: corev1.VolumeResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceStorage: resource.MustParse("1Gi"),
										},
									},
								},
							},
						},
					},
				},
			},
			existingPVCs: []client.Object{
				&corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "www-test-sandbox",
						Namespace: "default",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("1Gi"),
							},
						},
					},
				},
			},
			expectPVCCount: 1,
			expectPVCNames: []string{"www-test-sandbox"},
			wantErr:        false,
		},
		{
			name: "PVC with empty template name - should return error",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
						VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
							{
								ObjectMeta: metav1.ObjectMeta{
									Name: "",
								},
								Spec: corev1.PersistentVolumeClaimSpec{
									AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
									Resources: corev1.VolumeResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceStorage: resource.MustParse("1Gi"),
										},
									},
								},
							},
						},
					},
				},
			},
			existingPVCs:   []client.Object{},
			expectPVCCount: 0,
			expectPVCNames: []string{},
			wantErr:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup client with existing PVCs if any
			objects := []client.Object{}
			if tt.sandbox != nil {
				objects = append(objects, tt.sandbox)
			}
			objects = append(objects, tt.existingPVCs...)

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objects...).
				Build()

			reconciler := &SandboxReconciler{
				Client: fakeClient,
				Scheme: scheme,
			}

			ctx := context.Background()
			err := reconciler.ensureVolumeClaimTemplates(ctx, tt.sandbox)

			if (err != nil) != tt.wantErr {
				t.Errorf("ensureVolumeClaimTemplates() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				// List PVCs to verify they were created
				pvcList := &corev1.PersistentVolumeClaimList{}
				err = fakeClient.List(ctx, pvcList, client.InNamespace(tt.sandbox.Namespace))
				if err != nil {
					t.Errorf("Failed to list PVCs: %v", err)
					return
				}

				if len(pvcList.Items) != tt.expectPVCCount {
					t.Errorf("Expected %d PVCs, got %d", tt.expectPVCCount, len(pvcList.Items))
				}

				// Verify expected PVC names exist
				createdPVCNames := make([]string, len(pvcList.Items))
				for i, pvc := range pvcList.Items {
					createdPVCNames[i] = pvc.Name
				}

				for _, expectedName := range tt.expectPVCNames {
					found := false
					for _, actualName := range createdPVCNames {
						if actualName == expectedName {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("Expected PVC %s not found in created PVCs: %v", expectedName, createdPVCNames)
					}
				}

				// Verify PVC ownership for newly created PVCs
				for _, pvc := range pvcList.Items {
					// Skip checking ownership for existing PVCs that were in the initial objects
					isExistingPVC := false
					for _, existingPVC := range tt.existingPVCs {
						if existingPVC.GetName() == pvc.Name {
							isExistingPVC = true
							break
						}
					}

					if !isExistingPVC {
						if len(pvc.OwnerReferences) == 0 {
							t.Errorf("PVC %s does not have owner reference", pvc.Name)
							continue
						}
						ownerRef := pvc.OwnerReferences[0]
						if ownerRef.Name != tt.sandbox.Name {
							t.Errorf("PVC %s owner reference name is %s, expected %s", pvc.Name, ownerRef.Name, tt.sandbox.Name)
						}
					}
				}
			}
		})
	}
}

func TestCalculateStatus(t *testing.T) {
	tests := []struct {
		name              string
		pod               *corev1.Pod
		box               *agentsv1alpha1.Sandbox
		initStatus        *agentsv1alpha1.SandboxStatus
		expectedPhase     agentsv1alpha1.SandboxPhase
		expectedMessage   string
		expectedShouldReq bool
		checkConditions   func(t *testing.T, status *agentsv1alpha1.SandboxStatus)
	}{
		{
			name: "empty phase should set to pending",
			pod:  nil,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
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
			},
			initStatus:        &agentsv1alpha1.SandboxStatus{},
			expectedPhase:     agentsv1alpha1.SandboxPending,
			expectedShouldReq: false,
		},
		{
			name: "running phase with nil pod should set to failed",
			pod:  nil,
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
			},
			initStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
			},
			expectedPhase:     agentsv1alpha1.SandboxFailed,
			expectedMessage:   "Pod Not Found",
			expectedShouldReq: true,
		},
		{
			name: "running phase with pod deletion timestamp should set to failed",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-sandbox",
					Namespace:         "default",
					DeletionTimestamp: &metav1.Time{Time: time.Now()},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
			},
			initStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
			},
			expectedPhase:     agentsv1alpha1.SandboxFailed,
			expectedMessage:   "Pod Not Found",
			expectedShouldReq: true,
		},
		{
			name: "running phase with pod succeeded should set to succeeded",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodSucceeded,
				},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
			},
			initStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
			},
			expectedPhase:     agentsv1alpha1.SandboxSucceeded,
			expectedMessage:   "Pod status phase is Succeeded",
			expectedShouldReq: true,
		},
		{
			name: "running phase with pod failed should set to failed",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
				},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
			},
			initStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
			},
			expectedPhase:     agentsv1alpha1.SandboxFailed,
			expectedMessage:   "Pod status phase is Failed",
			expectedShouldReq: true,
		},
		{
			name: "running phase with paused spec should set to paused",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: true,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
			},
			initStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
				Conditions: []metav1.Condition{
					{
						Type:   string(agentsv1alpha1.SandboxConditionResumed),
						Status: metav1.ConditionTrue,
					},
				},
			},
			expectedPhase:     agentsv1alpha1.SandboxPaused,
			expectedShouldReq: false,
			checkConditions: func(t *testing.T, status *agentsv1alpha1.SandboxStatus) {
				// Should remove resumed condition
				for _, cond := range status.Conditions {
					if cond.Type == string(agentsv1alpha1.SandboxConditionResumed) {
						t.Errorf("Resumed condition should be removed")
					}
				}
			},
		},
		{
			name: "paused phase with paused condition true and not paused spec should set to resuming",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: false,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
			},
			initStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxPaused,
				Conditions: []metav1.Condition{
					{
						Type:   string(agentsv1alpha1.SandboxConditionPaused),
						Status: metav1.ConditionTrue,
					},
				},
			},
			expectedPhase:     agentsv1alpha1.SandboxResuming,
			expectedShouldReq: false,
			checkConditions: func(t *testing.T, status *agentsv1alpha1.SandboxStatus) {
				// Should remove paused condition
				for _, cond := range status.Conditions {
					if cond.Type == string(agentsv1alpha1.SandboxConditionPaused) {
						t.Errorf("Paused condition should be removed")
					}
				}
				// Should add resumed condition with false status
				found := false
				for _, cond := range status.Conditions {
					if cond.Type == string(agentsv1alpha1.SandboxConditionResumed) {
						found = true
						if cond.Status != metav1.ConditionFalse {
							t.Errorf("Resumed condition status should be false, got %s", cond.Status)
						}
						if cond.Reason != agentsv1alpha1.SandboxResumeReasonCreatePod {
							t.Errorf("Resumed condition reason should be CreatePod, got %s", cond.Reason)
						}
					}
				}
				if !found {
					t.Errorf("Resumed condition should be added")
				}
			},
		},
		{
			name: "paused phase with paused condition false and not paused spec should stay paused",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: false,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
			},
			initStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxPaused,
				Conditions: []metav1.Condition{
					{
						Type:   string(agentsv1alpha1.SandboxConditionPaused),
						Status: metav1.ConditionFalse,
					},
				},
			},
			expectedPhase:     agentsv1alpha1.SandboxPaused,
			expectedShouldReq: false,
		},
		{
			name: "running phase with running pod should stay running",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: false,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
			},
			initStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
			},
			expectedPhase:     agentsv1alpha1.SandboxRunning,
			expectedShouldReq: false,
		},
		{
			name: "pending phase with pod failed should set to failed",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
				},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
			},
			initStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxPending,
			},
			expectedPhase:     agentsv1alpha1.SandboxFailed,
			expectedMessage:   "Pod status phase is Failed",
			expectedShouldReq: true,
		},
		{
			name: "pending phase with pod succeed should set to succeed",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodSucceeded,
				},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
			},
			initStatus:        &agentsv1alpha1.SandboxStatus{},
			expectedPhase:     agentsv1alpha1.SandboxSucceeded,
			expectedMessage:   "Pod status phase is Succeeded",
			expectedShouldReq: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := core.EnsureFuncArgs{
				Pod:       tt.pod,
				Box:       tt.box,
				NewStatus: tt.initStatus,
			}

			newStatus, shouldRequeue := calculateStatus(args)

			if newStatus.Phase != tt.expectedPhase {
				t.Errorf("Expected phase %s, got %s", tt.expectedPhase, newStatus.Phase)
			}

			if tt.expectedMessage != "" && newStatus.Message != tt.expectedMessage {
				t.Errorf("Expected message %s, got %s", tt.expectedMessage, newStatus.Message)
			}

			if shouldRequeue != tt.expectedShouldReq {
				t.Errorf("Expected shouldRequeue %v, got %v", tt.expectedShouldReq, shouldRequeue)
			}

			if newStatus.ObservedGeneration != tt.box.Generation {
				t.Errorf("Expected observedGeneration %d, got %d", tt.box.Generation, newStatus.ObservedGeneration)
			}

			if newStatus.UpdateRevision == "" {
				t.Errorf("Expected updateRevision to be set")
			}

			if tt.checkConditions != nil {
				tt.checkConditions(t, newStatus)
			}
		})
	}
}

func TestSandboxReconciler_AddSandboxFinalizerAndHash(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name                   string
		sandbox                *agentsv1alpha1.Sandbox
		expectErr              bool
		expectFinalizerAdded   bool
		expectHashAnnotation   bool
		expectPatchCalled      bool
		checkResult            func(t *testing.T, result *agentsv1alpha1.Sandbox, original *agentsv1alpha1.Sandbox)
	}{
		{
			name: "sandbox without finalizer and hash - should add both",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
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
			},
			expectErr:            false,
			expectFinalizerAdded: true,
			expectHashAnnotation: true,
			expectPatchCalled:    true,
			checkResult: func(t *testing.T, result *agentsv1alpha1.Sandbox, original *agentsv1alpha1.Sandbox) {
				if result == nil {
					t.Fatalf("Result sandbox should not be nil")
				}
				// Check finalizer
				hasFinalizerInResult := false
				for _, f := range result.Finalizers {
					if f == utils.SandboxFinalizer {
						hasFinalizerInResult = true
						break
					}
				}
				if !hasFinalizerInResult {
					t.Errorf("Finalizer should be added to result sandbox")
				}
				// Check hash annotation
				if result.Annotations == nil {
					t.Fatalf("Annotations should not be nil")
				}
				if result.Annotations[agentsv1alpha1.SandboxHashWithoutImageAndResources] == "" {
					t.Errorf("Hash annotation should be set")
				}
			},
		},
		{
			name: "sandbox with existing finalizer - should return without patching",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox-with-finalizer",
					Namespace:  "default",
					Finalizers: []string{utils.SandboxFinalizer},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
			},
			expectErr:            false,
			expectFinalizerAdded: false,
			expectHashAnnotation: false,
			expectPatchCalled:    false,
			checkResult: func(t *testing.T, result *agentsv1alpha1.Sandbox, original *agentsv1alpha1.Sandbox) {
				if result == nil {
					t.Fatalf("Result sandbox should not be nil")
				}
				// Result should be the same as original
				if result.Name != original.Name {
					t.Errorf("Result should be the original sandbox")
				}
			},
		},
		{
			name: "sandbox with deletion timestamp - should return without patching",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-sandbox-deleting",
					Namespace:         "default",
					DeletionTimestamp: &metav1.Time{Time: time.Now()},
					Finalizers:        []string{"some-finalizer"}, // Need a finalizer for fake client
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
			},
			expectErr:            false,
			expectFinalizerAdded: false,
			expectHashAnnotation: false,
			expectPatchCalled:    false,
			checkResult: func(t *testing.T, result *agentsv1alpha1.Sandbox, original *agentsv1alpha1.Sandbox) {
				if result == nil {
					t.Fatalf("Result sandbox should not be nil")
				}
				// Result should be the same as original
				if result.Name != original.Name {
					t.Errorf("Result should be the original sandbox")
				}
			},
		},
		{
			name: "sandbox without annotations - should create annotations map",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-sandbox-no-annotations",
					Namespace:   "default",
					Annotations: nil,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
			},
			expectErr:            false,
			expectFinalizerAdded: true,
			expectHashAnnotation: true,
			expectPatchCalled:    true,
			checkResult: func(t *testing.T, result *agentsv1alpha1.Sandbox, original *agentsv1alpha1.Sandbox) {
				if result == nil {
					t.Fatalf("Result sandbox should not be nil")
				}
				// Check annotations map was created
				if result.Annotations == nil {
					t.Errorf("Annotations map should be created")
				}
				// Check hash annotation
				if result.Annotations[agentsv1alpha1.SandboxHashWithoutImageAndResources] == "" {
					t.Errorf("Hash annotation should be set")
				}
			},
		},
		{
			name: "sandbox with existing annotations - should preserve existing annotations",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox-with-annotations",
					Namespace: "default",
					Annotations: map[string]string{
						"existing-key": "existing-value",
					},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
					},
				},
			},
			expectErr:            false,
			expectFinalizerAdded: true,
			expectHashAnnotation: true,
			expectPatchCalled:    true,
			checkResult: func(t *testing.T, result *agentsv1alpha1.Sandbox, original *agentsv1alpha1.Sandbox) {
				if result == nil {
					t.Fatalf("Result sandbox should not be nil")
				}
				// Check existing annotation is preserved
				if result.Annotations["existing-key"] != "existing-value" {
					t.Errorf("Existing annotation should be preserved")
				}
				// Check hash annotation is added
				if result.Annotations[agentsv1alpha1.SandboxHashWithoutImageAndResources] == "" {
					t.Errorf("Hash annotation should be added")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake client with initial objects
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.sandbox).Build()

			reconciler := &SandboxReconciler{
				Client: fakeClient,
				Scheme: scheme,
			}

			originalSandbox := tt.sandbox.DeepCopy()
			ctx := context.Background()

			// Call the method
			result, err := reconciler.addSandboxFinalizerAndHash(ctx, tt.sandbox)

			// Check error expectation
			if tt.expectErr && err == nil {
				t.Errorf("Expected error but got none")
			}
			if !tt.expectErr && err != nil {
				t.Errorf("Expected no error but got: %v", err)
			}

			// Run custom check if provided
			if tt.checkResult != nil {
				tt.checkResult(t, result, originalSandbox)
			}

			// If patch was expected, verify the sandbox in the fake client
			if tt.expectPatchCalled && !tt.expectErr {
				updatedSandbox := &agentsv1alpha1.Sandbox{}
				err := fakeClient.Get(ctx, types.NamespacedName{
					Name:      tt.sandbox.Name,
					Namespace: tt.sandbox.Namespace,
				}, updatedSandbox)
				if err != nil {
					t.Fatalf("Failed to get updated sandbox: %v", err)
				}

				// Verify finalizer in persisted object
				if tt.expectFinalizerAdded {
					hasFinalizer := false
					for _, f := range updatedSandbox.Finalizers {
						if f == utils.SandboxFinalizer {
							hasFinalizer = true
							break
						}
					}
					if !hasFinalizer {
						t.Errorf("Finalizer should be added to persisted sandbox")
					}
				}

				// Verify hash annotation in persisted object
				if tt.expectHashAnnotation {
					if updatedSandbox.Annotations == nil {
						t.Errorf("Annotations should not be nil in persisted sandbox")
					} else if updatedSandbox.Annotations[agentsv1alpha1.SandboxHashWithoutImageAndResources] == "" {
						t.Errorf("Hash annotation should be set in persisted sandbox")
					}
				}
			}
		})
	}
}
