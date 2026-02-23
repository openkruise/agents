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

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/inplaceupdate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCommonControl_EnsureSandboxRunning(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name     string
		args     EnsureFuncArgs
		podExist bool
		wantErr  bool
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientBuilder().WithScheme(scheme).Build()
			control := &commonControl{
				Client:               client,
				recorder:             record.NewFakeRecorder(10),
				inplaceUpdateControl: inplaceupdate.NewInPlaceUpdateControl(client, inplaceupdate.DefaultGeneratePatchBodyFunc),
			}

			err := control.EnsureSandboxRunning(context.TODO(), tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("EnsureSandboxRunning() error = %v, wantErr %v", err, tt.wantErr)
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
				agentsv1alpha1.SandboxHashWithoutImageAndResources: "different-hash",
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
				agentsv1alpha1.SandboxHashWithoutImageAndResources: "same-hash",
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
