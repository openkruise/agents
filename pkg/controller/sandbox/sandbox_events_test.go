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

package sandbox

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/events"
	"github.com/openkruise/agents/pkg/controller/sandbox/core"
)

func TestSandboxLifecycleEvents(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name         string
		sandbox      *agentsv1alpha1.Sandbox
		pod          *corev1.Pod
		expectEvents []string
	}{
		{
			name: "sandbox pending with no pod emits SandboxPodCreated event on pod creation",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "create-pod-sandbox",
					Namespace:  "default",
					Generation: 1,
					Finalizers: []string{"agents.kruise.io/sandbox-protection"},
					Annotations: map[string]string{
						agentsv1alpha1.SandboxHashImmutablePart: "fakehash",
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
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPending,
				},
			},
			pod:          nil,
			expectEvents: []string{events.SandboxPodCreated},
		},
		{
			name: "sandbox pending with running pod emits SandboxRunning event",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "running-sandbox",
					Namespace:  "default",
					Generation: 1,
					Finalizers: []string{"agents.kruise.io/sandbox-protection"},
					Annotations: map[string]string{
						agentsv1alpha1.SandboxHashImmutablePart: "fakehash",
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
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPending,
				},
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "running-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			expectEvents: []string{events.SandboxReady},
		},
		{
			name: "running sandbox with deleted pod emits SandboxFailed event",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "failed-sandbox",
					Namespace:  "default",
					Generation: 1,
					Finalizers: []string{"agents.kruise.io/sandbox-protection"},
					Annotations: map[string]string{
						agentsv1alpha1.SandboxHashImmutablePart: "fakehash",
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
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			pod:          nil, // pod is missing
			expectEvents: []string{events.SandboxFailed},
		},
		{
			name: "paused sandbox with pod deleted emits SandboxPaused event",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "paused-sandbox",
					Namespace:  "default",
					Generation: 1,
					Finalizers: []string{"agents.kruise.io/sandbox-protection"},
					Annotations: map[string]string{
						agentsv1alpha1.SandboxHashImmutablePart: "fakehash",
					},
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
					Phase: agentsv1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{
							Type:               string(agentsv1alpha1.SandboxConditionPaused),
							Status:             metav1.ConditionFalse,
							Reason:             "DeletePod",
							LastTransitionTime: metav1.Now(),
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "1.2.3.4",
					},
				},
			},
			pod:          nil, // Pod already deleted
			expectEvents: []string{events.SandboxPaused},
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
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&agentsv1alpha1.Sandbox{}).
				WithObjects(objects...).
				Build()
			rl := core.NewRateLimiter()
			reconciler := &SandboxReconciler{
				Client:   fakeClient,
				Scheme:   scheme,
				recorder: fakeRecorder,
				controls: core.NewSandboxControl(core.SandboxControlArgs{
					Client:      fakeClient,
					Recorder:    fakeRecorder,
					RateLimiter: rl,
				}),
				rateLimiter: rl,
			}

			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      tt.sandbox.Name,
					Namespace: tt.sandbox.Namespace,
				},
			}

			_, _ = reconciler.Reconcile(context.Background(), req)

			// Collect all emitted events
			close(fakeRecorder.Events)
			var gotEvents []string
			for e := range fakeRecorder.Events {
				gotEvents = append(gotEvents, e)
			}

			// Verify expected events
			for _, expected := range tt.expectEvents {
				found := false
				for _, got := range gotEvents {
					if strings.Contains(got, expected) {
						found = true
						break
					}
				}
				assert.True(t, found, "expected event containing %q not found in %v", expected, gotEvents)
			}
		})
	}
}
