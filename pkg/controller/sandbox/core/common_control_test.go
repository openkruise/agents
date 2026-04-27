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

package core

import (
	"context"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
	"github.com/openkruise/agents/pkg/utils/inplaceupdate"
	"github.com/openkruise/agents/pkg/utils/sidecarutils"
)

func TestCommonControl_EnsureSandboxRunning(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name        string
		args        EnsureFuncArgs
		podExist    bool
		wantErr     bool
		wantRequeue time.Duration
		setupRL     func(rl *RateLimiter) // optional: pre-populate rate limiter
		featureGate bool
	}{
		{
			name: "pod does not exist, should create",
			args: EnsureFuncArgs{
				Pod: nil,
				Box: &agentsv1alpha1.Sandbox{
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
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			podExist: false,
			wantErr:  false,
		},
		{
			name: "pod exists but not running",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodPending,
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			podExist: true,
			wantErr:  false,
		},
		{
			name: "pod is running",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						Conditions: []corev1.PodCondition{
							{
								Type:               corev1.PodReady,
								Status:             corev1.ConditionTrue,
								LastTransitionTime: metav1.Now(),
							},
						},
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			podExist: true,
			wantErr:  false,
		},
		{
			name:        "feature gate enabled, threshold exceeded, normal sandbox rate-limited",
			featureGate: true,
			setupRL: func(rl *RateLimiter) {
				// pre-fill track to exceed threshold
				oldThreshold := prioritySandboxThreshold
				prioritySandboxThreshold = 0
				t.Cleanup(func() { prioritySandboxThreshold = oldThreshold })
				rl.highPrioritySandboxTrack["ns/hp1"] = &SandboxTrack{Namespace: "ns", Name: "hp1"}
			},
			args: EnsureFuncArgs{
				Pod: nil,
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "normal-sandbox",
						Namespace:         "default",
						CreationTimestamp: metav1.Now(), // within maxCreateSandboxDelay
					},
					Spec: agentsv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{{Name: "c", Image: "nginx"}},
								},
							},
						},
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxPending,
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			podExist:    false,
			wantErr:     false,
			wantRequeue: 3 * time.Second,
		},
		{
			name:        "feature gate enabled, high-priority sandbox bypasses rate limit",
			featureGate: true,
			setupRL: func(rl *RateLimiter) {
				oldThreshold := prioritySandboxThreshold
				prioritySandboxThreshold = 0
				t.Cleanup(func() { prioritySandboxThreshold = oldThreshold })
				rl.highPrioritySandboxTrack["ns/hp1"] = &SandboxTrack{Namespace: "ns", Name: "hp1"}
			},
			args: EnsureFuncArgs{
				Pod: nil,
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "high-sandbox",
						Namespace: "default",
						Annotations: map[string]string{
							agentsv1alpha1.SandboxAnnotationPriority: "1",
						},
					},
					Spec: agentsv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{{Name: "c", Image: "nginx"}},
								},
							},
						},
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			podExist:    false,
			wantErr:     false,
			wantRequeue: 0,
		},
		{
			name:        "feature gate disabled, no rate limiting even when threshold exceeded",
			featureGate: false,
			setupRL: func(rl *RateLimiter) {
				oldThreshold := prioritySandboxThreshold
				prioritySandboxThreshold = 0
				t.Cleanup(func() { prioritySandboxThreshold = oldThreshold })
				rl.highPrioritySandboxTrack["ns/hp1"] = &SandboxTrack{Namespace: "ns", Name: "hp1"}
			},
			args: EnsureFuncArgs{
				Pod: nil,
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "normal-sandbox",
						Namespace: "default",
					},
					Spec: agentsv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{{Name: "c", Image: "nginx"}},
								},
							},
						},
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			podExist:    false,
			wantErr:     false,
			wantRequeue: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// feature gate setup
			if tt.featureGate {
				_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodRateLimitGate=true")
				t.Cleanup(func() {
					_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodRateLimitGate=false")
				})
			}

			fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			rl := NewRateLimiter()
			if tt.setupRL != nil {
				tt.setupRL(rl)
			}
			control := &commonControl{
				Client:               fakeClient,
				recorder:             record.NewFakeRecorder(10),
				inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
				rateLimiter:          rl,
			}

			requeue, err := control.EnsureSandboxRunning(context.TODO(), tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("EnsureSandboxRunning() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if requeue != tt.wantRequeue {
				t.Errorf("EnsureSandboxRunning() requeue = %v, want %v", requeue, tt.wantRequeue)
			}

			// Verify that pod was created if it didn't exist and not rate-limited
			if !tt.podExist && tt.args.Pod == nil && tt.wantRequeue == 0 {
				pod := &corev1.Pod{}
				err := fakeClient.Get(context.TODO(), types.NamespacedName{Name: tt.args.Box.Name, Namespace: tt.args.Box.Namespace}, pod)
				if err != nil {
					t.Errorf("Expected pod to be created, but it wasn't: %v", err)
				}
			}

			// If pod is running, verify status was updated
			if tt.args.Pod != nil && tt.args.Pod.Status.Phase == corev1.PodRunning {
				if tt.args.NewStatus.Phase != agentsv1alpha1.SandboxRunning {
					t.Errorf("Expected sandbox phase to be Running, got %v", tt.args.NewStatus.Phase)
				}
			}
		})
	}
}

func TestCommonControl_EnsureSandboxUpdated(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name    string
		args    EnsureFuncArgs
		wantErr bool
	}{
		{
			name: "pod does not exist, should set failed phase",
			args: EnsureFuncArgs{
				Pod: nil,
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			wantErr: false,
		},
		{
			name: "pod exists, should update status fields",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Spec: corev1.PodSpec{
						NodeName: "node-1",
					},
					Status: corev1.PodStatus{
						PodIP: "10.0.0.1",
						Conditions: []corev1.PodCondition{
							{
								Type:               corev1.PodReady,
								Status:             corev1.ConditionTrue,
								LastTransitionTime: metav1.Now(),
							},
						},
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Spec: agentsv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									NodeName: "node-1",
								},
							},
						},
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
							Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "pod exists and start failed, should update status fields",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Spec: corev1.PodSpec{
						NodeName: "node-1",
					},
					Status: corev1.PodStatus{
						PodIP: "10.0.0.1",
						Conditions: []corev1.PodCondition{
							{
								Type:               corev1.PodReady,
								Status:             corev1.ConditionFalse,
								LastTransitionTime: metav1.Now(),
							},
						},
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name: "test-container",
								State: corev1.ContainerState{
									Waiting: &corev1.ContainerStateWaiting{
										Reason:  "CrashLoopBackOff",
										Message: "back-off 5m0s restarting failed",
									},
								},
								Ready:        false,
								RestartCount: 0,
								Image:        "nginx:latest",
								ImageID:      "docker-pullable://nginx@sha256:...",
								ContainerID:  "docker://abc123",
							},
						},
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Spec: agentsv1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									NodeName: "node-1",
								},
							},
						},
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: metav1.Now(),
							Reason:             agentsv1alpha1.SandboxReadyReasonStartContainerFailed,
							Message:            "back-off 5m0s restarting failed",
						},
					},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientBuilder().WithScheme(scheme).Build()
			if tt.args.Pod != nil {
				err := client.Create(context.TODO(), tt.args.Pod)
				if err != nil {
					t.Fatalf("create pod failed: %s", err.Error())
				}
			}
			control := &commonControl{
				Client:               client,
				recorder:             record.NewFakeRecorder(10),
				inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(client, inplaceupdate.DefaultGeneratePatchBodyFunc),
			}

			err := control.EnsureSandboxUpdated(context.TODO(), tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("EnsureSandboxUpdated() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.args.Pod == nil {
				if tt.args.NewStatus.Phase != agentsv1alpha1.SandboxFailed {
					t.Errorf("Expected sandbox phase to be Failed, got %v", tt.args.NewStatus.Phase)
				}
				if tt.args.NewStatus.Message != "Sandbox Pod Not Found" {
					t.Errorf("Expected message 'Sandbox Pod Not Found', got %v", tt.args.NewStatus.Message)
				}
			} else {
				if tt.args.NewStatus.NodeName != tt.args.Pod.Spec.NodeName {
					t.Errorf("Expected NodeName to be %s, got %s", tt.args.Pod.Spec.NodeName, tt.args.NewStatus.NodeName)
				}
				if tt.args.NewStatus.SandboxIp != tt.args.Pod.Status.PodIP {
					t.Errorf("Expected SandboxIp to be %s, got %s", tt.args.Pod.Status.PodIP, tt.args.NewStatus.SandboxIp)
				}
				if tt.args.NewStatus.PodInfo.PodIP != tt.args.Pod.Status.PodIP {
					t.Errorf("Expected PodInfo.PodIP to be %s, got %s", tt.args.Pod.Status.PodIP, tt.args.NewStatus.PodInfo.PodIP)
				}
				if tt.args.NewStatus.PodInfo.NodeName != tt.args.Pod.Spec.NodeName {
					t.Errorf("Expected PodInfo.NodeName to be %s, got %s", tt.args.Pod.Spec.NodeName, tt.args.NewStatus.PodInfo.NodeName)
				}
			}
		})
	}
}

func TestCommonControl_EnsureSandboxPaused(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name      string
		args      EnsureFuncArgs
		podExists bool
		wantErr   bool
	}{
		{
			name: "pod does not exist, should mark paused",
			args: EnsureFuncArgs{
				Pod: nil,
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionTrue,
							LastTransitionTime: metav1.Now(),
							Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
						},
					},
				},
			},
			podExists: false,
			wantErr:   false,
		},
		{
			name: "pod exists but being deleted, should wait",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "test-sandbox",
						Namespace:         "default",
						DeletionTimestamp: &metav1.Time{Time: metav1.Now().Time},
						Finalizers:        []string{"fake"},
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionTrue,
							LastTransitionTime: metav1.Now(),
							Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
						},
					},
				},
			},
			podExists: true,
			wantErr:   false,
		},
		{
			name: "pod exists, should delete it",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionTrue,
							LastTransitionTime: metav1.Now(),
							Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
						},
					},
				},
			},
			podExists: true,
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := []client.Object{}
			if tt.args.Pod != nil {
				objects = append(objects, tt.args.Pod)
			}

			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			control := &commonControl{
				Client:               client,
				recorder:             record.NewFakeRecorder(10),
				inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(client, inplaceupdate.DefaultGeneratePatchBodyFunc),
			}

			err := control.EnsureSandboxPaused(context.TODO(), tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("EnsureSandboxPaused() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			// Verify pod was deleted if it existed initially
			if tt.podExists && tt.args.Pod != nil && tt.args.Pod.DeletionTimestamp == nil {
				pod := &corev1.Pod{}
				err := client.Get(context.TODO(), types.NamespacedName{Name: tt.args.Pod.Name, Namespace: tt.args.Pod.Namespace}, pod)
				if err == nil && pod.DeletionTimestamp.IsZero() {
					t.Errorf("Expected pod to be deleted, but it still exists")
				}
			}
		})
	}
}

func TestCommonControl_EnsureSandboxResumed(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	now := metav1.Now()
	tests := []struct {
		name           string
		args           EnsureFuncArgs
		podExist       bool
		wantErr        bool
		expectedStatus *agentsv1alpha1.SandboxStatus
	}{
		{
			name: "pod does not exist, should create",
			args: EnsureFuncArgs{
				Pod: nil,
				Box: &agentsv1alpha1.Sandbox{
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
				NewStatus: &agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxResuming,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: now,
							Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
						},
					},
				},
			},
			podExist: false,
			wantErr:  false,
			expectedStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxResuming,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReady),
						Status:             metav1.ConditionFalse,
						LastTransitionTime: now,
						Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
					},
				},
			},
		},
		{
			name: "pod exists but not running",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodPending,
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxResuming,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: now,
							Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
						},
					},
				},
			},
			podExist: true,
			wantErr:  false,
			expectedStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxResuming,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReady),
						Status:             metav1.ConditionFalse,
						LastTransitionTime: now,
						Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
					},
				},
			},
		},
		{
			name: "pod is running and ready",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						Conditions: []corev1.PodCondition{
							{
								Type:               corev1.PodReady,
								Status:             corev1.ConditionTrue,
								LastTransitionTime: now,
							},
						},
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxResuming,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: now,
							Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
						},
					},
				},
			},
			podExist: true,
			wantErr:  false,
			expectedStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReady),
						Status:             metav1.ConditionTrue,
						LastTransitionTime: now,
						Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
					},
				},
			},
		},
		{
			name: "pod is running, but not ready",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						Conditions: []corev1.PodCondition{
							{
								Type:               corev1.PodReady,
								Status:             corev1.ConditionFalse,
								LastTransitionTime: now,
							},
						},
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxResuming,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionReady),
							Status:             metav1.ConditionFalse,
							LastTransitionTime: now,
							Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
						},
					},
				},
			},
			podExist: true,
			wantErr:  false,
			expectedStatus: &agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxResuming,
				Conditions: []metav1.Condition{
					{
						Type:               string(agentsv1alpha1.SandboxConditionReady),
						Status:             metav1.ConditionFalse,
						LastTransitionTime: now,
						Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientBuilder().WithScheme(scheme).Build()
			control := &commonControl{
				Client:               client,
				recorder:             record.NewFakeRecorder(10),
				inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(client, inplaceupdate.DefaultGeneratePatchBodyFunc),
			}

			err := control.EnsureSandboxResumed(context.TODO(), tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("EnsureSandboxResumed() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			// Verify that pod was created if it didn't exist
			if !tt.podExist && tt.args.Pod == nil {
				pod := &corev1.Pod{}
				err := client.Get(context.TODO(), types.NamespacedName{Name: tt.args.Box.Name, Namespace: tt.args.Box.Namespace}, pod)
				if err != nil {
					t.Errorf("Expected pod to be created, but it wasn't: %v", err)
				}
			}

			if !reflect.DeepEqual(tt.args.NewStatus, tt.expectedStatus) {
				t.Errorf("Expected sandbox(%s), got(%s)", utils.DumpJson(tt.expectedStatus), utils.DumpJson(tt.args.NewStatus))
			}
		})
	}
}

func TestCommonControl_EnsureSandboxTerminated(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name      string
		args      EnsureFuncArgs
		podExists bool
		wantErr   bool
	}{
		{
			name: "pod does not exist, should remove finalizer",
			args: EnsureFuncArgs{
				Pod: nil,
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "test-sandbox",
						Namespace:  "default",
						Finalizers: []string{utils.SandboxFinalizer},
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			podExists: false,
			wantErr:   false,
		},
		{
			name: "pod exists but being deleted, should wait",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "test-sandbox",
						Namespace:         "default",
						DeletionTimestamp: &metav1.Time{Time: metav1.Now().Time},
						Finalizers:        []string{"fake"},
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			podExists: true,
			wantErr:   false,
		},
		{
			name: "pod exists, should delete it",
			args: EnsureFuncArgs{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				Box: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
				},
				NewStatus: &agentsv1alpha1.SandboxStatus{},
			},
			podExists: true,
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := []client.Object{}
			if tt.args.Box != nil {
				objects = append(objects, tt.args.Box)
			}
			if tt.args.Pod != nil {
				objects = append(objects, tt.args.Pod)
			}

			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			control := &commonControl{
				Client:               client,
				recorder:             record.NewFakeRecorder(10),
				inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(client, inplaceupdate.DefaultGeneratePatchBodyFunc),
			}

			err := control.EnsureSandboxTerminated(context.TODO(), tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("EnsureSandboxTerminated() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			// Verify pod was deleted if it existed initially and wasn't already being deleted
			if tt.podExists && tt.args.Pod != nil && tt.args.Pod.DeletionTimestamp == nil {
				pod := &corev1.Pod{}
				err := client.Get(context.TODO(), types.NamespacedName{Name: tt.args.Pod.Name, Namespace: tt.args.Pod.Namespace}, pod)
				if err == nil && pod.DeletionTimestamp.IsZero() {
					t.Errorf("Expected pod to be deleted, but it still exists")
				}
			}
		})
	}
}

func TestCommonControl_createPod(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	control := &commonControl{
		Client:   fake.NewClientBuilder().WithScheme(scheme).Build(),
		recorder: record.NewFakeRecorder(10),
	}

	sandbox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels:      map[string]string{"app": "test"},
						Annotations: map[string]string{"annotation": "value"},
					},
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

	status := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "rev1",
	}

	pod, err := control.createPod(context.TODO(), sandbox, status)
	if err != nil {
		t.Fatalf("createPod() error = %v", err)
	}

	if pod.Name != sandbox.Name {
		t.Errorf("Expected pod name %s, got %s", sandbox.Name, pod.Name)
	}
	if pod.Namespace != sandbox.Namespace {
		t.Errorf("Expected pod namespace %s, got %s", sandbox.Namespace, pod.Namespace)
	}
	if pod.Labels[agentsv1alpha1.PodLabelTemplateHash] != status.UpdateRevision {
		t.Errorf("Expected pod label %s to be %s, got %s", agentsv1alpha1.PodLabelTemplateHash, status.UpdateRevision, pod.Labels[agentsv1alpha1.PodLabelTemplateHash])
	}
	if pod.Annotations[utils.PodAnnotationCreatedBy] != utils.CreatedBySandbox {
		t.Errorf("Expected pod annotation %s to be %s, got %s", utils.PodAnnotationCreatedBy, utils.CreatedBySandbox, pod.Annotations[utils.PodAnnotationCreatedBy])
	}
	if pod.Labels[utils.PodLabelCreatedBy] != utils.CreatedBySandbox {
		t.Errorf("Expected pod label %s to be %s, got %s", utils.PodLabelCreatedBy, utils.CreatedBySandbox, pod.Labels[utils.PodLabelCreatedBy])
	}

	sandboxWithPVC := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox-with-pvc",
			Namespace: "default",
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels:      map[string]string{"app": "test"},
						Annotations: map[string]string{"annotation": "value"},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:latest",
								VolumeMounts: []corev1.VolumeMount{
									{
										Name:      "www",
										MountPath: "/var/www",
									},
								},
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
	}

	podWithPVC, err := control.createPod(context.TODO(), sandboxWithPVC, status)
	if err != nil {
		t.Fatalf("createPod() with PVC error = %v", err)
	}

	expectedPVCName, err := GeneratePVCName("www", "test-sandbox-with-pvc")
	if err != nil {
		t.Fatalf("GeneratePVCName() error = %v", err)
	}

	if len(podWithPVC.Spec.Volumes) != 1 {
		t.Errorf("Expected 1 volume, got %d", len(podWithPVC.Spec.Volumes))
	} else {
		volume := podWithPVC.Spec.Volumes[0]
		if volume.Name != "www" {
			t.Errorf("Expected volume name to be 'www', got %s", volume.Name)
		}
		if volume.VolumeSource.PersistentVolumeClaim == nil {
			t.Error("Expected volume source to be PersistentVolumeClaim")
		} else if volume.VolumeSource.PersistentVolumeClaim.ClaimName != expectedPVCName {
			t.Errorf("Expected PVC claim name to be %s, got %s", expectedPVCName, volume.VolumeSource.PersistentVolumeClaim.ClaimName)
		}
	}

	if len(podWithPVC.Spec.Containers) != 1 {
		t.Errorf("Expected 1 container, got %d", len(podWithPVC.Spec.Containers))
	} else {
		container := podWithPVC.Spec.Containers[0]
		if len(container.VolumeMounts) != 1 {
			t.Errorf("Expected 1 volume mount, got %d", len(container.VolumeMounts))
		} else {
			volumeMount := container.VolumeMounts[0]
			if volumeMount.Name != "www" {
				t.Errorf("Expected volume mount name to be 'www', got %s", volumeMount.Name)
			}
			if volumeMount.MountPath != "/var/www" {
				t.Errorf("Expected volume mount path to be '/var/www', got %s", volumeMount.MountPath)
			}
		}
	}
}

func TestCommonControl_handleInplaceUpdateSandbox(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	control := &commonControl{
		Client:   fake.NewClientBuilder().WithScheme(scheme).Build(),
		recorder: record.NewFakeRecorder(10),
	}
	control.inplaceUpdateControl = inplaceupdate.NewInPlaceUpdateControl(control.Client, inplaceupdate.DefaultGeneratePatchBodyFunc)

	// Test case 1: Pod doesn't have template hash label
	sandbox1 := &agentsv1alpha1.Sandbox{
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
	}

	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels:    map[string]string{}, // No template hash label
		},
	}

	args1 := EnsureFuncArgs{
		Pod:       pod1,
		Box:       sandbox1,
		NewStatus: &agentsv1alpha1.SandboxStatus{},
	}

	done, err := control.handleInplaceUpdateSandbox(context.TODO(), args1)
	if err != nil {
		t.Fatalf("handleInplaceUpdateSandbox() error = %v", err)
	}
	if !done {
		t.Errorf("Expected done to be true when pod doesn't have template hash label")
	}

	// Test case 2: Hash mismatch
	sandbox2 := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.SandboxHashImmutablePart: "different-hash",
			},
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
	}

	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels:    map[string]string{agentsv1alpha1.PodLabelTemplateHash: "old-revision"},
		},
	}

	args2 := EnsureFuncArgs{
		Pod:       pod2,
		Box:       sandbox2,
		NewStatus: &agentsv1alpha1.SandboxStatus{UpdateRevision: "new-revision"},
	}

	done, err = control.handleInplaceUpdateSandbox(context.TODO(), args2)
	if err != nil {
		t.Fatalf("handleInplaceUpdateSandbox() error = %v", err)
	}
	if !done {
		t.Errorf("Expected done to be true when hash mismatch occurs")
	}

	// Test case 3: Revision consistent and inplace update completed
	sandbox3 := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.SandboxHashImmutablePart: "same-hash",
			},
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
	}

	pod3 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels:    map[string]string{agentsv1alpha1.PodLabelTemplateHash: "same-revision"},
		},
	}

	args3 := EnsureFuncArgs{
		Pod:       pod3,
		Box:       sandbox3,
		NewStatus: &agentsv1alpha1.SandboxStatus{UpdateRevision: "same-revision"},
	}

	done, err = control.handleInplaceUpdateSandbox(context.TODO(), args3)
	if err != nil {
		t.Fatalf("handleInplaceUpdateSandbox() error = %v", err)
	}
	if !done {
		t.Errorf("Expected done to be true when revision is consistent and inplace update is completed")
	}
}

func TestCommonControl_createPod_WithSidecarInjection(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name                    string
		sandbox                 *agentsv1alpha1.Sandbox
		configMap               *corev1.ConfigMap
		featureGateEnabled      bool
		expectInitContainers    int
		expectContainers        int
		expectMainContainerEnvs int
		expectMainVolumeMounts  int
		expectVolumes           int
		expectRuntimeSidecar    bool // expect runtime-sidecar in InitContainers
		expectCSISidecar        bool // expect csi-sidecar in InitContainers
	}{
		{
			name: "no injection - no annotations",
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
									{Name: "main", Image: "nginx"},
								},
							},
						},
					},
				},
			},
			configMap:               nil,
			featureGateEnabled:      true,
			expectInitContainers:    0,
			expectContainers:        1,
			expectMainContainerEnvs: 0,
			expectMainVolumeMounts:  0,
			expectVolumes:           0,
			expectRuntimeSidecar:    false,
			expectCSISidecar:        false,
		},
		{
			name: "runtime injection only",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime,
						},
					},
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx"},
								},
							},
						},
					},
				},
			},
			configMap: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sidecarutils.SandboxInjectionConfigName,
					Namespace: utils.DefaultSandboxDeployNamespace,
				},
				Data: map[string]string{
					sidecarutils.KEY_RUNTIME_INJECTION_CONFIG: `{
						"mainContainer": {
							"name": "",
							"env": [
								{"name": "RUNTIME_ENV", "value": "test"},
								{"name": "DEBUG", "value": "true"}
							],
							"volumeMounts": []
						},
						"csiSidecar": [{
							"name": "runtime-sidecar",
							"image": "runtime:v1"
						}],
						"volume": []
					}`,
				},
			},
			featureGateEnabled:      true,
			expectInitContainers:    1,
			expectContainers:        1,
			expectMainContainerEnvs: 2,
			expectMainVolumeMounts:  0,
			expectVolumes:           0,
			expectRuntimeSidecar:    true,
			expectCSISidecar:        false,
		},
		{
			name: "csi injection only",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount,
						},
					},
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx"},
								},
							},
						},
					},
				},
			},
			configMap: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sidecarutils.SandboxInjectionConfigName,
					Namespace: utils.DefaultSandboxDeployNamespace,
				},
				Data: map[string]string{
					sidecarutils.KEY_CSI_INJECTION_CONFIG: `{
						"mainContainer": {
							"name": "",
							"env": [],
							"volumeMounts": [
								{"name": "csi-volume", "mountPath": "/mnt/csi"},
								{"name": "data-volume", "mountPath": "/data"}
							]
						},
						"csiSidecar": [{
							"name": "csi-sidecar",
							"image": "csi:v1"
						}],
						"volume": [
							{"name": "csi-volume", "emptyDir": {}},
							{"name": "data-volume", "emptyDir": {}}
						]
					}`,
				},
			},
			featureGateEnabled:      true,
			expectInitContainers:    1, // CSI sidecar is injected to InitContainers
			expectContainers:        1, // only main container
			expectMainContainerEnvs: 0,
			expectMainVolumeMounts:  2,
			expectVolumes:           2,
			expectRuntimeSidecar:    false,
			expectCSISidecar:        true,
		},
		{
			name: "both runtime and csi injection",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime,
						},
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount,
						},
					},
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx"},
								},
							},
						},
					},
				},
			},
			configMap: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sidecarutils.SandboxInjectionConfigName,
					Namespace: utils.DefaultSandboxDeployNamespace,
				},
				Data: map[string]string{
					sidecarutils.KEY_RUNTIME_INJECTION_CONFIG: `{
						"mainContainer": {
							"name": "",
							"env": [{"name": "RUNTIME", "value": "enabled"}],
							"volumeMounts": []
						},
						"csiSidecar": [{
							"name": "runtime-sidecar",
							"image": "runtime:v1"
						}],
						"volume": []
					}`,
					sidecarutils.KEY_CSI_INJECTION_CONFIG: `{
						"mainContainer": {
							"name": "",
							"env": [],
							"volumeMounts": [{"name": "csi-vol", "mountPath": "/csi"}]
						},
						"csiSidecar": [{
							"name": "csi-sidecar",
							"image": "csi:v1"
						}],
						"volume": [{"name": "csi-vol", "emptyDir": {}}]
					}`,
				},
			},
			featureGateEnabled:      true,
			expectInitContainers:    2, // runtime-sidecar + csi-sidecar both injected to InitContainers
			expectContainers:        1, // only main container
			expectMainContainerEnvs: 1,
			expectMainVolumeMounts:  1,
			expectVolumes:           1,
			expectRuntimeSidecar:    true,
			expectCSISidecar:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Feature gate setup
			if tt.featureGateEnabled {
				_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodInjectConfigGate=true")
				t.Cleanup(func() {
					_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodInjectConfigGate=false")
				})
			} else {
				_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodInjectConfigGate=false")
			}

			// Build client with configmap if needed
			objs := []client.Object{tt.sandbox}
			if tt.configMap != nil {
				objs = append(objs, tt.configMap)
			}

			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			control := &commonControl{
				Client:               fakeClient,
				recorder:             record.NewFakeRecorder(10),
				inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
			}

			newStatus := &agentsv1alpha1.SandboxStatus{
				UpdateRevision: "test-revision",
			}

			// Call createPod
			pod, err := control.createPod(context.TODO(), tt.sandbox, newStatus)
			if err != nil {
				t.Fatalf("createPod() unexpected error: %v", err)
			}

			// Verify InitContainers
			if len(pod.Spec.InitContainers) != tt.expectInitContainers {
				t.Errorf("expected %d InitContainers, got %d", tt.expectInitContainers, len(pod.Spec.InitContainers))
			}

			// Verify Containers
			if len(pod.Spec.Containers) != tt.expectContainers {
				t.Errorf("expected %d Containers, got %d", tt.expectContainers, len(pod.Spec.Containers))
			}

			// Find main container
			var mainContainer *corev1.Container
			for i := range pod.Spec.Containers {
				if pod.Spec.Containers[i].Name == "main" {
					mainContainer = &pod.Spec.Containers[i]
					break
				}
			}

			if mainContainer == nil {
				t.Fatal("main container not found")
			}

			// Verify main container envs
			if len(mainContainer.Env) != tt.expectMainContainerEnvs {
				t.Errorf("expected %d envs in main container, got %d", tt.expectMainContainerEnvs, len(mainContainer.Env))
			}

			// Verify main container volume mounts
			if len(mainContainer.VolumeMounts) != tt.expectMainVolumeMounts {
				t.Errorf("expected %d volume mounts in main container, got %d", tt.expectMainVolumeMounts, len(mainContainer.VolumeMounts))
			}

			// Verify volumes
			if len(pod.Spec.Volumes) != tt.expectVolumes {
				t.Errorf("expected %d volumes, got %d", tt.expectVolumes, len(pod.Spec.Volumes))
			}

			// Verify runtime sidecar in InitContainers
			if tt.expectRuntimeSidecar {
				runtimeFound := false
				for _, ic := range pod.Spec.InitContainers {
					if ic.Name == "runtime-sidecar" {
						runtimeFound = true
						break
					}
				}
				if !runtimeFound {
					t.Error("runtime sidecar not found in InitContainers")
				}
			}

			// Verify csi sidecar in InitContainers
			if tt.expectCSISidecar {
				csiFound := false
				for _, ic := range pod.Spec.InitContainers {
					if ic.Name == "csi-sidecar" {
						csiFound = true
						break
					}
				}
				if !csiFound {
					t.Error("csi sidecar not found in InitContainers")
				}
			}
		})
	}
}

func TestNewCommonControl(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	recorder := record.NewFakeRecorder(10)
	rl := NewRateLimiter()

	control := NewCommonControl(fakeClient, recorder, rl)
	if control == nil {
		t.Fatal("NewCommonControl returned nil")
	}

	// Verify it implements SandboxControl interface
	var _ SandboxControl = control

	// Verify internal fields by calling methods (smoke test)
	args := EnsureFuncArgs{
		Pod: nil,
		Box: &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-sandbox",
				Namespace: "default",
			},
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "test", Image: "nginx"},
							},
						},
					},
				},
			},
		},
		NewStatus: &agentsv1alpha1.SandboxStatus{},
	}

	// EnsureSandboxUpdated with nil pod should set Failed phase
	err := control.EnsureSandboxUpdated(context.TODO(), args)
	if err != nil {
		t.Errorf("EnsureSandboxUpdated() unexpected error: %v", err)
	}
	if args.NewStatus.Phase != agentsv1alpha1.SandboxFailed {
		t.Errorf("Expected phase Failed, got %s", args.NewStatus.Phase)
	}
}

func TestCommonControl_EnsureSandboxUpdated_InplaceNotDone(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.SandboxHashImmutablePart: "hash-a",
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "test", Image: "nginx:v2"}},
					},
				},
			},
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-rev",
			},
		},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.2",
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	control := &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-rev",
	}

	err := control.EnsureSandboxUpdated(context.TODO(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Fatalf("EnsureSandboxUpdated() unexpected error: %v", err)
	}
	// handleInplaceUpdateSandbox with hash mismatch returns (true, nil), so status gets synced
	if newStatus.SandboxIp != "10.0.0.2" {
		t.Errorf("Expected SandboxIp '10.0.0.2', got %s", newStatus.SandboxIp)
	}
}

func TestCommonControl_EnsureSandboxResumed_TerminatingPod(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	now := metav1.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-sandbox",
			Namespace:         "default",
			DeletionTimestamp: &metav1.Time{Time: now.Time},
			Finalizers:        []string{"fake-finalizer"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	control := &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		Phase: agentsv1alpha1.SandboxResuming,
	}

	err := control.EnsureSandboxResumed(context.TODO(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err == nil {
		t.Error("Expected error for terminating pod, got nil")
	}
}

func TestCommonControl_EnsureSandboxResumed_SetResumedCondition(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	now := metav1.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	control := &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		Phase: agentsv1alpha1.SandboxResuming,
		Conditions: []metav1.Condition{
			{
				Type:               string(agentsv1alpha1.SandboxConditionResumed),
				Status:             metav1.ConditionFalse,
				LastTransitionTime: now,
				Reason:             agentsv1alpha1.SandboxResumeReasonCreatePod,
			},
			{
				Type:               string(agentsv1alpha1.SandboxConditionReady),
				Status:             metav1.ConditionFalse,
				LastTransitionTime: now,
				Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
			},
		},
	}

	err := control.EnsureSandboxResumed(context.TODO(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Fatalf("EnsureSandboxResumed() unexpected error: %v", err)
	}

	resumedCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionResumed))
	if resumedCond == nil {
		t.Fatal("Expected Resumed condition to exist")
	}
	if resumedCond.Status != metav1.ConditionTrue {
		t.Errorf("Expected Resumed condition to be True, got %s", resumedCond.Status)
	}

	if newStatus.Phase != agentsv1alpha1.SandboxResuming {
		t.Errorf("Expected phase Resuming, got %s", newStatus.Phase)
	}
}

func TestCommonControl_EnsureSandboxTerminated_PodNotExist_NoFinalizer(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(box).Build()
	control := &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
	}

	err := control.EnsureSandboxTerminated(context.TODO(), EnsureFuncArgs{Pod: nil, Box: box, NewStatus: &agentsv1alpha1.SandboxStatus{}})
	if err != nil {
		t.Errorf("EnsureSandboxTerminated() unexpected error: %v", err)
	}
}

func TestCommonControl_EnsureSandboxPaused_AlreadyPaused(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	now := metav1.Now()
	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	control := &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		Phase: agentsv1alpha1.SandboxPaused,
		Conditions: []metav1.Condition{
			{
				Type:               string(agentsv1alpha1.SandboxConditionPaused),
				Status:             metav1.ConditionTrue,
				Reason:             agentsv1alpha1.SandboxPausedReasonDeletePod,
				LastTransitionTime: now,
			},
		},
	}

	err := control.EnsureSandboxPaused(context.TODO(), EnsureFuncArgs{Pod: nil, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Errorf("EnsureSandboxPaused() unexpected error: %v", err)
	}

	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Error("Expected Paused condition to remain True")
	}
}

func TestCommonControl_performRecreateUpgrade_PodTerminating(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	now := metav1.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-sandbox",
			Namespace:         "default",
			DeletionTimestamp: &metav1.Time{Time: now.Time},
			Finalizers:        []string{"fake"},
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-rev",
			},
		},
	}

	box := &agentsv1alpha1.Sandbox{
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

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	control := &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-rev",
	}

	done, err := control.performRecreateUpgrade(context.TODO(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Fatalf("performRecreateUpgrade() unexpected error: %v", err)
	}
	if done {
		t.Error("Expected done=false for terminating pod")
	}
}

func TestCommonControl_performRecreateUpgrade_NewPodNotReady(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "new-rev",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	box := &agentsv1alpha1.Sandbox{
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

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	control := &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-rev",
	}

	done, err := control.performRecreateUpgrade(context.TODO(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Fatalf("performRecreateUpgrade() unexpected error: %v", err)
	}
	if done {
		t.Error("Expected done=false for pod not ready")
	}
}

func TestCommonControl_performRecreateUpgrade_PodReadyFalse(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "new-rev",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
		},
	}

	box := &agentsv1alpha1.Sandbox{
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

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	control := &commonControl{
		Client:               fakeClient,
		recorder:             record.NewFakeRecorder(10),
		inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(fakeClient, inplaceupdate.DefaultGeneratePatchBodyFunc),
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-rev",
	}

	done, err := control.performRecreateUpgrade(context.TODO(), EnsureFuncArgs{Pod: pod, Box: box, NewStatus: newStatus})
	if err != nil {
		t.Fatalf("performRecreateUpgrade() unexpected error: %v", err)
	}
	if done {
		t.Error("Expected done=false for pod with Ready=False")
	}
}
