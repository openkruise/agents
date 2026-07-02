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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
	"github.com/openkruise/agents/pkg/utils/fieldindex"
)

func TestValidateContainerImages(t *testing.T) {
	tests := []struct {
		name        string
		pod         *corev1.Pod
		box         *agentsv1alpha1.Sandbox
		expectError string
	}{
		{
			name: "images match - no error",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
						{Name: "sidecar", Image: "envoy:1.20"},
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx:1.21"},
								},
							},
						},
					},
				},
			},
			expectError: "",
		},
		{
			name: "image changed - returns error",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.22"},
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx:1.21"},
								},
							},
						},
					},
				},
			},
			expectError: "container \"main\" image changed",
		},
		{
			name: "template is nil - no error",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: nil,
					},
				},
			},
			expectError: "",
		},
		{
			name: "sidecar init container image match - no error",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
					},
					InitContainers: []corev1.Container{
						{Name: "sidecar", Image: "envoy:1.20", RestartPolicy: func() *corev1.ContainerRestartPolicy { p := corev1.ContainerRestartPolicyAlways; return &p }()},
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx:1.21"},
								},
								InitContainers: []corev1.Container{
									{Name: "sidecar", Image: "envoy:1.20", RestartPolicy: func() *corev1.ContainerRestartPolicy { p := corev1.ContainerRestartPolicyAlways; return &p }()},
								},
							},
						},
					},
				},
			},
			expectError: "",
		},
		{
			name: "sidecar init container image changed - returns error",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
					},
					InitContainers: []corev1.Container{
						{Name: "sidecar", Image: "envoy:1.22", RestartPolicy: func() *corev1.ContainerRestartPolicy { p := corev1.ContainerRestartPolicyAlways; return &p }()},
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx:1.21"},
								},
								InitContainers: []corev1.Container{
									{Name: "sidecar", Image: "envoy:1.20", RestartPolicy: func() *corev1.ContainerRestartPolicy { p := corev1.ContainerRestartPolicyAlways; return &p }()},
								},
							},
						},
					},
				},
			},
			expectError: "sidecar init container \"sidecar\" image changed",
		},
		{
			name: "non-sidecar init container image changed - no error",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
					},
					InitContainers: []corev1.Container{
						{Name: "init", Image: "busybox:1.36"},
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx:1.21"},
								},
								InitContainers: []corev1.Container{
									{Name: "init", Image: "busybox:1.35"},
								},
							},
						},
					},
				},
			},
			expectError: "",
		},
		{
			name: "extra containers in pod not in template - no error",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:1.21"},
						{Name: "istio-proxy", Image: "istio/proxyv2:1.20"},
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "nginx:1.21"},
								},
							},
						},
					},
				},
			},
			expectError: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateContainerImages(tt.pod, tt.box)
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
		})
	}
}

func TestListCheckpointsForSandbox(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	sandboxUID := types.UID("sandbox-uid-123")
	otherUID := types.UID("other-uid-456")
	ownerRef := metav1.OwnerReference{
		APIVersion: agentsv1alpha1.GroupVersion.String(),
		Kind:       "Sandbox",
		Name:       "test-sandbox",
		UID:        sandboxUID,
		Controller: func() *bool { v := true; return &v }(),
	}

	tests := []struct {
		name            string
		checkpoints     []agentsv1alpha1.Checkpoint
		sandboxUID      types.UID
		expectEmpty     bool
		expectError     string
		expectedCPName  string
		expectedCPCount int
	}{
		{
			name:        "no checkpoints found - returns nil",
			checkpoints: nil,
			sandboxUID:  sandboxUID,
			expectEmpty: true,
			expectError: "",
		},
		{
			name: "single checkpoint found",
			checkpoints: []agentsv1alpha1.Checkpoint{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "test-sandbox-abc",
						Namespace:         "default",
						CreationTimestamp: metav1.Now(),
						OwnerReferences:   []metav1.OwnerReference{ownerRef},
						Labels: map[string]string{
							agentsv1alpha1.CheckpointLabelSandboxName: "test-sandbox",
							agentsv1alpha1.CheckpointLabelType:        agentsv1alpha1.CheckpointTypePodInfo,
						},
					},
					Status: agentsv1alpha1.CheckpointStatus{
						Phase: agentsv1alpha1.CheckpointSucceeded,
					},
				},
			},
			sandboxUID:      sandboxUID,
			expectEmpty:     false,
			expectError:     "",
			expectedCPName:  "test-sandbox-abc",
			expectedCPCount: 1,
		},
		{
			name: "multiple checkpoints - newest first",
			checkpoints: []agentsv1alpha1.Checkpoint{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "test-sandbox-old",
						Namespace:         "default",
						CreationTimestamp: metav1.NewTime(metav1.Now().Add(-10 * 60 * 1e9)),
						OwnerReferences:   []metav1.OwnerReference{ownerRef},
						Labels: map[string]string{
							agentsv1alpha1.CheckpointLabelSandboxName: "test-sandbox",
							agentsv1alpha1.CheckpointLabelType:        agentsv1alpha1.CheckpointTypePodInfo,
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "test-sandbox-new",
						Namespace:         "default",
						CreationTimestamp: metav1.Now(),
						OwnerReferences:   []metav1.OwnerReference{ownerRef},
						Labels: map[string]string{
							agentsv1alpha1.CheckpointLabelSandboxName: "test-sandbox",
							agentsv1alpha1.CheckpointLabelType:        agentsv1alpha1.CheckpointTypePodInfo,
						},
					},
				},
			},
			sandboxUID:      sandboxUID,
			expectEmpty:     false,
			expectError:     "",
			expectedCPName:  "test-sandbox-new",
			expectedCPCount: 2,
		},
		{
			name: "checkpoint for different sandbox - not found",
			checkpoints: []agentsv1alpha1.Checkpoint{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "other-sandbox-cp",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: agentsv1alpha1.GroupVersion.String(),
								Kind:       "Sandbox",
								Name:       "other-sandbox",
								UID:        otherUID,
								Controller: func() *bool { v := true; return &v }(),
							},
						},
						Labels: map[string]string{
							agentsv1alpha1.CheckpointLabelSandboxName: "other-sandbox",
							agentsv1alpha1.CheckpointLabelType:        agentsv1alpha1.CheckpointTypePodInfo,
						},
					},
				},
			},
			sandboxUID:  sandboxUID,
			expectEmpty: true,
			expectError: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme).
				WithIndex(&agentsv1alpha1.Checkpoint{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc)
			for i := range tt.checkpoints {
				builder = builder.WithObjects(&tt.checkpoints[i])
			}
			cli := builder.Build()

			box := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					UID:       tt.sandboxUID,
				},
			}
			cpList, err := listCheckpointsForSandbox(context.TODO(), cli, box)
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
			if tt.expectEmpty {
				assert.Empty(t, cpList)
			} else {
				assert.Len(t, cpList, tt.expectedCPCount)
				if tt.expectedCPName != "" {
					assert.Equal(t, tt.expectedCPName, cpList[0].Name)
				}
			}
		})
	}
}

func newCheckpointTestControl(objs ...client.Object) (*CheckpointControl, client.Client) {
	ctrl, cli, _ := newCheckpointTestControlWithRecorder(objs...)
	return ctrl, cli
}

func newCheckpointTestControlWithRecorder(objs ...client.Object) (*CheckpointControl, client.Client, *record.FakeRecorder) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	builder := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&agentsv1alpha1.Checkpoint{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc).
		WithStatusSubresource(&agentsv1alpha1.Checkpoint{})
	for _, o := range objs {
		builder = builder.WithObjects(o)
	}
	cli := builder.Build()
	recorder := record.NewFakeRecorder(10)
	return NewCheckpointControl(cli, recorder), cli, recorder
}

func newCheckpointTestSandbox() *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			UID:       types.UID("sandbox-uid-001"),
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "main", Image: "nginx:1.21"},
						},
					},
				},
			},
		},
	}
}

func newCheckpointTestPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			UID:       types.UID("pod-uid-001"),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "nginx:1.21"},
			},
		},
	}
}

func newCheckpointTestCP(name string, box *agentsv1alpha1.Sandbox, phase agentsv1alpha1.CheckpointPhase) *agentsv1alpha1.Checkpoint {
	return &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: box.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(box, sandboxControllerKind),
			},
			Labels: map[string]string{
				agentsv1alpha1.CheckpointLabelSandboxName: box.Name,
				agentsv1alpha1.CheckpointLabelType:        agentsv1alpha1.CheckpointTypePodInfo,
			},
		},
		Status: agentsv1alpha1.CheckpointStatus{
			Phase: phase,
		},
	}
}

func enableCheckpointGate(t *testing.T) {
	t.Helper()
	_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxPauseCheckpoint=true")
	t.Cleanup(func() {
		_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxPauseCheckpoint=false")
	})
}

func TestAssumePodCheckpointed(t *testing.T) {
	tests := []struct {
		name         string
		enableGate   bool
		pod          *corev1.Pod
		box          *agentsv1alpha1.Sandbox
		existingCPs  []client.Object
		condReason   string
		expectWait   bool
		expectReason string
	}{
		{
			name:       "feature gate disabled - returns false immediately",
			enableGate: false,
			pod:        newCheckpointTestPod(),
			box:        newCheckpointTestSandbox(),
			expectWait: false,
		},
		{
			name:         "non-checkpoint reason - returns false immediately",
			enableGate:   true,
			pod:          newCheckpointTestPod(),
			box:          newCheckpointTestSandbox(),
			condReason:   agentsv1alpha1.SandboxPausedReasonDeletePod,
			expectWait:   false,
			expectReason: agentsv1alpha1.SandboxPausedReasonDeletePod,
		},
		{
			name:       "image changed - pause rejected",
			enableGate: true,
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default", UID: "pod-uid-001"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "nginx:1.22"}},
				},
			},
			box:          newCheckpointTestSandbox(),
			expectWait:   true,
			expectReason: agentsv1alpha1.SandboxPausedReasonImageChanged,
		},
		{
			name:       "image changed retry from ImageChanged reason",
			enableGate: true,
			condReason: agentsv1alpha1.SandboxPausedReasonImageChanged,
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default", UID: "pod-uid-001"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "nginx:1.22"}},
				},
			},
			box:          newCheckpointTestSandbox(),
			expectWait:   true,
			expectReason: agentsv1alpha1.SandboxPausedReasonImageChanged,
		},
		{
			name:         "fresh pause - creates checkpoint",
			enableGate:   true,
			pod:          newCheckpointTestPod(),
			box:          newCheckpointTestSandbox(),
			expectWait:   true,
			expectReason: agentsv1alpha1.SandboxPausedReasonCheckpointCreating,
		},
		{
			name:       "checkpoint succeeded - returns false",
			enableGate: true,
			pod:        newCheckpointTestPod(),
			box:        newCheckpointTestSandbox(),
			existingCPs: []client.Object{
				newCheckpointTestCP("test-sandbox-cp1", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointSucceeded),
			},
			condReason:   agentsv1alpha1.SandboxPausedReasonCheckpointCreating,
			expectWait:   false,
			expectReason: agentsv1alpha1.SandboxPausedReasonCheckpointSucceeded,
		},
		{
			name:       "checkpoint failed - returns true",
			enableGate: true,
			pod:        newCheckpointTestPod(),
			box:        newCheckpointTestSandbox(),
			existingCPs: []client.Object{
				newCheckpointTestCP("test-sandbox-cp1", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointFailed),
			},
			condReason:   agentsv1alpha1.SandboxPausedReasonCheckpointCreating,
			expectWait:   true,
			expectReason: agentsv1alpha1.SandboxPausedReasonCheckpointFailed,
		},
		{
			name:         "checkpoint failed reason - retry creates checkpoint",
			enableGate:   true,
			pod:          newCheckpointTestPod(),
			box:          newCheckpointTestSandbox(),
			condReason:   agentsv1alpha1.SandboxPausedReasonCheckpointFailed,
			expectWait:   true,
			expectReason: agentsv1alpha1.SandboxPausedReasonCheckpointFailed,
		},
		{
			name:       "checkpoint in progress - waits",
			enableGate: true,
			pod:        newCheckpointTestPod(),
			box:        newCheckpointTestSandbox(),
			existingCPs: []client.Object{
				newCheckpointTestCP("test-sandbox-cp1", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointCreating),
			},
			condReason:   agentsv1alpha1.SandboxPausedReasonCheckpointCreating,
			expectWait:   true,
			expectReason: agentsv1alpha1.SandboxPausedReasonCheckpointCreating,
		},
		{
			name:       "existing succeeded checkpoint with fresh entry - returns false",
			enableGate: true,
			pod:        newCheckpointTestPod(),
			box:        newCheckpointTestSandbox(),
			existingCPs: []client.Object{
				newCheckpointTestCP("test-sandbox-stale", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointSucceeded),
			},
			expectWait:   false,
			expectReason: agentsv1alpha1.SandboxPausedReasonCheckpointSucceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.enableGate {
				enableCheckpointGate(t)
			}
			ctrl, _ := newCheckpointTestControl(tt.existingCPs...)
			newStatus := &agentsv1alpha1.SandboxStatus{}
			cond := &metav1.Condition{
				Type:               string(agentsv1alpha1.SandboxConditionPaused),
				Status:             metav1.ConditionFalse,
				Reason:             tt.condReason,
				LastTransitionTime: metav1.Now(),
			}

			wait := ctrl.AssumePodCheckpointed(context.TODO(), tt.pod, tt.box, newStatus, cond)
			assert.Equal(t, tt.expectWait, wait)
			if tt.expectReason != "" {
				assert.Equal(t, tt.expectReason, cond.Reason)
			}
		})
	}
}

func TestGetPodTemplateDelta(t *testing.T) {
	tests := []struct {
		name       string
		enableGate bool
		existingCP *agentsv1alpha1.Checkpoint
		expectNil  bool
	}{
		{
			name:       "feature gate disabled - returns nil",
			enableGate: false,
			expectNil:  true,
		},
		{
			name:       "no checkpoints - returns nil",
			enableGate: true,
			expectNil:  true,
		},
		{
			name:       "checkpoint with delta - returns delta",
			enableGate: true,
			existingCP: func() *agentsv1alpha1.Checkpoint {
				box := newCheckpointTestSandbox()
				cp := newCheckpointTestCP("test-sandbox-cp1", box, agentsv1alpha1.CheckpointSucceeded)
				cp.Status.PodTemplateDelta = runtime.RawExtension{Raw: []byte(`{"spec":{"containers":[]}}`)}
				return cp
			}(),
			expectNil: false,
		},
		{
			name:       "checkpoint with empty delta - returns nil",
			enableGate: true,
			existingCP: func() *agentsv1alpha1.Checkpoint {
				box := newCheckpointTestSandbox()
				return newCheckpointTestCP("test-sandbox-cp1", box, agentsv1alpha1.CheckpointSucceeded)
			}(),
			expectNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.enableGate {
				enableCheckpointGate(t)
			}
			var objs []client.Object
			if tt.existingCP != nil {
				objs = append(objs, tt.existingCP)
			}
			ctrl, _ := newCheckpointTestControl(objs...)
			box := newCheckpointTestSandbox()
			delta := ctrl.GetPodTemplateDelta(context.TODO(), box)
			if tt.expectNil {
				assert.Nil(t, delta)
			} else {
				assert.NotNil(t, delta)
				assert.NotEmpty(t, delta.Raw)
			}
		})
	}
}

func TestCleanup(t *testing.T) {
	tests := []struct {
		name       string
		enableGate bool
		cpCount    int
	}{
		{
			name:       "feature gate disabled - no deletion",
			enableGate: false,
			cpCount:    2,
		},
		{
			name:       "feature gate enabled - deletes all checkpoints",
			enableGate: true,
			cpCount:    2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.enableGate {
				enableCheckpointGate(t)
			}
			box := newCheckpointTestSandbox()
			var objs []client.Object
			for i := 0; i < tt.cpCount; i++ {
				objs = append(objs, newCheckpointTestCP(
					box.Name+"-cp"+string(rune('0'+i)), box, agentsv1alpha1.CheckpointSucceeded,
				))
			}
			ctrl, cli := newCheckpointTestControl(objs...)

			ctrl.Cleanup(context.TODO(), box)

			remaining := &agentsv1alpha1.CheckpointList{}
			_ = cli.List(context.TODO(), remaining, client.InNamespace(box.Namespace))
			if tt.enableGate {
				assert.Empty(t, remaining.Items)
			} else {
				assert.Len(t, remaining.Items, tt.cpCount)
			}
		})
	}
}

func TestCreateCheckpoint(t *testing.T) {
	enableCheckpointGate(t)
	box := newCheckpointTestSandbox()
	ctrl, cli, recorder := newCheckpointTestControlWithRecorder()

	err := ctrl.createCheckpoint(context.TODO(), box)
	assert.NoError(t, err)

	cpList := &agentsv1alpha1.CheckpointList{}
	err = cli.List(context.TODO(), cpList, client.InNamespace(box.Namespace))
	assert.NoError(t, err)
	assert.Len(t, cpList.Items, 1)

	cp := cpList.Items[0]
	assert.Equal(t, box.Name, *cp.Spec.SandboxName)
	assert.Nil(t, cp.Spec.PodName)
	assert.Equal(t, box.Name, cp.Labels[agentsv1alpha1.CheckpointLabelSandboxName])
	assert.Equal(t, agentsv1alpha1.CheckpointTypePodInfo, cp.Labels[agentsv1alpha1.CheckpointLabelType])
	assert.Len(t, cp.OwnerReferences, 1)
	assert.Equal(t, box.Name, cp.OwnerReferences[0].Name)
	assertCheckpointRecorderEvent(t, recorder, corev1.EventTypeNormal+" "+EventCheckpointStarted, "created, waiting for completion")
}

func TestAssumePodCheckpointedRecordsCheckpointSuccessEvent(t *testing.T) {
	tests := []struct {
		name           string
		checkpoint     *agentsv1alpha1.Checkpoint
		expectPrefix   string
		expectContains string
	}{
		{
			name:           "checkpoint succeeded records normal event",
			checkpoint:     newCheckpointTestCP("test-sandbox-cp1", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointSucceeded),
			expectPrefix:   corev1.EventTypeNormal + " " + EventCheckpointSucceeded,
			expectContains: "Checkpoint test-sandbox-cp1 succeeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enableCheckpointGate(t)
			box := newCheckpointTestSandbox()
			ctrl, _, recorder := newCheckpointTestControlWithRecorder(tt.checkpoint)
			newStatus := &agentsv1alpha1.SandboxStatus{}
			cond := &metav1.Condition{
				Type:               string(agentsv1alpha1.SandboxConditionPaused),
				Status:             metav1.ConditionFalse,
				Reason:             agentsv1alpha1.SandboxPausedReasonCheckpointCreating,
				LastTransitionTime: metav1.Now(),
			}

			wait := ctrl.AssumePodCheckpointed(context.TODO(), newCheckpointTestPod(), box, newStatus, cond)

			assert.False(t, wait)
			assert.Equal(t, agentsv1alpha1.SandboxPausedReasonCheckpointSucceeded, cond.Reason)
			assertCheckpointRecorderEvent(t, recorder, tt.expectPrefix, tt.expectContains)
		})
	}
}

func assertCheckpointRecorderEvent(t *testing.T, recorder *record.FakeRecorder, expectPrefix, expectContains string) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, expectPrefix)
		assert.Contains(t, event, expectContains)
	default:
		t.Fatalf("expected event %q, got none", expectPrefix)
	}
}

func TestAssumePodCheckpointed_ListError(t *testing.T) {
	enableCheckpointGate(t)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	cli := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&agentsv1alpha1.Checkpoint{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return fmt.Errorf("list unavailable")
			},
		}).Build()
	recorder := record.NewFakeRecorder(10)
	ctrl := NewCheckpointControl(cli, recorder)

	box := newCheckpointTestSandbox()
	pod := newCheckpointTestPod()
	newStatus := &agentsv1alpha1.SandboxStatus{}
	cond := &metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxConditionPaused),
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
	}

	wait := ctrl.AssumePodCheckpointed(context.TODO(), pod, box, newStatus, cond)
	assert.True(t, wait)
	assert.Equal(t, agentsv1alpha1.SandboxPausedReasonCheckpointFailed, cond.Reason)
	assert.Contains(t, cond.Message, "list unavailable")
}

func TestAssumePodCheckpointed_CreateError(t *testing.T) {
	enableCheckpointGate(t)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	cli := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&agentsv1alpha1.Checkpoint{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				return fmt.Errorf("create denied")
			},
		}).Build()
	recorder := record.NewFakeRecorder(10)
	ctrl := NewCheckpointControl(cli, recorder)

	box := newCheckpointTestSandbox()
	pod := newCheckpointTestPod()
	newStatus := &agentsv1alpha1.SandboxStatus{}
	cond := &metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxConditionPaused),
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
	}

	wait := ctrl.AssumePodCheckpointed(context.TODO(), pod, box, newStatus, cond)
	assert.True(t, wait)
	assert.Equal(t, agentsv1alpha1.SandboxPausedReasonCheckpointFailed, cond.Reason)
	assert.Contains(t, cond.Message, "create denied")
}

func TestCleanup_ListError(t *testing.T) {
	enableCheckpointGate(t)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	cli := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&agentsv1alpha1.Checkpoint{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return fmt.Errorf("list error")
			},
		}).Build()
	recorder := record.NewFakeRecorder(10)
	ctrl := NewCheckpointControl(cli, recorder)

	box := newCheckpointTestSandbox()
	ctrl.Cleanup(context.TODO(), box)
}

func TestCleanup_DeleteError(t *testing.T) {
	enableCheckpointGate(t)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	box := newCheckpointTestSandbox()
	cp := newCheckpointTestCP("test-sandbox-cp1", box, agentsv1alpha1.CheckpointSucceeded)

	cli := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&agentsv1alpha1.Checkpoint{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc).
		WithObjects(cp).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
				return fmt.Errorf("delete forbidden")
			},
		}).Build()
	recorder := record.NewFakeRecorder(10)
	ctrl := NewCheckpointControl(cli, recorder)

	ctrl.Cleanup(context.TODO(), box)
}

func TestGetPodTemplateDelta_ListError(t *testing.T) {
	enableCheckpointGate(t)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	cli := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&agentsv1alpha1.Checkpoint{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return fmt.Errorf("list error")
			},
		}).Build()
	recorder := record.NewFakeRecorder(10)
	ctrl := NewCheckpointControl(cli, recorder)

	box := newCheckpointTestSandbox()
	delta := ctrl.GetPodTemplateDelta(context.TODO(), box)
	assert.Nil(t, delta)
}

func TestCreateCheckpoint_AlreadyExists(t *testing.T) {
	enableCheckpointGate(t)
	box := newCheckpointTestSandbox()

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	cli := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&agentsv1alpha1.Checkpoint{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				return fmt.Errorf("already exists")
			},
		}).Build()
	recorder := record.NewFakeRecorder(10)
	ctrl := NewCheckpointControl(cli, recorder)

	err := ctrl.createCheckpoint(context.TODO(), box)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}
