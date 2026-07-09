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

package core

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

// simplePodGenFunc is a minimal PodGenerateFunc for testing that returns a
// basic pod without requiring a full sandbox template.
func simplePodGenFunc(_ context.Context, args PodGenerateArgs) (*corev1.Pod, error) {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      args.Box.Name,
			Namespace: args.Box.Namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "nginx:latest"},
			},
		},
	}, nil
}

func TestCreatePod(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	baseBox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
	}

	tests := []struct {
		name          string
		createErr     error // error returned by the fake client Create; nil means success
		newStatus     *agentsv1alpha1.SandboxStatus
		expectError   string
		expectPod     bool   // whether a non-nil pod should be returned
		expectEvent   bool   // whether a Warning event should be emitted
		expectCond    bool   // whether a Ready=False condition should be set
		expectReason  string // expected condition reason
		expectMessage string // expected substring in condition message and event
	}{
		{
			name:        "create succeeds - no event, no condition",
			createErr:   nil,
			newStatus:   &agentsv1alpha1.SandboxStatus{},
			expectError: "",
			expectPod:   true,
			expectEvent: false,
			expectCond:  false,
		},
		{
			name:          "create fails with generic error - emits event and sets condition",
			createErr:     fmt.Errorf("pvc test-pvc is invalid"),
			newStatus:     &agentsv1alpha1.SandboxStatus{},
			expectError:   "pvc test-pvc is invalid",
			expectPod:     false,
			expectEvent:   true,
			expectCond:    true,
			expectReason:  agentsv1alpha1.SandboxReadyReasonPodCreateFailed,
			expectMessage: "pvc test-pvc is invalid",
		},
		{
			name:        "create fails with AlreadyExists - no event, no condition, returns pod",
			createErr:   apierrors.NewAlreadyExists(schema.GroupResource{Resource: "pods"}, "test-sandbox"),
			newStatus:   &agentsv1alpha1.SandboxStatus{},
			expectError: "",
			expectPod:   true,
			expectEvent: false,
			expectCond:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fc client.Client
			if tt.createErr != nil {
				fc = fake.NewClientBuilder().WithScheme(scheme).
					WithInterceptorFuncs(interceptor.Funcs{
						Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
							return tt.createErr
						},
					}).Build()
			} else {
				fc = fake.NewClientBuilder().WithScheme(scheme).Build()
			}

			recorder := record.NewFakeRecorder(10)
			podControl := NewPodControl(fc, recorder, simplePodGenFunc)

			box := baseBox.DeepCopy()
			args := CreatePodArgs{
				Box:       box,
				NewStatus: tt.newStatus,
			}

			pod, err := podControl.CreatePod(context.TODO(), args)

			// Error assertion
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
			}

			// Pod assertion
			if tt.expectPod {
				assert.NotNil(t, pod)
			} else {
				assert.Nil(t, pod)
			}

			// Event assertion
			if tt.expectEvent {
				select {
				case event := <-recorder.Events:
					assert.Contains(t, event, "PodCreateFailed")
					if tt.expectMessage != "" {
						assert.Contains(t, event, tt.expectMessage)
					}
				default:
					t.Error("expected a Warning event to be recorded")
				}
			} else {
				select {
				case <-recorder.Events:
					t.Error("did not expect any event to be recorded")
				default:
					// expected - no event
				}
			}

			// Condition assertion
			if tt.expectCond {
				require.NotNil(t, tt.newStatus, "NewStatus should not be nil when expecting condition")
				cond := utils.GetSandboxCondition(tt.newStatus, string(agentsv1alpha1.SandboxConditionReady))
				require.NotNil(t, cond, "expected Ready condition to be set")
				assert.Equal(t, metav1.ConditionFalse, cond.Status)
				assert.Equal(t, tt.expectReason, cond.Reason)
				if tt.expectMessage != "" {
					assert.Contains(t, cond.Message, tt.expectMessage)
				}
			} else {
				cond := utils.GetSandboxCondition(tt.newStatus, string(agentsv1alpha1.SandboxConditionReady))
				assert.Nil(t, cond, "did not expect Ready condition to be set")
			}
		})
	}
}
