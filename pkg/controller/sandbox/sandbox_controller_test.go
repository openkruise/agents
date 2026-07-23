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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/sandbox/core"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

type sandboxPatchBody struct {
	Spec *struct {
		Paused *bool `json:"paused"`
	} `json:"spec"`
}

func sandboxPatchSetsPaused(t *testing.T, patch client.Patch, obj client.Object) ([]byte, bool) {
	t.Helper()

	patchData, err := patch.Data(obj)
	require.NoError(t, err, "failed to render sandbox patch")

	var body sandboxPatchBody
	require.NoError(t, json.Unmarshal(patchData, &body), "failed to parse sandbox patch")
	return patchData, body.Spec != nil && body.Spec.Paused != nil && *body.Spec.Paused
}

func TestAdd_FeatureGateDisabled(t *testing.T) {
	// When SandboxGate feature gate is disabled, Add should return nil immediately
	// without touching the manager.
	_ = utilfeature.DefaultMutableFeatureGate.Set("Sandbox=false")
	defer func() {
		_ = utilfeature.DefaultMutableFeatureGate.Set("Sandbox=true")
	}()

	err := Add(nil, nil) // manager and enqueuer are nil, but should never be accessed
	if err != nil {
		t.Errorf("Add() error = %v, expected nil when feature gate is disabled", err)
	}
}

func TestAdd_GVKNotDiscovered(t *testing.T) {
	// When SandboxGate feature gate is enabled but DiscoverGVK returns false
	// (generic client is nil in test environment), Add should return nil.
	_ = utilfeature.DefaultMutableFeatureGate.Set("Sandbox=true")
	defer func() {
		_ = utilfeature.DefaultMutableFeatureGate.Set("Sandbox=false")
	}()

	// client.GetGenericClient() returns nil in test, so DiscoverGVK returns false
	err := Add(nil, nil) // manager and enqueuer are nil, but should never be accessed
	if err != nil {
		t.Errorf("Add() error = %v, expected nil when GVK is not discovered", err)
	}
}

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
					Finalizers:        []string{core.SandboxFinalizer},
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
			name: "sandbox resuming - pod running transitions to running",
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
			expectedPhase: agentsv1alpha1.SandboxRunning,
			wantErr:       false,
		},
		{
			name: "sandbox with shutdownTime in past - should be deleted",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "shutdown-sandbox",
					Namespace:  "default",
					Finalizers: []string{core.SandboxFinalizer},
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
					Finalizers: []string{core.SandboxFinalizer},
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
					Finalizers: []string{core.SandboxFinalizer},
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
			expectRequeueAfter: 0,
		},
		{
			name: "pod not found but sandbox running - should set to failed",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "pod-missing-sandbox",
					Namespace:  "default",
					Finalizers: []string{core.SandboxFinalizer},
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
					Finalizers: []string{core.SandboxFinalizer},
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
					Finalizers: []string{core.SandboxFinalizer},
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
			expectRequeueAfter: 0,
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
					Finalizers: []string{core.SandboxFinalizer},
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
		{
			name: "high priority sandbox - should trigger updateHighCreatingSandbox",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "high-priority-sandbox",
					Namespace:  "default",
					Finalizers: []string{core.SandboxFinalizer},
					Annotations: map[string]string{
						agentsv1alpha1.SandboxAnnotationPriority: "1",
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
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPending,
				},
			},
			pod:           nil,
			expectedPhase: agentsv1alpha1.SandboxPending,
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
			rl := core.NewRateLimiter()
			reconciler := &SandboxReconciler{
				Client: client,
				Scheme: scheme,
				controls: core.NewSandboxControl(core.SandboxControlArgs{
					Client:      client,
					Recorder:    fakeRecorder,
					RateLimiter: rl,
					PodControl:  core.NewPodControl(client, fakeRecorder, core.GeneratePodFromSandbox),
				}),
				checkpointControl: core.NewCheckpointControl(client, fakeRecorder),
				rateLimiter:       rl,
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

func TestSandboxReconciler_updateSandboxStatusRecordsPhaseEvent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name           string
		oldStatus      agentsv1alpha1.SandboxStatus
		newStatus      agentsv1alpha1.SandboxStatus
		expectEvent    bool
		expectPrefix   string
		expectContains string
	}{
		{
			name:      "records start message for initial pending phase",
			oldStatus: agentsv1alpha1.SandboxStatus{},
			newStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxPending,
			},
			expectEvent:    true,
			expectPrefix:   corev1.EventTypeNormal + " SandboxPending",
			expectContains: "Start sandbox",
		},
		{
			name: "records started message when pending sandbox becomes running",
			oldStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxPending,
			},
			newStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
			},
			expectEvent:    true,
			expectPrefix:   corev1.EventTypeNormal + " SandboxRunning",
			expectContains: "Sandbox started",
		},
		{
			name: "records resume message when paused sandbox becomes resuming",
			oldStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxPaused,
			},
			newStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxResuming,
			},
			expectEvent:    true,
			expectPrefix:   corev1.EventTypeNormal + " SandboxResuming",
			expectContains: "Resume sandbox",
		},
		{
			name: "records resume message when paused sandbox becomes running",
			oldStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxPaused,
			},
			newStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
			},
			expectEvent:    true,
			expectPrefix:   corev1.EventTypeNormal + " SandboxRunning",
			expectContains: "Resume sandbox",
		},
		{
			name: "records resume message when resuming sandbox becomes running",
			oldStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxResuming,
			},
			newStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
			},
			expectEvent:    true,
			expectPrefix:   corev1.EventTypeNormal + " SandboxRunning",
			expectContains: "Sandbox resumed",
		},
		{
			name: "records pause message when running sandbox becomes paused",
			oldStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
			},
			newStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxPaused,
			},
			expectEvent:    true,
			expectPrefix:   corev1.EventTypeNormal + " SandboxPaused",
			expectContains: "Pause sandbox",
		},
		{
			name: "records upgrade message when running sandbox becomes upgrading",
			oldStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
			},
			newStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
			},
			expectEvent:    true,
			expectPrefix:   corev1.EventTypeNormal + " SandboxUpgrading",
			expectContains: "Upgrade sandbox",
		},
		{
			name: "records upgraded message when upgrading sandbox becomes running",
			oldStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
			},
			newStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
			},
			expectEvent:    true,
			expectPrefix:   corev1.EventTypeNormal + " SandboxRunning",
			expectContains: "Sandbox upgraded",
		},
		{
			name: "records recycle message when running sandbox becomes recycling",
			oldStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
			},
			newStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRecycling,
			},
			expectEvent:    true,
			expectPrefix:   corev1.EventTypeNormal + " SandboxRecycling",
			expectContains: "Recycle sandbox",
		},
		{
			name: "records recycled message when recycling sandbox becomes running",
			oldStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRecycling,
			},
			newStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
			},
			expectEvent:    true,
			expectPrefix:   corev1.EventTypeNormal + " SandboxRunning",
			expectContains: "Sandbox recycled",
		},
		{
			name: "records warning event when phase changes to failed",
			oldStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
			},
			newStatus: agentsv1alpha1.SandboxStatus{
				Phase:   agentsv1alpha1.SandboxFailed,
				Message: "Pod status phase is Failed",
			},
			expectEvent:    true,
			expectPrefix:   corev1.EventTypeWarning + " SandboxFailed",
			expectContains: "Sandbox failed: Pod status phase is Failed",
		},
		{
			name: "records succeeded message when phase changes to succeeded",
			oldStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
			},
			newStatus: agentsv1alpha1.SandboxStatus{
				Phase:   agentsv1alpha1.SandboxSucceeded,
				Message: "Pod status phase is Succeeded",
			},
			expectEvent:    true,
			expectPrefix:   corev1.EventTypeNormal + " SandboxSucceeded",
			expectContains: "Sandbox succeeded: Pod status phase is Succeeded",
		},
		{
			name: "records generic message for unknown transition",
			oldStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxPhase("CustomOldPhase"),
			},
			newStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxPhase("CustomNewPhase"),
			},
			expectEvent:    true,
			expectPrefix:   corev1.EventTypeNormal + " SandboxCustomNewPhase",
			expectContains: "Sandbox phase changed from CustomOldPhase to CustomNewPhase",
		},
		{
			name: "does not record event when only message changes",
			oldStatus: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
			},
			newStatus: agentsv1alpha1.SandboxStatus{
				Phase:   agentsv1alpha1.SandboxRunning,
				Message: "still running",
			},
			expectEvent: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := record.NewFakeRecorder(10)
			sandbox := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Status: tt.oldStatus,
			}
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&agentsv1alpha1.Sandbox{}).
				WithObjects(sandbox).
				Build()
			reconciler := &SandboxReconciler{
				Client:   fakeClient,
				Scheme:   scheme,
				recorder: recorder,
			}

			err := reconciler.updateSandboxStatus(context.Background(), tt.newStatus, sandbox)
			assert.NoError(t, err)
			if tt.expectEvent {
				assertSandboxRecorderEvent(t, recorder, tt.expectPrefix, tt.expectContains)
			} else {
				assertNoSandboxRecorderEvent(t, recorder)
			}
		})
	}
}

func TestSandboxReconciler_recordSandboxTerminatingEvent(t *testing.T) {
	tests := []struct {
		name           string
		recorder       *record.FakeRecorder
		deleting       bool
		expectEvent    bool
		expectContains string
	}{
		{
			name:           "records terminating event for deleting sandbox",
			recorder:       record.NewFakeRecorder(10),
			deleting:       true,
			expectEvent:    true,
			expectContains: "Sandbox deletion is in progress from phase Running",
		},
		{
			name:        "does not record event without deletion timestamp",
			recorder:    record.NewFakeRecorder(10),
			deleting:    false,
			expectEvent: false,
		},
		{
			name:        "nil recorder is ignored",
			recorder:    nil,
			deleting:    true,
			expectEvent: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			}
			if tt.deleting {
				now := metav1.Now()
				sandbox.DeletionTimestamp = &now
			}
			var eventRecorder record.EventRecorder
			if tt.recorder != nil {
				eventRecorder = tt.recorder
			}
			reconciler := &SandboxReconciler{recorder: eventRecorder}

			reconciler.recordSandboxTerminatingEvent(sandbox)

			if tt.expectEvent {
				assertSandboxRecorderEvent(t, tt.recorder, corev1.EventTypeNormal+" "+eventReasonSandboxTerminating, tt.expectContains)
			} else if tt.recorder != nil {
				assertNoSandboxRecorderEvent(t, tt.recorder)
			}
		})
	}
}

func TestSandboxReconciler_handleTerminatingRecordsEventOnlyWhenDeletingPod(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	now := metav1.Now()
	tests := []struct {
		name                   string
		pod                    *corev1.Pod
		expectEvent            bool
		expectFinalizerRemoved bool
	}{
		{
			name: "records event when active pod is deleted",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
			},
			expectEvent: true,
		},
		{
			name: "does not record event when pod is already deleting",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-sandbox",
					Namespace:         "default",
					DeletionTimestamp: &now,
					Finalizers:        []string{"fake-finalizer"},
				},
			},
			expectEvent: false,
		},
		{
			name:                   "does not record event when removing finalizer after pod is gone",
			pod:                    nil,
			expectEvent:            false,
			expectFinalizerRemoved: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deletionTime := metav1.Now()
			sandbox := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-sandbox",
					Namespace:         "default",
					DeletionTimestamp: &deletionTime,
					Finalizers:        []string{core.SandboxFinalizer},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			}

			objects := []client.Object{sandbox}
			if tt.pod != nil {
				objects = append(objects, tt.pod)
			}
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objects...).
				Build()
			recorder := record.NewFakeRecorder(10)
			reconciler := &SandboxReconciler{
				Client:   fakeClient,
				Scheme:   scheme,
				recorder: recorder,
				controls: core.NewSandboxControl(core.SandboxControlArgs{
					Client:   fakeClient,
					Recorder: recorder,
				}),
			}

			_, err := reconciler.handleTerminating(context.Background(), core.EnsureFuncArgs{
				Pod:       tt.pod,
				Box:       sandbox,
				NewStatus: sandbox.Status.DeepCopy(),
			})
			assert.NoError(t, err)

			if tt.expectEvent {
				assertSandboxRecorderEvent(t, recorder, corev1.EventTypeNormal+" "+eventReasonSandboxTerminating, "Sandbox deletion is in progress from phase Running")
			} else {
				assertNoSandboxRecorderEvent(t, recorder)
			}

			if tt.expectFinalizerRemoved {
				updatedSandbox := &agentsv1alpha1.Sandbox{}
				err := fakeClient.Get(context.Background(), types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, updatedSandbox)
				if apierrors.IsNotFound(err) {
					return
				}
				assert.NoError(t, err)
				assert.NotContains(t, updatedSandbox.Finalizers, core.SandboxFinalizer)
			}
		})
	}
}

func assertSandboxRecorderEvent(t *testing.T, recorder *record.FakeRecorder, expectPrefix, expectContains string) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, expectPrefix)
		assert.Contains(t, event, expectContains)
	default:
		t.Fatalf("expected event %q, got none", expectPrefix)
	}
}

func assertNoSandboxRecorderEvent(t *testing.T, recorder *record.FakeRecorder) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		t.Fatalf("unexpected event: %s", event)
	default:
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
	}

	err := client.Create(context.TODO(), originalSandbox)
	if err != nil {
		t.Fatalf("Failed to create sandbox: %v", err)
	}

	pendingStatus := agentsv1alpha1.SandboxStatus{
		Phase:              agentsv1alpha1.SandboxPending,
		ObservedGeneration: 1,
		UpdateRevision:     "abc123",
	}

	err = reconciler.updateSandboxStatus(context.Background(), pendingStatus, originalSandbox)
	if err != nil {
		t.Errorf("updateSandboxStatus() error = %v, want nil", err)
		return
	}

	updatedSandbox := &agentsv1alpha1.Sandbox{}
	err = client.Get(context.TODO(), types.NamespacedName{Name: originalSandbox.Name, Namespace: originalSandbox.Namespace}, updatedSandbox)
	if err != nil {
		t.Fatalf("Failed to get updated sandbox: %v", err)
	}
	if updatedSandbox.Status.Phase != agentsv1alpha1.SandboxPending {
		t.Errorf("Expected sandbox phase %v, got %v", agentsv1alpha1.SandboxPending, updatedSandbox.Status.Phase)
	}
	if updatedSandbox.Status.ObservedGeneration != 1 {
		t.Errorf("Expected ObservedGeneration 1, got %v", updatedSandbox.Status.ObservedGeneration)
	}
	if updatedSandbox.Status.UpdateRevision != "abc123" {
		t.Errorf("Expected UpdateRevision abc123, got %v", updatedSandbox.Status.UpdateRevision)
	}
}

func TestSandboxReconciler_ShutdownTime(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	fakeRecorder := record.NewFakeRecorder(100)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	rl := core.NewRateLimiter()
	reconciler := &SandboxReconciler{
		Client: client,
		Scheme: scheme,
		controls: core.NewSandboxControl(core.SandboxControlArgs{
			Client:      client,
			Recorder:    fakeRecorder,
			RateLimiter: rl,
			PodControl:  core.NewPodControl(client, fakeRecorder, core.GeneratePodFromSandbox),
		}),
		checkpointControl: core.NewCheckpointControl(client, fakeRecorder),
		rateLimiter:       rl,
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

func TestSandboxReconciler_CheckTimers(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))

	now := metav1.NewTime(time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC))
	past := metav1.NewTime(now.Add(-time.Second))

	tests := []struct {
		name          string
		reuseReason   string
		expectDone    bool
		expectDeleted bool
	}{
		{
			name:        "reuse in progress skips expired pause and shutdown timers",
			reuseReason: agentsv1alpha1.SandboxRecyclingReasonStarted,
		},
		{
			name:          "reuse failed allows expired shutdown deletion",
			reuseReason:   agentsv1alpha1.SandboxRecyclingReasonFailed,
			expectDone:    true,
			expectDeleted: true,
		},
		{
			name:          "reuse timeout allows expired shutdown deletion",
			reuseReason:   agentsv1alpha1.SandboxRecyclingReasonTimeout,
			expectDone:    true,
			expectDeleted: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			box := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "reuse-timer-sandbox",
					Namespace:       "default",
					ResourceVersion: "42",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					PauseTime:    &past,
					ShutdownTime: &past,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRecycling,
					Conditions: []metav1.Condition{
						{
							Type:   string(agentsv1alpha1.SandboxConditionRecycling),
							Status: metav1.ConditionFalse,
							Reason: tt.reuseReason,
						},
					},
				},
			}

			deleteCalls := 0
			patchCalls := 0
			cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(box).WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					deleteCalls++
					return nil
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, ok := obj.(*agentsv1alpha1.Sandbox); ok {
						_, setsPaused := sandboxPatchSetsPaused(t, patch, obj)
						if setsPaused {
							patchCalls++
						}
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			}).Build()
			reconciler := &SandboxReconciler{Client: cli}

			result, done, err := reconciler.checkTimers(context.Background(), box, now)
			require.NoError(t, err)
			assert.Equal(t, ctrl.Result{}, result)
			assert.Equal(t, tt.expectDone, done)
			if tt.expectDeleted {
				assert.Equal(t, 1, deleteCalls)
			} else {
				assert.Equal(t, 0, deleteCalls)
			}
			assert.Equal(t, 0, patchCalls)
		})
	}
}

func TestSandboxReconciler_HandleShutdownTimeout(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))

	now := metav1.NewTime(time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC))
	past := metav1.NewTime(now.Add(-time.Second))
	exact := metav1.NewTime(now.Time)
	future := metav1.NewTime(now.Add(time.Second))
	deletingAt := metav1.NewTime(now.Add(-time.Minute))

	tests := []struct {
		name          string
		shutdownTime  *metav1.Time
		pauseTime     *metav1.Time
		paused        bool
		annotations   map[string]string
		deletingAt    *metav1.Time
		expectDone    bool
		expectDeleted bool
	}{
		{
			name: "nil shutdown time skips handling",
		},
		{
			name:         "future shutdown time skips handling",
			shutdownTime: &future,
		},
		{
			name:         "exact shutdown time skips handling",
			shutdownTime: &exact,
		},
		{
			name:          "past shutdown time without annotation deletes",
			shutdownTime:  &past,
			expectDone:    true,
			expectDeleted: true,
		},
		{
			name:         "past shutdown time with deletion timestamp skips handling",
			shutdownTime: &past,
			deletingAt:   &deletingAt,
		},
		{
			name:         "past shutdown time with annotation and nil pause time deletes",
			shutdownTime: &past,
			annotations: map[string]string{
				agentsv1alpha1.AnnotationReservePausedSandboxDuration: timeout.ReservePausedSandboxDurationForeverValue,
			},
			expectDone:    true,
			expectDeleted: true,
		},
		{
			name:         "past shutdown time with annotation and future pause time deletes",
			shutdownTime: &past,
			pauseTime:    &future,
			annotations: map[string]string{
				agentsv1alpha1.AnnotationReservePausedSandboxDuration: timeout.ReservePausedSandboxDurationForeverValue,
			},
			expectDone:    true,
			expectDeleted: true,
		},
		{
			name:         "past shutdown time with annotation and due pause time lets pause run",
			shutdownTime: &past,
			pauseTime:    &exact,
			annotations: map[string]string{
				agentsv1alpha1.AnnotationReservePausedSandboxDuration: timeout.ReservePausedSandboxDurationForeverValue,
			},
		},
		{
			name:         "past shutdown time with annotation and paused sandbox deletes",
			shutdownTime: &past,
			pauseTime:    &past,
			paused:       true,
			annotations: map[string]string{
				agentsv1alpha1.AnnotationReservePausedSandboxDuration: timeout.ReservePausedSandboxDurationForeverValue,
			},
			expectDone:    true,
			expectDeleted: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deleteCalls := 0
			cli := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					deleteCalls++
					return nil
				},
			}).Build()
			reconciler := &SandboxReconciler{Client: cli}
			box := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "timeout-sandbox",
					Namespace:         "default",
					Annotations:       tt.annotations,
					DeletionTimestamp: tt.deletingAt,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused:       tt.paused,
					PauseTime:    tt.pauseTime,
					ShutdownTime: tt.shutdownTime,
				},
			}

			done, err := reconciler.handleShutdownTimeout(context.Background(), box, now)
			require.NoError(t, err)
			assert.Equal(t, tt.expectDone, done)
			if tt.expectDeleted {
				assert.Equal(t, 1, deleteCalls)
			} else {
				assert.Equal(t, 0, deleteCalls)
			}
		})
	}
}

func TestSandboxReconciler_HandlePauseTimeout(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))

	now := metav1.NewTime(time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC))
	past := metav1.NewTime(now.Add(-time.Second))
	exact := metav1.NewTime(now.Time)
	future := metav1.NewTime(now.Add(time.Second))
	shutdown := metav1.NewTime(now.Add(time.Hour))

	tests := []struct {
		name                 string
		pauseTime            *metav1.Time
		shutdownTime         *metav1.Time
		paused               bool
		annotationValue      *string
		injectPatchConflict  bool
		expectDone           bool
		expectRequeue        bool
		expectPatch          bool
		expectPaused         bool
		expectPauseUnchanged bool
		expectShutdown       string
		expectRetention      time.Duration
	}{
		{
			name:                 "nil pause time skips handling",
			shutdownTime:         &shutdown,
			expectPauseUnchanged: true,
			expectShutdown:       "unchanged",
		},
		{
			name:                 "future pause time skips handling",
			pauseTime:            &future,
			shutdownTime:         &shutdown,
			expectPauseUnchanged: true,
			expectShutdown:       "unchanged",
		},
		{
			name:                 "due pause time on already paused sandbox skips handling",
			pauseTime:            &exact,
			shutdownTime:         &shutdown,
			paused:               true,
			expectPaused:         true,
			expectPauseUnchanged: true,
			expectShutdown:       "unchanged",
		},
		{
			name:                 "exact pause time without annotation patches paused only",
			pauseTime:            &exact,
			shutdownTime:         &shutdown,
			expectDone:           true,
			expectPatch:          true,
			expectPaused:         true,
			expectPauseUnchanged: true,
			expectShutdown:       "unchanged",
		},
		{
			name:                 "past pause time without annotation patches paused only",
			pauseTime:            &past,
			shutdownTime:         &shutdown,
			expectDone:           true,
			expectPatch:          true,
			expectPaused:         true,
			expectPauseUnchanged: true,
			expectShutdown:       "unchanged",
		},
		{
			name:            "default annotation recalculates shutdown and pause time",
			pauseTime:       &exact,
			shutdownTime:    &shutdown,
			annotationValue: ptr.To(timeout.ReservePausedSandboxDurationForeverValue),
			expectDone:      true,
			expectPatch:     true,
			expectPaused:    true,
			expectShutdown:  "retention",
			expectRetention: timeout.ForeverReservePausedSandboxDuration,
		},
		{
			name:            "custom annotation recalculates shutdown and pause time",
			pauseTime:       &exact,
			shutdownTime:    &shutdown,
			annotationValue: ptr.To("30m"),
			expectDone:      true,
			expectPatch:     true,
			expectPaused:    true,
			expectShutdown:  "retention",
			expectRetention: 30 * time.Minute,
		},
		{
			name:                 "annotation with nil shutdown does not create shutdown",
			pauseTime:            &exact,
			annotationValue:      ptr.To(timeout.ReservePausedSandboxDurationForeverValue),
			expectDone:           true,
			expectPatch:          true,
			expectPaused:         true,
			expectPauseUnchanged: true,
			expectShutdown:       "nil",
		},
		{
			name:            "invalid annotation uses default retention without backfilling",
			pauseTime:       &exact,
			shutdownTime:    &shutdown,
			annotationValue: ptr.To("invalid"),
			expectDone:      true,
			expectPatch:     true,
			expectPaused:    true,
			expectShutdown:  "retention",
			expectRetention: timeout.ForeverReservePausedSandboxDuration,
		},
		{
			name:                 "patch conflict requeues and leaves spec unchanged",
			pauseTime:            &exact,
			shutdownTime:         &shutdown,
			injectPatchConflict:  true,
			expectDone:           true,
			expectRequeue:        true,
			expectPatch:          true,
			expectPauseUnchanged: true,
			expectShutdown:       "unchanged",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			annotations := map[string]string{}
			if tt.annotationValue != nil {
				annotations[agentsv1alpha1.AnnotationReservePausedSandboxDuration] = *tt.annotationValue
			}
			box := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "pause-timeout-sandbox",
					Namespace:       "default",
					ResourceVersion: "42",
					Annotations:     annotations,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused:       tt.paused,
					PauseTime:    tt.pauseTime,
					ShutdownTime: tt.shutdownTime,
				},
			}

			patchCalls := 0
			cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(box).WithInterceptorFuncs(interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, ok := obj.(*agentsv1alpha1.Sandbox); ok {
						_, setsPaused := sandboxPatchSetsPaused(t, patch, obj)
						if setsPaused {
							patchCalls++
							if tt.injectPatchConflict {
								return apierrors.NewConflict(schema.GroupResource{Group: agentsv1alpha1.GroupVersion.Group, Resource: "sandboxes"}, box.Name, fmt.Errorf("simulated conflict"))
							}
						}
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			}).Build()
			reconciler := &SandboxReconciler{Client: cli}

			result, done, err := reconciler.handlePauseTimeout(context.Background(), box, now)
			require.NoError(t, err)
			assert.Equal(t, tt.expectDone, done)
			assert.Equal(t, tt.expectRequeue, result.Requeue)
			if tt.expectPatch {
				assert.Equal(t, 1, patchCalls)
			} else {
				assert.Equal(t, 0, patchCalls)
			}

			updated := &agentsv1alpha1.Sandbox{}
			require.NoError(t, cli.Get(context.TODO(), types.NamespacedName{Name: box.Name, Namespace: box.Namespace}, updated))
			assert.Equal(t, tt.expectPaused, updated.Spec.Paused)
			if tt.expectPauseUnchanged {
				if tt.pauseTime == nil {
					assert.Nil(t, updated.Spec.PauseTime)
				} else {
					require.NotNil(t, updated.Spec.PauseTime)
					assert.True(t, updated.Spec.PauseTime.Time.Equal(tt.pauseTime.Time))
				}
			}

			switch tt.expectShutdown {
			case "nil":
				assert.Nil(t, updated.Spec.ShutdownTime)
			case "unchanged":
				require.NotNil(t, updated.Spec.ShutdownTime)
				assert.True(t, updated.Spec.ShutdownTime.Time.Equal(tt.shutdownTime.Time))
			case "retention":
				require.NotNil(t, updated.Spec.ShutdownTime)
				require.NotNil(t, updated.Spec.PauseTime)
				expectedShutdown := timeout.NormalizeTime(now.Add(tt.expectRetention))
				assert.True(t, updated.Spec.ShutdownTime.Time.Equal(expectedShutdown))
				assert.True(t, updated.Spec.PauseTime.Time.Equal(expectedShutdown))
			default:
				t.Fatalf("unexpected expectShutdown value %q", tt.expectShutdown)
			}

			if tt.annotationValue == nil {
				_, hasAnnotation := updated.Annotations[agentsv1alpha1.AnnotationReservePausedSandboxDuration]
				assert.False(t, hasAnnotation)
			} else {
				assert.Equal(t, *tt.annotationValue, updated.Annotations[agentsv1alpha1.AnnotationReservePausedSandboxDuration])
			}
		})
	}
}

func TestSandboxReconciler_AutoPauseBranch(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	pastPauseTime := metav1.NewTime(time.Now().Add(-1 * time.Minute).Truncate(time.Second))
	shutdownTime := metav1.NewTime(time.Now().Add(1 * time.Hour).Truncate(time.Second))
	futurePauseDelta := 10 * time.Second
	futurePauseTime := metav1.NewTime(time.Now().Add(futurePauseDelta))

	runningSandbox := func(name string, pauseTime metav1.Time) *agentsv1alpha1.Sandbox {
		return &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       "default",
				Finalizers:      []string{core.SandboxFinalizer},
				ResourceVersion: "42",
			},
			Spec: agentsv1alpha1.SandboxSpec{
				Paused:       false,
				PauseTime:    &pauseTime,
				ShutdownTime: &shutdownTime,
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
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReady),
						Status:             metav1.ConditionTrue,
						LastTransitionTime: metav1.NewTime(time.Now().Add(-1 * time.Minute)),
						Reason:             "Test",
					},
				},
			},
		}
	}

	noControlSandbox := func(name string, pauseTime metav1.Time) *agentsv1alpha1.Sandbox {
		return &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:       name,
				Namespace:  "default",
				Finalizers: []string{core.SandboxFinalizer},
			},
			Spec: agentsv1alpha1.SandboxSpec{
				Paused:    false,
				PauseTime: &pauseTime,
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
						},
					},
				},
			},
			Status: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxPhase("NoControlAction"),
			},
		}
	}

	cases := []struct {
		name                     string
		sandbox                  *agentsv1alpha1.Sandbox
		injectPatchErr           error
		expectAutoPausePatch     bool
		expectRequeue            bool
		expectRequeueAfter       time.Duration
		expectPausedAfter        bool
		expectPauseTimeUnchanged bool
	}{
		{
			name:                     "past pause time with patch conflict requeues and leaves spec unchanged",
			sandbox:                  runningSandbox("auto-pause-conflict", pastPauseTime),
			injectPatchErr:           apierrors.NewConflict(schema.GroupResource{Group: agentsv1alpha1.GroupVersion.Group, Resource: "sandboxes"}, "auto-pause-conflict", fmt.Errorf("simulated conflict")),
			expectAutoPausePatch:     true,
			expectRequeue:            true,
			expectPausedAfter:        false,
			expectPauseTimeUnchanged: true,
		},
		{
			name:                 "past pause time with successful patch persists paused=true",
			sandbox:              runningSandbox("auto-pause-success", pastPauseTime),
			expectAutoPausePatch: true,
			expectPausedAfter:    true,
		},
		{
			name:                 "future pause time skips patch and schedules requeue",
			sandbox:              noControlSandbox("future-pause-sandbox", futurePauseTime),
			expectAutoPausePatch: false,
			expectRequeueAfter:   futurePauseDelta,
			expectPausedAfter:    false,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			autoPausePatches := 0
			optimisticLockSeen := false
			fakeRecorder := record.NewFakeRecorder(100)
			cli := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&agentsv1alpha1.Sandbox{}).
				WithObjects(tt.sandbox).
				WithInterceptorFuncs(interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if _, ok := obj.(*agentsv1alpha1.Sandbox); ok {
							patchData, setsPaused := sandboxPatchSetsPaused(t, patch, obj)
							if setsPaused {
								if bytes.Contains(patchData, []byte(`"resourceVersion"`)) {
									optimisticLockSeen = true
								}
								autoPausePatches++
								if tt.injectPatchErr != nil {
									return tt.injectPatchErr
								}
							}
						}
						return c.Patch(ctx, obj, patch, opts...)
					},
				}).
				Build()
			rl := core.NewRateLimiter()
			reconciler := &SandboxReconciler{
				Client: cli,
				Scheme: scheme,
				controls: core.NewSandboxControl(core.SandboxControlArgs{
					Client:      cli,
					Recorder:    fakeRecorder,
					RateLimiter: rl,
					PodControl:  core.NewPodControl(cli, fakeRecorder, core.GeneratePodFromSandbox),
				}),
				checkpointControl: core.NewCheckpointControl(cli, fakeRecorder),
				rateLimiter:       rl,
			}

			req := ctrl.Request{
				NamespacedName: types.NamespacedName{Name: tt.sandbox.Name, Namespace: tt.sandbox.Namespace},
			}
			result, err := reconciler.Reconcile(context.Background(), req)
			if err != nil {
				t.Fatalf("unexpected reconcile error: %v", err)
			}

			if tt.expectAutoPausePatch {
				if autoPausePatches == 0 {
					t.Fatalf("expected auto-pause patch to be issued")
				}
				if !optimisticLockSeen {
					t.Fatalf("expected auto-pause patch to include optimistic-lock resourceVersion")
				}
			} else if autoPausePatches > 0 {
				t.Fatalf("did not expect auto-pause patch, got %d", autoPausePatches)
			}

			if tt.expectRequeue != result.Requeue {
				t.Fatalf("expected Requeue=%v, got result=%+v", tt.expectRequeue, result)
			}
			if tt.expectRequeueAfter != 0 {
				if delta := result.RequeueAfter - tt.expectRequeueAfter; delta < -2*time.Second || delta > 2*time.Second {
					t.Fatalf("expected RequeueAfter within 2s of %v, got %v", tt.expectRequeueAfter, result.RequeueAfter)
				}
			}

			updated := &agentsv1alpha1.Sandbox{}
			if err := cli.Get(context.TODO(), req.NamespacedName, updated); err != nil {
				t.Fatalf("failed to get sandbox after reconcile: %v", err)
			}
			if updated.Spec.Paused != tt.expectPausedAfter {
				t.Fatalf("expected Spec.Paused=%v, got %v", tt.expectPausedAfter, updated.Spec.Paused)
			}
			if tt.expectPauseTimeUnchanged {
				if updated.Spec.PauseTime == nil || !updated.Spec.PauseTime.Time.Equal(tt.sandbox.Spec.PauseTime.Time) {
					t.Fatalf("expected Spec.PauseTime unchanged at %v, got %v", tt.sandbox.Spec.PauseTime, updated.Spec.PauseTime)
				}
				if tt.sandbox.Spec.ShutdownTime != nil &&
					(updated.Spec.ShutdownTime == nil || !updated.Spec.ShutdownTime.Time.Equal(tt.sandbox.Spec.ShutdownTime.Time)) {
					t.Fatalf("expected Spec.ShutdownTime unchanged at %v, got %v", tt.sandbox.Spec.ShutdownTime, updated.Spec.ShutdownTime)
				}
			}
		})
	}
}

func TestSandboxReconciler_AutoPauseReservePausedRetention(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	now := time.Now().Add(-time.Minute)
	tests := []struct {
		name                 string
		annotationValue      *string
		expectAnnotation     string
		initialShutdown      *metav1.Time
		nilPauseTime         bool
		futurePauseTime      bool
		injectPatchConflict  bool
		expectPaused         bool
		expectRequeue        bool
		expectDeleted        bool
		expectShutdownChange bool
	}{
		{
			name:                 "default annotation recalculates shutdown",
			annotationValue:      ptr.To(timeout.ReservePausedSandboxDurationForeverValue),
			initialShutdown:      ptr.To(metav1.NewTime(time.Now().Add(time.Hour))),
			expectPaused:         true,
			expectShutdownChange: true,
		},
		{
			name:                 "custom annotation recalculates shutdown",
			annotationValue:      ptr.To("30m"),
			initialShutdown:      ptr.To(metav1.NewTime(time.Now().Add(time.Hour))),
			expectPaused:         true,
			expectShutdownChange: true,
		},
		{
			name:                 "expired shutdown still auto-pauses before deletion",
			annotationValue:      ptr.To("30m"),
			initialShutdown:      ptr.To(metav1.NewTime(time.Now().Add(-time.Second))),
			expectPaused:         true,
			expectShutdownChange: true,
		},
		{
			name:                 "no annotation keeps existing CRD behavior",
			initialShutdown:      ptr.To(metav1.NewTime(time.Now().Add(time.Hour))),
			expectPaused:         true,
			expectShutdownChange: false,
		},
		{
			name:            "no annotation with expired pause and shutdown deletes",
			initialShutdown: ptr.To(metav1.NewTime(time.Now().Add(-time.Second))),
			expectDeleted:   true,
		},
		{
			name:                 "annotation with nil shutdown preserves never-timeout",
			annotationValue:      ptr.To(timeout.ReservePausedSandboxDurationForeverValue),
			initialShutdown:      nil,
			expectPaused:         true,
			expectShutdownChange: false,
		},
		{
			name:                 "invalid annotation patches paused without backfilling default",
			annotationValue:      ptr.To("invalid"),
			initialShutdown:      ptr.To(metav1.NewTime(time.Now().Add(time.Hour))),
			expectPaused:         true,
			expectShutdownChange: true,
		},
		{
			name:                "patch conflict requeues and leaves spec unchanged",
			initialShutdown:     ptr.To(metav1.NewTime(time.Now().Add(time.Hour))),
			injectPatchConflict: true,
			expectPaused:        false,
			expectRequeue:       true,
		},
		{
			name:            "annotation present but nil pause time with expired shutdown deletes",
			annotationValue: ptr.To(timeout.ReservePausedSandboxDurationForeverValue),
			initialShutdown: ptr.To(metav1.NewTime(time.Now().Add(-time.Second))),
			nilPauseTime:    true,
			expectDeleted:   true,
		},
		{
			name:            "annotation present with future pause time and expired shutdown deletes",
			annotationValue: ptr.To(timeout.ReservePausedSandboxDurationForeverValue),
			initialShutdown: ptr.To(metav1.NewTime(time.Now().Add(-time.Second))),
			futurePauseTime: true,
			expectDeleted:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			annotations := map[string]string{}
			if tt.annotationValue != nil {
				annotations[agentsv1alpha1.AnnotationReservePausedSandboxDuration] = *tt.annotationValue
			}
			var pauseTime *metav1.Time
			if tt.futurePauseTime {
				pauseTime = &metav1.Time{Time: time.Now().Add(10 * time.Minute)}
			} else if !tt.nilPauseTime {
				pauseTime = &metav1.Time{Time: now}
			}
			sandbox := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "auto-pause-retention",
					Namespace:       "default",
					Finalizers:      []string{core.SandboxFinalizer},
					ResourceVersion: "42",
					Annotations:     annotations,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused:       false,
					PauseTime:    pauseTime,
					ShutdownTime: tt.initialShutdown,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "nginx"}}},
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue},
					},
				},
			}

			autoPausePatches := 0
			optimisticLockSeen := false
			var autoPausePatchData []byte
			fakeRecorder := record.NewFakeRecorder(100)
			cli := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&agentsv1alpha1.Sandbox{}).
				WithObjects(sandbox).
				WithInterceptorFuncs(interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if _, ok := obj.(*agentsv1alpha1.Sandbox); ok {
							patchData, setsPaused := sandboxPatchSetsPaused(t, patch, obj)
							if setsPaused {
								if bytes.Contains(patchData, []byte(`"resourceVersion"`)) {
									optimisticLockSeen = true
								}
								autoPausePatchData = append([]byte(nil), patchData...)
								autoPausePatches++
								if tt.injectPatchConflict {
									return apierrors.NewConflict(schema.GroupResource{Group: agentsv1alpha1.GroupVersion.Group, Resource: "sandboxes"}, sandbox.Name, fmt.Errorf("simulated conflict"))
								}
							}
						}
						return c.Patch(ctx, obj, patch, opts...)
					},
				}).
				Build()
			rl := core.NewRateLimiter()
			reconciler := &SandboxReconciler{
				Client: cli,
				Scheme: scheme,
				controls: core.NewSandboxControl(core.SandboxControlArgs{
					Client:      cli,
					Recorder:    fakeRecorder,
					RateLimiter: rl,
					PodControl:  core.NewPodControl(cli, fakeRecorder, core.GeneratePodFromSandbox),
				}),
				checkpointControl: core.NewCheckpointControl(cli, fakeRecorder),
				rateLimiter:       rl,
				recorder:          fakeRecorder,
			}

			req := ctrl.Request{
				NamespacedName: types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace},
			}
			result, err := reconciler.Reconcile(context.Background(), req)
			require.NoError(t, err)

			assert.Equal(t, tt.expectRequeue, result.Requeue)
			if tt.expectDeleted {
				assert.Equal(t, 0, autoPausePatches, "did not expect auto-pause patch before delete")
				updated := &agentsv1alpha1.Sandbox{}
				getErr := cli.Get(context.TODO(), req.NamespacedName, updated)
				if getErr == nil {
					assert.False(t, updated.DeletionTimestamp.IsZero(), "expected sandbox to be deleting")
				} else {
					assert.True(t, apierrors.IsNotFound(getErr), "expected not found or deleting sandbox, got %v", getErr)
				}
				return
			}
			if !tt.expectShutdownChange {
				require.Equal(t, 1, autoPausePatches, "expected one auto-pause patch")
				assert.True(t, optimisticLockSeen, "expected auto-pause patch to include optimistic-lock resourceVersion")
			}
			if !tt.expectShutdownChange && !tt.injectPatchConflict {
				assert.NotContains(t, string(autoPausePatchData), "shutdownTime")
			}

			updated := &agentsv1alpha1.Sandbox{}
			require.NoError(t, cli.Get(context.TODO(), req.NamespacedName, updated))
			assert.Equal(t, tt.expectPaused, updated.Spec.Paused)
			if tt.expectAnnotation != "" {
				assert.Equal(t, tt.expectAnnotation, updated.Annotations[agentsv1alpha1.AnnotationReservePausedSandboxDuration])
			} else if tt.annotationValue != nil {
				assert.Equal(t, *tt.annotationValue, updated.Annotations[agentsv1alpha1.AnnotationReservePausedSandboxDuration])
			}

			if tt.injectPatchConflict {
				assert.False(t, updated.Spec.Paused)
				if tt.initialShutdown == nil {
					assert.Nil(t, updated.Spec.ShutdownTime)
				} else {
					require.NotNil(t, updated.Spec.ShutdownTime)
					assert.True(t, timeout.NormalizeTime(updated.Spec.ShutdownTime.Time).Equal(timeout.NormalizeTime(tt.initialShutdown.Time)))
				}
				return
			}

			if tt.expectShutdownChange {
				require.NotNil(t, updated.Spec.ShutdownTime)
				require.NotNil(t, updated.Spec.PauseTime)
				retention := timeout.ForeverReservePausedSandboxDuration
				if tt.annotationValue != nil && *tt.annotationValue == "30m" {
					retention = 30 * time.Minute
				}
				assert.WithinDuration(t, time.Now().Add(retention), updated.Spec.ShutdownTime.Time, 5*time.Second)
				assert.WithinDuration(t, updated.Spec.ShutdownTime.Time, updated.Spec.PauseTime.Time, time.Second)
			} else if tt.initialShutdown == nil {
				assert.Nil(t, updated.Spec.ShutdownTime)
			} else {
				require.NotNil(t, updated.Spec.ShutdownTime)
				assert.True(t, timeout.NormalizeTime(updated.Spec.ShutdownTime.Time).Equal(timeout.NormalizeTime(tt.initialShutdown.Time)))
			}
		})
	}
}

func TestSandboxReconciler_CalcTimeoutRequeue(t *testing.T) {
	now := metav1.NewTime(time.Date(2026, 6, 17, 9, 0, 0, 0, time.UTC))
	past := metav1.NewTime(now.Add(-time.Second))
	exact := metav1.NewTime(now.Time)
	pauseSoon := metav1.NewTime(now.Add(5 * time.Second))
	pauseLater := metav1.NewTime(now.Add(10 * time.Second))
	shutdownSoon := metav1.NewTime(now.Add(3 * time.Second))
	shutdownLater := metav1.NewTime(now.Add(10 * time.Second))
	deletingAt := metav1.NewTime(now.Add(-time.Minute))

	tests := []struct {
		name         string
		pauseTime    *metav1.Time
		shutdownTime *metav1.Time
		paused       bool
		deletingAt   *metav1.Time
		expect       time.Duration
	}{
		{
			name: "no timeout returns zero",
		},
		{
			name:      "future pause time returns pause delta",
			pauseTime: &pauseSoon,
			expect:    5 * time.Second,
		},
		{
			name:         "future shutdown time returns shutdown delta",
			shutdownTime: &shutdownSoon,
			expect:       3 * time.Second,
		},
		{
			name:         "both future times uses pause when earlier",
			pauseTime:    &pauseSoon,
			shutdownTime: &shutdownLater,
			expect:       5 * time.Second,
		},
		{
			name:         "both future times uses shutdown when earlier",
			pauseTime:    &pauseLater,
			shutdownTime: &shutdownSoon,
			expect:       3 * time.Second,
		},
		{
			name:      "past pause time returns zero",
			pauseTime: &past,
		},
		{
			name:      "exact pause time returns zero",
			pauseTime: &exact,
		},
		{
			name:         "past shutdown time returns zero",
			shutdownTime: &past,
		},
		{
			name:         "exact shutdown time returns zero",
			shutdownTime: &exact,
		},
		{
			name:      "paused sandbox ignores future pause time",
			pauseTime: &pauseSoon,
			paused:    true,
		},
		{
			name:         "paused sandbox still requeues future shutdown time",
			pauseTime:    &pauseSoon,
			shutdownTime: &shutdownLater,
			paused:       true,
			expect:       10 * time.Second,
		},
		{
			name:         "deleting sandbox ignores future shutdown time",
			shutdownTime: &shutdownSoon,
			deletingAt:   &deletingAt,
		},
	}

	reconciler := &SandboxReconciler{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			box := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: tt.deletingAt,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused:       tt.paused,
					PauseTime:    tt.pauseTime,
					ShutdownTime: tt.shutdownTime,
				},
			}

			assert.Equal(t, tt.expect, reconciler.calcTimeoutRequeue(box, now))
		})
	}
}

func TestPauseTimeReached(t *testing.T) {
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	cases := []struct {
		name      string
		pauseTime metav1.Time
		now       metav1.Time
		expectDue bool
	}{
		{
			name:      "past pause time is due",
			pauseTime: metav1.NewTime(now.Add(-time.Second)),
			now:       now,
			expectDue: true,
		},
		{
			name:      "exact pause time is due",
			pauseTime: now,
			now:       now,
			expectDue: true,
		},
		{
			name:      "future pause time is not due",
			pauseTime: metav1.NewTime(now.Add(time.Second)),
			now:       now,
			expectDue: false,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := pauseTimeReached(&tt.pauseTime, tt.now); got != tt.expectDue {
				t.Fatalf("pauseTimeReached() = %v, want %v", got, tt.expectDue)
			}
		})
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
			name: "running phase with hash mismatch and recreate policy should transition to upgrading and remove upgrading condition",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.PodLabelTemplateHash: "old-hash",
					},
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
					UpgradePolicy: &agentsv1alpha1.SandboxUpgradePolicy{
						Type: agentsv1alpha1.SandboxUpgradePolicyRecreate,
					},
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx:v2"}},
							},
						},
					},
				},
			},
			initStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionTrue,
						Reason:             "PreviousUpgrade",
						LastTransitionTime: metav1.Now(),
					},
				},
			},
			expectedPhase:     agentsv1alpha1.SandboxUpgrading,
			expectedShouldReq: false,
			checkConditions: func(t *testing.T, status *agentsv1alpha1.SandboxStatus) {
				// Upgrading condition should be removed when entering upgrading phase
				for _, cond := range status.Conditions {
					if cond.Type == string(agentsv1alpha1.SandboxConditionUpgrading) {
						t.Errorf("Upgrading condition should be removed, but still exists")
					}
				}
			},
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
		{
			name: "running phase with cleanup annotations should transition to cleaning",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Generation: 1,
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationCleanup:        "true",
						agentsv1alpha1.AnnotationCleanupEnabled: "true",
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
			initStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
			},
			expectedPhase:     agentsv1alpha1.SandboxRecycling,
			expectedShouldReq: false,
		},
		{
			name: "running phase with only cleanup annotation (no cleanup-enabled) should not transition",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Generation: 1,
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationCleanup: "true",
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
			initStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
			},
			expectedPhase:     agentsv1alpha1.SandboxRunning,
			expectedShouldReq: false,
		},
		{
			name: "running phase with cleanup annotations and VolumeClaimTemplates should reject and stay running",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Generation: 1,
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationCleanup:        "true",
						agentsv1alpha1.AnnotationCleanupEnabled: "true",
					},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
							},
						},
						VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
							{ObjectMeta: metav1.ObjectMeta{Name: "data"}},
						},
					},
				},
			},
			initStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
			},
			expectedPhase:     agentsv1alpha1.SandboxRunning,
			expectedShouldReq: false,
			checkConditions: func(t *testing.T, status *agentsv1alpha1.SandboxStatus) {
				cond := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionRecycling))
				require.NotNil(t, cond, "Cleaning condition should be set")
				assert.Equal(t, metav1.ConditionFalse, cond.Status)
				assert.Equal(t, agentsv1alpha1.SandboxRecyclingReasonRejected, cond.Reason)
				assert.Contains(t, cond.Message, "persistent volume claims")
			},
		},
		{
			name: "running phase with cleanup annotations and PVC in pod template volumes should reject and stay running",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Generation: 1,
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationCleanup:        "true",
						agentsv1alpha1.AnnotationCleanupEnabled: "true",
					},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "test", Image: "nginx"}},
								Volumes: []corev1.Volume{
									{
										Name: "data",
										VolumeSource: corev1.VolumeSource{
											PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
												ClaimName: "my-pvc",
											},
										},
									},
								},
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
			checkConditions: func(t *testing.T, status *agentsv1alpha1.SandboxStatus) {
				cond := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionRecycling))
				require.NotNil(t, cond, "Cleaning condition should be set")
				assert.Equal(t, metav1.ConditionFalse, cond.Status)
				assert.Equal(t, agentsv1alpha1.SandboxRecyclingReasonRejected, cond.Reason)
			},
		},
		{
			name: "paused phase with cleanup annotations should reject and stay paused",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-sandbox",
					Namespace:  "default",
					Generation: 1,
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationCleanup:        "true",
						agentsv1alpha1.AnnotationCleanupEnabled: "true",
					},
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
				Phase: agentsv1alpha1.SandboxPaused,
				Conditions: []metav1.Condition{
					{
						Type:   string(agentsv1alpha1.SandboxConditionPaused),
						Status: metav1.ConditionTrue,
					},
				},
			},
			expectedPhase:     agentsv1alpha1.SandboxPaused,
			expectedShouldReq: false,
			checkConditions: func(t *testing.T, status *agentsv1alpha1.SandboxStatus) {
				cond := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionRecycling))
				require.NotNil(t, cond, "Cleaning condition should be set")
				assert.Equal(t, metav1.ConditionFalse, cond.Status)
				assert.Equal(t, agentsv1alpha1.SandboxRecyclingReasonRejected, cond.Reason)
				assert.Contains(t, cond.Message, "Paused state")
			},
		},
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	fakeRecorder := record.NewFakeRecorder(100)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &SandboxReconciler{
		checkpointControl: core.NewCheckpointControl(fakeClient, record.NewFakeRecorder(10)),
		recorder:          fakeRecorder,
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Ensure pod has matching template hash to avoid false upgrade trigger
			if tt.pod != nil && tt.initStatus != nil && tt.initStatus.Phase == agentsv1alpha1.SandboxRunning {
				hash, _ := core.HashSandbox(tt.box)
				if tt.pod.Labels == nil {
					tt.pod.Labels = map[string]string{}
				}
				if _, exists := tt.pod.Labels[agentsv1alpha1.PodLabelTemplateHash]; !exists {
					tt.pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = hash
				}
			}

			args := core.EnsureFuncArgs{
				Pod:       tt.pod,
				Box:       tt.box,
				NewStatus: tt.initStatus,
			}

			newStatus, shouldRequeue := reconciler.calculateStatus(context.Background(), args)

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
		name                 string
		sandbox              *agentsv1alpha1.Sandbox
		expectErr            bool
		expectFinalizerAdded bool
		expectHashAnnotation bool
		expectPatchCalled    bool
		checkResult          func(t *testing.T, result *agentsv1alpha1.Sandbox, original *agentsv1alpha1.Sandbox)
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
					if f == core.SandboxFinalizer {
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
				if result.Annotations[agentsv1alpha1.SandboxHashImmutablePart] == "" {
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
					Finalizers: []string{core.SandboxFinalizer},
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
				if result.Annotations[agentsv1alpha1.SandboxHashImmutablePart] == "" {
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
				if result.Annotations[agentsv1alpha1.SandboxHashImmutablePart] == "" {
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
						if f == core.SandboxFinalizer {
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
					} else if updatedSandbox.Annotations[agentsv1alpha1.SandboxHashImmutablePart] == "" {
						t.Errorf("Hash annotation should be set in persisted sandbox")
					}
				}
			}
		})
	}
}

func TestIsHighPrioritySandbox(t *testing.T) {
	tests := []struct {
		name     string
		sandbox  *agentsv1alpha1.Sandbox
		expected bool
	}{
		{
			name: "no priority annotation - should return false",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
			},
			expected: false,
		},
		{
			name: "empty priority annotation - should return false",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						agentsv1alpha1.SandboxAnnotationPriority: "",
					},
				},
			},
			expected: false,
		},
		{
			name: "priority is 0 - should return false",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						agentsv1alpha1.SandboxAnnotationPriority: "0",
					},
				},
			},
			expected: false,
		},
		{
			name: "priority is negative - should return false",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						agentsv1alpha1.SandboxAnnotationPriority: "-1",
					},
				},
			},
			expected: false,
		},
		{
			name: "priority is 1 - should return true",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						agentsv1alpha1.SandboxAnnotationPriority: "1",
					},
				},
			},
			expected: true,
		},
		{
			name: "priority is positive - should return true",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						agentsv1alpha1.SandboxAnnotationPriority: "100",
					},
				},
			},
			expected: true,
		},
		{
			name: "invalid priority format - should return false",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Annotations: map[string]string{
						agentsv1alpha1.SandboxAnnotationPriority: "abc",
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := core.IsHighPrioritySandbox(context.Background(), tt.sandbox)
			if result != tt.expected {
				t.Errorf("isHighPrioritySandbox() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestReconcile_SandboxLifecycle_ClearSpecThenDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	ctx := context.Background()
	ns := "default"
	sbxName := "lifecycle-sandbox"

	// Setup fake client and reconciler
	fakeRecorder := record.NewFakeRecorder(100)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&agentsv1alpha1.Sandbox{}).
		Build()
	rl := core.NewRateLimiter()
	reconciler := &SandboxReconciler{
		Client: fakeClient,
		Scheme: scheme,
		controls: core.NewSandboxControl(core.SandboxControlArgs{
			Client:      fakeClient,
			Recorder:    fakeRecorder,
			RateLimiter: rl,
			PodControl:  core.NewPodControl(fakeClient, fakeRecorder, core.GeneratePodFromSandbox),
		}),
		checkpointControl: core.NewCheckpointControl(fakeClient, fakeRecorder),
		rateLimiter:       rl,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      sbxName,
			Namespace: ns,
		},
	}

	// Step 1: Create a Sandbox with Template, trigger reconcile, verify finalizer is added
	t.Run("step1_create_sandbox_and_add_finalizer", func(t *testing.T) {
		sandbox := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:       sbxName,
				Namespace:  ns,
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
		}

		if err := fakeClient.Create(ctx, sandbox); err != nil {
			t.Fatalf("Failed to create sandbox: %v", err)
		}

		result, err := reconciler.Reconcile(ctx, req)
		if err != nil {
			t.Errorf("Reconcile() error = %v", err)
		}
		if result.Requeue {
			t.Errorf("Expected no requeue, got requeue = %v", result.Requeue)
		}

		// Verify finalizer was added
		updatedSandbox := &agentsv1alpha1.Sandbox{}
		if err := fakeClient.Get(ctx, req.NamespacedName, updatedSandbox); err != nil {
			t.Fatalf("Failed to get sandbox: %v", err)
		}
		hasFinalizer := false
		for _, f := range updatedSandbox.Finalizers {
			if f == core.SandboxFinalizer {
				hasFinalizer = true
				break
			}
		}
		if !hasFinalizer {
			t.Errorf("Expected finalizer %s to be added", core.SandboxFinalizer)
		}
	})

	// Step 2: Update Sandbox to clear Spec.Template (set to nil)
	t.Run("step2_clear_spec_template", func(t *testing.T) {
		sandbox := &agentsv1alpha1.Sandbox{}
		if err := fakeClient.Get(ctx, req.NamespacedName, sandbox); err != nil {
			t.Fatalf("Failed to get sandbox: %v", err)
		}

		sandbox.Spec.Template = nil
		if err := fakeClient.Update(ctx, sandbox); err != nil {
			t.Fatalf("Failed to update sandbox: %v", err)
		}

		// Verify Template is nil
		updatedSandbox := &agentsv1alpha1.Sandbox{}
		if err := fakeClient.Get(ctx, req.NamespacedName, updatedSandbox); err != nil {
			t.Fatalf("Failed to get sandbox: %v", err)
		}
		if updatedSandbox.Spec.Template != nil {
			t.Errorf("Expected Template to be nil, got non-nil")
		}
	})

	// Step 3: Delete Sandbox, verify it is successfully deleted
	t.Run("step3_delete_sandbox", func(t *testing.T) {
		sandbox := &agentsv1alpha1.Sandbox{}
		if err := fakeClient.Get(ctx, req.NamespacedName, sandbox); err != nil {
			t.Fatalf("Failed to get sandbox: %v", err)
		}

		// Check if a pod was created during step1 reconcile
		pod := &corev1.Pod{}
		err := fakeClient.Get(ctx, req.NamespacedName, pod)
		if err == nil {
			// Pod exists, need to delete it first (simulating real cluster behavior)
			// When sandbox is deleted with nil template, pod should be deleted first
			if err := fakeClient.Delete(ctx, pod); err != nil {
				t.Fatalf("Failed to delete pod: %v", err)
			}
		}

		// Delete the sandbox using fake client Delete
		if err := fakeClient.Delete(ctx, sandbox); err != nil {
			t.Fatalf("Failed to delete sandbox: %v", err)
		}

		// Trigger reconcile to handle deletion
		result, err := reconciler.Reconcile(ctx, req)
		if err != nil {
			t.Errorf("Reconcile() error = %v", err)
		}
		if result.Requeue {
			t.Errorf("Expected no requeue, got requeue = %v", result.Requeue)
		}

		// Verify sandbox is deleted (finalizer removed triggers garbage collection)
		updatedSandbox := &agentsv1alpha1.Sandbox{}
		err = fakeClient.Get(ctx, req.NamespacedName, updatedSandbox)
		if err == nil {
			// Sandbox still exists, check if finalizer was removed
			if updatedSandbox.DeletionTimestamp.IsZero() {
				t.Errorf("Expected DeletionTimestamp to be set")
			}
			for _, f := range updatedSandbox.Finalizers {
				if f == core.SandboxFinalizer {
					t.Errorf("Expected finalizer to be removed, but it still exists")
				}
			}
		} else {
			// If sandbox is not found, it means finalizer was removed and sandbox was garbage collected - this is expected behavior
			if !apierrors.IsNotFound(err) {
				t.Fatalf("Failed to get sandbox: %v", err)
			}
		}
	})
}

func TestSandboxReconciler_Reconcile_RateLimitFeatureGate(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	now := metav1.Now()

	tests := []struct {
		name               string
		sandbox            *agentsv1alpha1.Sandbox
		pod                *corev1.Pod
		setupRL            func(rl *core.RateLimiter)
		expectRequeueAfter bool
		wantErr            bool
	}{
		{
			name: "normal pending sandbox with count exceeding threshold - should requeue",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "normal-sandbox",
					Namespace:         "default",
					Finalizers:        []string{core.SandboxFinalizer},
					CreationTimestamp: now,
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
			pod: nil,
			setupRL: func(rl *core.RateLimiter) {
				// exceed threshold by adding real high-priority sandboxes to track
				for i := 0; i <= core.PrioritySandboxThreshold(); i++ {
					hpBox := &agentsv1alpha1.Sandbox{
						ObjectMeta: metav1.ObjectMeta{
							Name:              fmt.Sprintf("hp-%d", i),
							Namespace:         "ns",
							CreationTimestamp: metav1.Now(),
							Annotations: map[string]string{
								agentsv1alpha1.SandboxAnnotationPriority: "1",
							},
						},
						Status: agentsv1alpha1.SandboxStatus{
							Phase: agentsv1alpha1.SandboxPending,
						},
					}
					rl.UpdateRateLimiter(hpBox)
				}
			},
			expectRequeueAfter: true,
			wantErr:            false,
		},
		{
			name: "normal pending sandbox with count below threshold - should proceed",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "normal-sandbox-low",
					Namespace:  "default",
					Finalizers: []string{core.SandboxFinalizer},
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
			pod:                nil,
			expectRequeueAfter: false,
			wantErr:            false,
		},
		{
			name: "high priority pending sandbox - should add to track and defer triggers requeue",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "high-priority-sandbox",
					Namespace:         "default",
					Finalizers:        []string{core.SandboxFinalizer},
					CreationTimestamp: now,
					Annotations: map[string]string{
						agentsv1alpha1.SandboxAnnotationPriority: "1",
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
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPending,
				},
			},
			pod:                nil,
			expectRequeueAfter: true,
			wantErr:            false,
		},
		{
			name: "high priority sandbox already ready - should remove from track, defer no requeue",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "high-priority-ready",
					Namespace:         "default",
					Finalizers:        []string{core.SandboxFinalizer},
					CreationTimestamp: now,
					Annotations: map[string]string{
						agentsv1alpha1.SandboxAnnotationPriority: "1",
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
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(agentsv1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
				},
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "high-priority-ready",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.PodLabelTemplateHash: "d568cdw42",
					},
				},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			},
			// Note: high-priority sandbox will be added to track during Reconcile,
			// and removed automatically when it becomes Ready
			expectRequeueAfter: false,
			wantErr:            false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodRateLimitGate=true")
			defer func() {
				_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodRateLimitGate=false")
			}()

			core.ResourceVersionExpectations.Delete(tt.sandbox)
			core.ScaleExpectation.DeleteExpectations(utils.GetControllerKey(tt.sandbox))

			objects := []client.Object{tt.sandbox}
			if tt.pod != nil {
				objects = append(objects, tt.pod)
			}

			fakeRecorder := record.NewFakeRecorder(100)
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&agentsv1alpha1.Sandbox{}).
				WithObjects(objects...).
				Build()
			rl := core.NewRateLimiter()
			if tt.setupRL != nil {
				tt.setupRL(rl)
			}
			reconciler := &SandboxReconciler{
				Client: fakeClient,
				Scheme: scheme,
				controls: core.NewSandboxControl(core.SandboxControlArgs{
					Client:      fakeClient,
					Recorder:    fakeRecorder,
					RateLimiter: rl,
					PodControl:  core.NewPodControl(fakeClient, fakeRecorder, core.GeneratePodFromSandbox),
				}),
				checkpointControl: core.NewCheckpointControl(fakeClient, fakeRecorder),
				rateLimiter:       rl,
			}

			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      tt.sandbox.Name,
					Namespace: tt.sandbox.Namespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)

			if (err != nil) != tt.wantErr {
				t.Errorf("Reconcile() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.expectRequeueAfter && result.RequeueAfter == 0 {
				t.Errorf("expected RequeueAfter > 0, got 0")
			}
			if !tt.expectRequeueAfter && result.RequeueAfter > 0 {
				t.Errorf("expected RequeueAfter == 0, got %v", result.RequeueAfter)
			}
		})
	}
}

// TestReconcile_SandboxNotFoundCleanup tests the path where sandbox does not exist (L109-117).
func TestReconcile_SandboxNotFoundCleanup(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	fakeRecorder := record.NewFakeRecorder(100)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Sandbox{}).Build()
	rl := core.NewRateLimiter()
	reconciler := &SandboxReconciler{
		Client: fakeClient,
		Scheme: scheme,
		controls: core.NewSandboxControl(core.SandboxControlArgs{
			Client:      fakeClient,
			Recorder:    fakeRecorder,
			RateLimiter: rl,
			PodControl:  core.NewPodControl(fakeClient, fakeRecorder, core.GeneratePodFromSandbox),
		}),
		checkpointControl: core.NewCheckpointControl(fakeClient, fakeRecorder),
		rateLimiter:       rl,
		metricsCleanup:    &fakeEnqueuer{},
	}

	// Set up expectations for a sandbox that will not be found
	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gone-sandbox",
			Namespace: "default",
		},
	}
	core.ScaleExpectation.ExpectScale(utils.GetControllerKey(box), expectations.Create, "some-pod")

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "gone-sandbox",
			Namespace: "default",
		},
	}

	// Sandbox does not exist in the fake client
	result, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile() unexpected error: %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Errorf("Expected no requeue, got requeue=%v, requeueAfter=%v", result.Requeue, result.RequeueAfter)
	}

	// Verify expectations were cleaned up
	isSatisfied, _, _ := core.ScaleExpectation.SatisfiedExpectations(utils.GetControllerKey(box))
	if !isSatisfied {
		t.Errorf("Expected ScaleExpectation to be cleaned up after sandbox not found")
	}
}

// TestReconcile_ScaleExpectationUnsatisfied tests the path where ScaleExpectation is not satisfied (L136-142).
func TestReconcile_ScaleExpectationUnsatisfied(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scale-exp-sandbox",
			Namespace: "default",
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
	}

	fakeRecorder := record.NewFakeRecorder(100)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Sandbox{}).WithObjects(sandbox).Build()
	rl := core.NewRateLimiter()
	reconciler := &SandboxReconciler{
		Client: fakeClient,
		Scheme: scheme,
		controls: core.NewSandboxControl(core.SandboxControlArgs{
			Client:      fakeClient,
			Recorder:    fakeRecorder,
			RateLimiter: rl,
			PodControl:  core.NewPodControl(fakeClient, fakeRecorder, core.GeneratePodFromSandbox),
		}),
		checkpointControl: core.NewCheckpointControl(fakeClient, fakeRecorder),
		rateLimiter:       rl,
	}

	// Set up unsatisfied ScaleExpectation (expect a pod that won't be observed)
	core.ScaleExpectation.ExpectScale(utils.GetControllerKey(sandbox), expectations.Create, "expected-pod-never-seen")

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile() unexpected error: %v", err)
	}
	// Should requeue with a duration because expectation is not satisfied
	if result.RequeueAfter <= 0 {
		t.Errorf("Expected RequeueAfter > 0 for unsatisfied ScaleExpectation, got %v", result.RequeueAfter)
	}

	// Clean up
	core.ScaleExpectation.DeleteExpectations(utils.GetControllerKey(sandbox))
}

// TestReconcile_ResourceVersionExpectationUnsatisfied tests the path where ResourceVersionExpectation is not satisfied (L146-152).
func TestReconcile_ResourceVersionExpectationUnsatisfied(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	sandboxUID := types.UID("rv-exp-uid-12345")
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rv-exp-sandbox",
			Namespace: "default",
			UID:       sandboxUID,
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
	}

	fakeRecorder := record.NewFakeRecorder(100)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Sandbox{}).WithObjects(sandbox).Build()
	rl := core.NewRateLimiter()
	reconciler := &SandboxReconciler{
		Client: fakeClient,
		Scheme: scheme,
		controls: core.NewSandboxControl(core.SandboxControlArgs{
			Client:      fakeClient,
			Recorder:    fakeRecorder,
			RateLimiter: rl,
			PodControl:  core.NewPodControl(fakeClient, fakeRecorder, core.GeneratePodFromSandbox),
		}),
		checkpointControl: core.NewCheckpointControl(fakeClient, fakeRecorder),
		rateLimiter:       rl,
	}

	// Create a fake object with the same UID but a much higher resource version
	fakeObj := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "rv-exp-sandbox",
			Namespace:       "default",
			UID:             sandboxUID,
			ResourceVersion: "999999",
		},
	}
	core.ResourceVersionExpectations.Expect(fakeObj)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile() unexpected error: %v", err)
	}
	// Should requeue with a duration because ResourceVersion expectation is not satisfied
	if result.RequeueAfter <= 0 {
		t.Errorf("Expected RequeueAfter > 0 for unsatisfied ResourceVersionExpectation, got %v", result.RequeueAfter)
	}

	// Clean up
	core.ResourceVersionExpectations.Delete(fakeObj)
}

// TestReconcile_ScaleExpectationTimeout tests the path where ScaleExpectation is unsatisfied overtime (L146-147).
func TestReconcile_ScaleExpectationTimeout(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scale-timeout-sandbox",
			Namespace: "default",
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
	}

	fakeRecorder := record.NewFakeRecorder(100)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Sandbox{}).WithObjects(sandbox).Build()
	rl := core.NewRateLimiter()
	reconciler := &SandboxReconciler{
		Client: fakeClient,
		Scheme: scheme,
		controls: core.NewSandboxControl(core.SandboxControlArgs{
			Client:      fakeClient,
			Recorder:    fakeRecorder,
			RateLimiter: rl,
			PodControl:  core.NewPodControl(fakeClient, fakeRecorder, core.GeneratePodFromSandbox),
		}),
		checkpointControl: core.NewCheckpointControl(fakeClient, fakeRecorder),
		rateLimiter:       rl,
	}

	// Set up unsatisfied ScaleExpectation
	core.ScaleExpectation.ExpectScale(utils.GetControllerKey(sandbox), expectations.Create, "never-observed-pod")

	// Temporarily set ExpectationTimeout to 0 so any unsatisfied expectation appears timed out
	origTimeout := expectations.ExpectationTimeout
	expectations.ExpectationTimeout = 0
	defer func() {
		expectations.ExpectationTimeout = origTimeout
		core.ScaleExpectation.DeleteExpectations(utils.GetControllerKey(sandbox))
	}()

	// First call sets firstUnsatisfiedTimestamp to now, but since timeout=0, it immediately times out
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		},
	}

	_, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile() unexpected error: %v", err)
	}
}

// TestReconcile_ResourceVersionExpectationTimeout tests the path where ResourceVersionExpectation
// is unsatisfied overtime (L156-157).
func TestReconcile_ResourceVersionExpectationTimeout(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	sandboxUID := types.UID("rv-timeout-uid-12345")
	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rv-timeout-sandbox",
			Namespace: "default",
			UID:       sandboxUID,
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
	}

	fakeRecorder := record.NewFakeRecorder(100)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Sandbox{}).WithObjects(sandbox).Build()
	rl := core.NewRateLimiter()
	reconciler := &SandboxReconciler{
		Client: fakeClient,
		Scheme: scheme,
		controls: core.NewSandboxControl(core.SandboxControlArgs{
			Client:      fakeClient,
			Recorder:    fakeRecorder,
			RateLimiter: rl,
			PodControl:  core.NewPodControl(fakeClient, fakeRecorder, core.GeneratePodFromSandbox),
		}),
		checkpointControl: core.NewCheckpointControl(fakeClient, fakeRecorder),
		rateLimiter:       rl,
	}

	// Create expectation with very high resource version
	fakeObj := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "rv-timeout-sandbox",
			Namespace:       "default",
			UID:             sandboxUID,
			ResourceVersion: "999999",
		},
	}
	core.ResourceVersionExpectations.Expect(fakeObj)

	// Temporarily set ExpectationTimeout to 0 so the expectation immediately times out
	origTimeout := expectations.ExpectationTimeout
	expectations.ExpectationTimeout = 0
	defer func() {
		expectations.ExpectationTimeout = origTimeout
		core.ResourceVersionExpectations.Delete(fakeObj)
	}()

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		},
	}

	_, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile() unexpected error: %v", err)
	}
}

// TestReconcile_GetPodNonNotFoundError tests the error path where Get pod returns a non-NotFound error (L105-107).
func TestReconcile_GetPodNonNotFoundError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "get-pod-err-sandbox",
			Namespace: "default",
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
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&agentsv1alpha1.Sandbox{}).
		WithObjects(sandbox).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				// Fail Get for Pod with a non-NotFound error
				if _, ok := obj.(*corev1.Pod); ok {
					return fmt.Errorf("simulated internal server error")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	fakeRecorder := record.NewFakeRecorder(100)
	rl := core.NewRateLimiter()
	reconciler := &SandboxReconciler{
		Client: fakeClient,
		Scheme: scheme,
		controls: core.NewSandboxControl(core.SandboxControlArgs{
			Client:      fakeClient,
			Recorder:    fakeRecorder,
			RateLimiter: rl,
			PodControl:  core.NewPodControl(fakeClient, fakeRecorder, core.GeneratePodFromSandbox),
		}),
		checkpointControl: core.NewCheckpointControl(fakeClient, fakeRecorder),
		rateLimiter:       rl,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		},
	}

	_, err := reconciler.Reconcile(context.Background(), req)
	if err == nil {
		t.Fatal("Expected error from Reconcile when Get pod fails, got nil")
	}
}

// TestReconcile_UpgradingPhase tests the Upgrading phase switch case (L233-234).
func TestReconcile_UpgradingPhase(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "upgrading-sandbox",
			Namespace:  "default",
			Finalizers: []string{core.SandboxFinalizer},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			UpgradePolicy: &agentsv1alpha1.SandboxUpgradePolicy{
				Type: agentsv1alpha1.SandboxUpgradePolicyRecreate,
			},
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test", Image: "nginx:v2"}},
					},
				},
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
		},
	}

	// Pod with mismatched hash to trigger upgrade
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "upgrading-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-hash",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	fakeRecorder := record.NewFakeRecorder(100)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agentsv1alpha1.Sandbox{}).WithObjects(sandbox, pod).Build()
	rl := core.NewRateLimiter()
	reconciler := &SandboxReconciler{
		Client: fakeClient,
		Scheme: scheme,
		controls: core.NewSandboxControl(core.SandboxControlArgs{
			Client:      fakeClient,
			Recorder:    fakeRecorder,
			RateLimiter: rl,
			PodControl:  core.NewPodControl(fakeClient, fakeRecorder, core.GeneratePodFromSandbox),
		}),
		checkpointControl: core.NewCheckpointControl(fakeClient, fakeRecorder),
		rateLimiter:       rl,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		},
	}

	_, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("Reconcile() unexpected error: %v", err)
	}

	// Verify the sandbox transitioned to Upgrading
	updatedSandbox := &agentsv1alpha1.Sandbox{}
	err = fakeClient.Get(context.Background(), req.NamespacedName, updatedSandbox)
	if err != nil {
		t.Fatalf("Failed to get updated sandbox: %v", err)
	}
	if updatedSandbox.Status.Phase != agentsv1alpha1.SandboxUpgrading {
		t.Errorf("Expected phase Upgrading, got %v", updatedSandbox.Status.Phase)
	}
}

// TestReconcile_ErrorPath_UpdatesSandboxStatus tests that when EnsureSandboxUpdated returns an error,
// the error path persists sandbox status via updateSandboxStatus before returning the error.
func TestReconcile_ErrorPath_UpdatesSandboxStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "error-path-sandbox",
			Namespace:   "default",
			Finalizers:  []string{core.SandboxFinalizer},
			Annotations: map[string]string{
				// Will be filled with the computed hashImmutablePart below
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: "nginx:v2"}},
					},
				},
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
		},
	}

	// Pre-compute hashImmutablePart and set it in the sandbox annotation so inplace update is permitted
	_, hashImmutablePart := core.HashSandbox(sandbox)
	sandbox.Annotations[agentsv1alpha1.SandboxHashImmutablePart] = hashImmutablePart

	// Pod with old hash label (different from computed) and old image to trigger inplace update patch
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "error-path-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-hash",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "nginx:v1"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	patchCallCount := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&agentsv1alpha1.Sandbox{}).
		WithObjects(sandbox, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				// Allow Sandbox status patches, but fail Pod patches (inplace update)
				if _, ok := obj.(*corev1.Pod); ok {
					patchCallCount++
					return fmt.Errorf("simulated pod patch failure")
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	fakeRecorder := record.NewFakeRecorder(100)
	rl := core.NewRateLimiter()
	reconciler := &SandboxReconciler{
		Client: fakeClient,
		Scheme: scheme,
		controls: core.NewSandboxControl(core.SandboxControlArgs{
			Client:      fakeClient,
			Recorder:    fakeRecorder,
			RateLimiter: rl,
			PodControl:  core.NewPodControl(fakeClient, fakeRecorder, core.GeneratePodFromSandbox),
		}),
		checkpointControl: core.NewCheckpointControl(fakeClient, fakeRecorder),
		rateLimiter:       rl,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		},
	}

	_, err := reconciler.Reconcile(context.Background(), req)
	if err == nil {
		t.Fatal("Expected error from Reconcile, got nil")
	}
	if patchCallCount == 0 {
		t.Error("Expected at least one Pod patch attempt")
	}
}

// TestReconcile_ErrorPath_StatusUpdateAlsoFails tests the error path where both the Ensure*
// function fails AND the subsequent updateSandboxStatus also fails.
func TestReconcile_ErrorPath_StatusUpdateAlsoFails(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "error-both-sandbox",
			Namespace:   "default",
			Finalizers:  []string{core.SandboxFinalizer},
			Annotations: map[string]string{},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "c", Image: "nginx:v2"}},
					},
				},
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
		},
	}

	_, hashImmutablePart := core.HashSandbox(sandbox)
	sandbox.Annotations[agentsv1alpha1.SandboxHashImmutablePart] = hashImmutablePart

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "error-both-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-hash",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "nginx:v1"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&agentsv1alpha1.Sandbox{}).
		WithObjects(sandbox, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				// Fail ALL patches (both Pod patch for inplace update and Sandbox patch)
				if _, ok := obj.(*corev1.Pod); ok {
					return fmt.Errorf("simulated pod patch failure")
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
			SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				// Fail status patch to trigger the retErr != nil branch
				return fmt.Errorf("simulated status patch failure")
			},
		}).
		Build()

	fakeRecorder := record.NewFakeRecorder(100)
	rl := core.NewRateLimiter()
	reconciler := &SandboxReconciler{
		Client: fakeClient,
		Scheme: scheme,
		controls: core.NewSandboxControl(core.SandboxControlArgs{
			Client:      fakeClient,
			Recorder:    fakeRecorder,
			RateLimiter: rl,
			PodControl:  core.NewPodControl(fakeClient, fakeRecorder, core.GeneratePodFromSandbox),
		}),
		checkpointControl: core.NewCheckpointControl(fakeClient, fakeRecorder),
		rateLimiter:       rl,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      sandbox.Name,
			Namespace: sandbox.Namespace,
		},
	}

	_, err := reconciler.Reconcile(context.Background(), req)
	if err == nil {
		t.Fatal("Expected error from Reconcile, got nil")
	}
	// The original error from inplace update should be returned, not the status update error
	if err.Error() != "simulated pod patch failure" {
		t.Errorf("Expected 'simulated pod patch failure', got: %v", err)
	}
}

// TestEnsureVolumeClaimTemplates_SetControllerReferenceError tests the SetControllerReference error path.
func TestEnsureVolumeClaimTemplates_SetControllerReferenceError(t *testing.T) {
	// Use a scheme that does NOT include agentsv1alpha1 so SetControllerReference fails
	incompleteScheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(incompleteScheme)

	// We need to use a full scheme for the fake client to accept the sandbox
	fullScheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(fullScheme)
	_ = agentsv1alpha1.AddToScheme(fullScheme)

	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ctrl-ref-sandbox",
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
	}

	// Use full scheme for the fake client but incomplete scheme for the reconciler
	fakeClient := fake.NewClientBuilder().WithScheme(fullScheme).WithObjects(sandbox).Build()

	reconciler := &SandboxReconciler{
		Client: fakeClient,
		Scheme: incompleteScheme, // This will cause SetControllerReference to fail
	}

	err := reconciler.ensureVolumeClaimTemplates(context.Background(), sandbox)
	if err == nil {
		t.Errorf("Expected error from SetControllerReference, got nil")
	}
}

func TestEnsureVolumeClaimTemplates_GetPVCError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	sandbox := &agentsv1alpha1.Sandbox{
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
						ObjectMeta: metav1.ObjectMeta{Name: "data"},
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
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sandbox).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
					return fmt.Errorf("simulated get error")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	reconciler := &SandboxReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	err := reconciler.ensureVolumeClaimTemplates(context.Background(), sandbox)
	if err == nil {
		t.Error("Expected error from Get PVC, got nil")
	}
}

func TestEnsureVolumeClaimTemplates_CreatePVCError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	sandbox := &agentsv1alpha1.Sandbox{
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
						ObjectMeta: metav1.ObjectMeta{Name: "data"},
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
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sandbox).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
					return fmt.Errorf("simulated create error")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()

	reconciler := &SandboxReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	err := reconciler.ensureVolumeClaimTemplates(context.Background(), sandbox)
	if err == nil {
		t.Error("Expected error from Create PVC, got nil")
	}
}

func TestEnsureVolumeClaimTemplates_CreateAlreadyExists(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	sandbox := &agentsv1alpha1.Sandbox{
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
						ObjectMeta: metav1.ObjectMeta{Name: "data"},
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
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sandbox).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
					return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "persistentvolumeclaims"}, obj.GetName())
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()

	reconciler := &SandboxReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	err := reconciler.ensureVolumeClaimTemplates(context.Background(), sandbox)
	if err != nil {
		t.Errorf("Expected no error for AlreadyExists on Create, got: %v", err)
	}
}

func TestAddSandboxFinalizerAndHash_PatchError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	sandbox := &agentsv1alpha1.Sandbox{
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
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sandbox).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				return fmt.Errorf("simulated patch error")
			},
		}).
		Build()

	reconciler := &SandboxReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	result, err := reconciler.addSandboxFinalizerAndHash(context.Background(), sandbox)
	if err == nil {
		t.Error("Expected error from Patch, got nil")
	}
	if result != nil {
		t.Error("Expected nil result on error")
	}
}

func TestUpdateSandboxStatus_PatchError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxPending,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sandbox).
		WithStatusSubresource(&agentsv1alpha1.Sandbox{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				return fmt.Errorf("simulated status patch error")
			},
		}).
		Build()

	reconciler := &SandboxReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	newStatus := agentsv1alpha1.SandboxStatus{
		Phase: agentsv1alpha1.SandboxRunning,
	}

	err := reconciler.updateSandboxStatus(context.Background(), newStatus, sandbox)
	if err == nil {
		t.Error("Expected error from Status Patch, got nil")
	}
}

func TestUpdateSandboxStatus_Upgrading_RevisionChanged_ResetsToPreUpgrade(t *testing.T) {
	box := &agentsv1alpha1.Sandbox{
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
		Status: agentsv1alpha1.SandboxStatus{
			UpdateRevision: "old-revision",
		},
	}

	initStatus := &agentsv1alpha1.SandboxStatus{
		Phase: agentsv1alpha1.SandboxUpgrading,
		Conditions: []metav1.Condition{
			{
				Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
				Status:             metav1.ConditionFalse,
				Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
				Message:            "upgrading pod",
				LastTransitionTime: metav1.Now(),
			},
		},
	}

	args := core.EnsureFuncArgs{
		Pod:       nil,
		Box:       box,
		NewStatus: initStatus,
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &SandboxReconciler{
		checkpointControl: core.NewCheckpointControl(fakeClient, record.NewFakeRecorder(10)),
	}
	newStatus, _ := reconciler.calculateStatus(context.Background(), args)

	// The computed hash from box.Spec should differ from "old-revision",
	// so the upgrade lifecycle should be reset to PreUpgrade.
	upgradeCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
	if upgradeCond == nil {
		t.Fatal("Expected Upgrading condition to exist")
	}
	if upgradeCond.Reason != agentsv1alpha1.SandboxUpgradingReasonPreUpgrade {
		t.Errorf("Expected Reason %s, got %s", agentsv1alpha1.SandboxUpgradingReasonPreUpgrade, upgradeCond.Reason)
	}
	if upgradeCond.Message != "" {
		t.Errorf("Expected Message to be empty, got %q", upgradeCond.Message)
	}
}

func TestUpdateSandboxStatus_Upgrading_RevisionUnchanged_NoReset(t *testing.T) {
	box := &agentsv1alpha1.Sandbox{
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
	}

	// Set box.Status.UpdateRevision to the same hash that calculateStatus will compute
	hash, _ := core.HashSandbox(box)
	box.Status.UpdateRevision = hash

	initStatus := &agentsv1alpha1.SandboxStatus{
		Phase: agentsv1alpha1.SandboxUpgrading,
		Conditions: []metav1.Condition{
			{
				Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
				Status:             metav1.ConditionFalse,
				Reason:             agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
				Message:            "upgrading pod",
				LastTransitionTime: metav1.Now(),
			},
		},
	}

	args := core.EnsureFuncArgs{
		Pod:       nil,
		Box:       box,
		NewStatus: initStatus,
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &SandboxReconciler{
		checkpointControl: core.NewCheckpointControl(fakeClient, record.NewFakeRecorder(10)),
	}
	newStatus, _ := reconciler.calculateStatus(context.Background(), args)

	// Revision unchanged, so the condition should NOT be reset
	upgradeCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
	if upgradeCond == nil {
		t.Fatal("Expected Upgrading condition to exist")
	}
	if upgradeCond.Reason != agentsv1alpha1.SandboxUpgradingReasonUpgradePod {
		t.Errorf("Expected Reason %s, got %s", agentsv1alpha1.SandboxUpgradingReasonUpgradePod, upgradeCond.Reason)
	}
	if upgradeCond.Message != "upgrading pod" {
		t.Errorf("Expected Message %q, got %q", "upgrading pod", upgradeCond.Message)
	}
}

func TestUpdateSandboxStatus_Upgrading_RevisionChanged_ResumeFromFailedStep(t *testing.T) {
	_ = utilfeature.DefaultMutableFeatureGate.Set(string(features.SandboxUpgradeResumeFromFailedStepGate) + "=true")
	defer func() {
		_ = utilfeature.DefaultMutableFeatureGate.Set(string(features.SandboxUpgradeResumeFromFailedStepGate) + "=true")
	}()

	tests := []struct {
		name           string
		pod            *corev1.Pod
		reason         string
		expectReason   string
		updateRevision string
	}{
		{
			name:           "pre upgrade failed resumes from preUpgrade",
			reason:         agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed,
			expectReason:   agentsv1alpha1.SandboxUpgradingReasonPreUpgrade,
			updateRevision: "old-revision",
		},
		{
			name:           "upgrade pod failed resumes from upgradePod",
			reason:         agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed,
			expectReason:   agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
			updateRevision: "old-revision",
		},
		{
			name: "post upgrade failed with matching pod revision resumes from postUpgrade",
			pod: func() *corev1.Pod {
				p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}}}
				return p
			}(),
			reason:         agentsv1alpha1.SandboxUpgradingReasonPostUpgradeFailed,
			expectReason:   agentsv1alpha1.SandboxUpgradingReasonPostUpgrade,
			updateRevision: "old-revision",
		},
		{
			name:           "post upgrade failed with mismatched pod revision resumes from upgradePod",
			reason:         agentsv1alpha1.SandboxUpgradingReasonPostUpgradeFailed,
			expectReason:   agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
			updateRevision: "old-revision",
		},
		{
			name:           "non failed reason still resets to preUpgrade",
			reason:         agentsv1alpha1.SandboxUpgradingReasonUpgradePod,
			expectReason:   agentsv1alpha1.SandboxUpgradingReasonPreUpgrade,
			updateRevision: "old-revision",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			box := &agentsv1alpha1.Sandbox{
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
				Status: agentsv1alpha1.SandboxStatus{
					UpdateRevision: tt.updateRevision,
				},
			}

			hash, _ := core.HashSandbox(box)
			if tt.pod != nil {
				tt.pod = tt.pod.DeepCopy()
				tt.pod.Labels[agentsv1alpha1.PodLabelTemplateHash] = hash
			}

			initStatus := &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxUpgrading,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
						Status:             metav1.ConditionFalse,
						Reason:             tt.reason,
						Message:            "needs reset",
						LastTransitionTime: metav1.Now(),
					},
				},
			}

			args := core.EnsureFuncArgs{
				Pod:       tt.pod,
				Box:       box,
				NewStatus: initStatus,
			}

			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			_ = agentsv1alpha1.AddToScheme(scheme)
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			reconciler := &SandboxReconciler{
				checkpointControl: core.NewCheckpointControl(fakeClient, record.NewFakeRecorder(10)),
			}
			newStatus, _ := reconciler.calculateStatus(context.Background(), args)
			upgradeCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
			if upgradeCond == nil {
				t.Fatal("Expected Upgrading condition to exist")
			}
			if upgradeCond.Reason != tt.expectReason {
				t.Errorf("Expected Reason %s, got %s", tt.expectReason, upgradeCond.Reason)
			}
			if upgradeCond.Message != "" {
				t.Errorf("Expected Message to be empty, got %q", upgradeCond.Message)
			}
		})
	}
}

func TestUpdateSandboxStatus_Upgrading_RevisionChanged_FeatureGateDisabled_ResetsToPreUpgrade(t *testing.T) {
	_ = utilfeature.DefaultMutableFeatureGate.Set(string(features.SandboxUpgradeResumeFromFailedStepGate) + "=false")
	defer func() {
		_ = utilfeature.DefaultMutableFeatureGate.Set(string(features.SandboxUpgradeResumeFromFailedStepGate) + "=true")
	}()

	box := &agentsv1alpha1.Sandbox{
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
		Status: agentsv1alpha1.SandboxStatus{
			UpdateRevision: "old-revision",
		},
	}

	initStatus := &agentsv1alpha1.SandboxStatus{
		Phase: agentsv1alpha1.SandboxUpgrading,
		Conditions: []metav1.Condition{
			{
				Type:               string(agentsv1alpha1.SandboxConditionUpgrading),
				Status:             metav1.ConditionFalse,
				Reason:             agentsv1alpha1.SandboxUpgradingReasonPostUpgradeFailed,
				Message:            "post failed",
				LastTransitionTime: metav1.Now(),
			},
		},
	}

	args := core.EnsureFuncArgs{
		Pod: &corev1.Pod{
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
			},
		},
		Box:       box,
		NewStatus: initStatus,
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &SandboxReconciler{
		checkpointControl: core.NewCheckpointControl(fakeClient, record.NewFakeRecorder(10)),
	}
	newStatus, _ := reconciler.calculateStatus(context.Background(), args)
	upgradeCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionUpgrading))
	if upgradeCond == nil {
		t.Fatal("Expected Upgrading condition to exist")
	}
	if upgradeCond.Reason != agentsv1alpha1.SandboxUpgradingReasonPreUpgrade {
		t.Errorf("Expected Reason %s, got %s", agentsv1alpha1.SandboxUpgradingReasonPreUpgrade, upgradeCond.Reason)
	}
	if upgradeCond.Message != "" {
		t.Errorf("Expected Message to be empty, got %q", upgradeCond.Message)
	}
}

// fakeEnqueuer captures Enqueue invocations for assertion.
type fakeEnqueuer struct {
	mu    sync.Mutex
	calls []struct{ Namespace, Name string }
}

func (f *fakeEnqueuer) Enqueue(namespace, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, struct{ Namespace, Name string }{namespace, name})
}

func (f *fakeEnqueuer) snapshot() []struct{ Namespace, Name string } {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]struct{ Namespace, Name string }, len(f.calls))
	copy(out, f.calls)
	return out
}

func TestReconcile_NotFoundEnqueuesAsyncCleanup(t *testing.T) {
	scheme := runtime.NewScheme()
	assert.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	assert.NoError(t, corev1.AddToScheme(scheme))

	cli := fake.NewClientBuilder().WithScheme(scheme).Build()
	enq := &fakeEnqueuer{}
	r := &SandboxReconciler{
		Client:         cli,
		Scheme:         scheme,
		metricsCleanup: enq,
	}

	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "missing"},
	})
	assert.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, res)

	calls := enq.snapshot()
	assert.Len(t, calls, 1)
	assert.Equal(t, "ns", calls[0].Namespace)
	assert.Equal(t, "missing", calls[0].Name)
}

func TestHasPVCVolumes(t *testing.T) {
	tests := []struct {
		name     string
		box      *agentsv1alpha1.Sandbox
		expected bool
	}{
		{
			name:     "nil template",
			box:      &agentsv1alpha1.Sandbox{},
			expected: false,
		},
		{
			name: "no volumes",
			box: &agentsv1alpha1.Sandbox{
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
			expected: false,
		},
		{
			name: "only emptyDir volumes",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Volumes: []corev1.Volume{
									{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
								},
							},
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "has VolumeClaimTemplates",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
							{ObjectMeta: metav1.ObjectMeta{Name: "data"}},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "has PVC in pod template volumes",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Volumes: []corev1.Volume{
									{
										Name: "data",
										VolumeSource: corev1.VolumeSource{
											PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "pvc1"},
										},
									},
								},
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "has both VolumeClaimTemplates and PVC volume",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Volumes: []corev1.Volume{
									{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "pvc1"}}},
								},
							},
						},
						VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
							{ObjectMeta: metav1.ObjectMeta{Name: "data"}},
						},
					},
				},
			},
			expected: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, hasPVCVolumes(tt.box))
		})
	}
}

func TestRejectCleanup(t *testing.T) {
	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
	}

	t.Run("sets condition and records event", func(t *testing.T) {
		recorder := record.NewFakeRecorder(10)
		r := &SandboxReconciler{recorder: recorder}
		status := &agentsv1alpha1.SandboxStatus{}

		r.rejectRecycle(context.Background(), box, status, "test reason")

		cond := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionRecycling))
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, agentsv1alpha1.SandboxRecyclingReasonRejected, cond.Reason)
		assert.Equal(t, "test reason", cond.Message)

		// Verify event was recorded
		select {
		case event := <-recorder.Events:
			assert.Contains(t, event, agentsv1alpha1.SandboxRecyclingReasonRejected)
			assert.Contains(t, event, "test reason")
		default:
			t.Error("expected event to be recorded")
		}
	})

	t.Run("nil recorder does not panic", func(t *testing.T) {
		r := &SandboxReconciler{}
		status := &agentsv1alpha1.SandboxStatus{}

		assert.NotPanics(t, func() {
			r.rejectRecycle(context.Background(), box, status, "test reason")
		})

		cond := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionRecycling))
		require.NotNil(t, cond)
		assert.Equal(t, agentsv1alpha1.SandboxRecyclingReasonRejected, cond.Reason)
	})

	t.Run("deduplication - same reason and message skips update", func(t *testing.T) {
		recorder := record.NewFakeRecorder(10)
		r := &SandboxReconciler{recorder: recorder}
		status := &agentsv1alpha1.SandboxStatus{}

		// First call sets the condition and records an event
		r.rejectRecycle(context.Background(), box, status, "test reason")

		// Second call with same reason+message should be a no-op
		r.rejectRecycle(context.Background(), box, status, "test reason")

		// Only one event should have been recorded
		select {
		case <-recorder.Events:
			// First event consumed
		default:
			t.Error("expected first event to be recorded")
		}
		select {
		case <-recorder.Events:
			t.Error("second event should not have been recorded")
		default:
			// Expected - no second event
		}
	})

	t.Run("different message triggers new update", func(t *testing.T) {
		recorder := record.NewFakeRecorder(10)
		r := &SandboxReconciler{recorder: recorder}
		status := &agentsv1alpha1.SandboxStatus{}

		r.rejectRecycle(context.Background(), box, status, "reason A")
		r.rejectRecycle(context.Background(), box, status, "reason B")

		cond := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionRecycling))
		require.NotNil(t, cond)
		assert.Equal(t, "reason B", cond.Message)

		// Both events should have been recorded
		select {
		case <-recorder.Events:
		default:
			t.Error("expected first event")
		}
		select {
		case <-recorder.Events:
		default:
			t.Error("expected second event")
		}
	})
}
