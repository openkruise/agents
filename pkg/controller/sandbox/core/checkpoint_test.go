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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
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
							agentsv1alpha1.CheckpointLabelType:        agentsv1alpha1.CheckpointPersistentContentPodInfo,
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
							agentsv1alpha1.CheckpointLabelType:        agentsv1alpha1.CheckpointPersistentContentPodInfo,
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
							agentsv1alpha1.CheckpointLabelType:        agentsv1alpha1.CheckpointPersistentContentPodInfo,
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
							agentsv1alpha1.CheckpointLabelType:        agentsv1alpha1.CheckpointPersistentContentPodInfo,
						},
					},
				},
			},
			sandboxUID:  sandboxUID,
			expectEmpty: true,
			expectError: "",
		},
		{
			name: "all checkpoints being deleted - returns nil",
			checkpoints: []agentsv1alpha1.Checkpoint{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "test-sandbox-deleted",
						Namespace:         "default",
						CreationTimestamp: metav1.Now(),
						DeletionTimestamp: &metav1.Time{Time: metav1.Now().Time},
						Finalizers:        []string{"foregroundDeletion"},
						OwnerReferences:   []metav1.OwnerReference{ownerRef},
						Labels: map[string]string{
							agentsv1alpha1.CheckpointLabelSandboxName: "test-sandbox",
							agentsv1alpha1.CheckpointLabelType:        agentsv1alpha1.CheckpointPersistentContentPodInfo,
						},
					},
				},
			},
			sandboxUID:  sandboxUID,
			expectEmpty: true,
			expectError: "",
		},
		{
			name: "mix of deleted and active checkpoints - only returns active",
			checkpoints: []agentsv1alpha1.Checkpoint{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "test-sandbox-deleted",
						Namespace:         "default",
						CreationTimestamp: metav1.NewTime(metav1.Now().Add(-5 * 60 * 1e9)),
						DeletionTimestamp: &metav1.Time{Time: metav1.Now().Time},
						Finalizers:        []string{"foregroundDeletion"},
						OwnerReferences:   []metav1.OwnerReference{ownerRef},
						Labels: map[string]string{
							agentsv1alpha1.CheckpointLabelSandboxName: "test-sandbox",
							agentsv1alpha1.CheckpointLabelType:        agentsv1alpha1.CheckpointPersistentContentPodInfo,
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "test-sandbox-active",
						Namespace:         "default",
						CreationTimestamp: metav1.Now(),
						OwnerReferences:   []metav1.OwnerReference{ownerRef},
						Labels: map[string]string{
							agentsv1alpha1.CheckpointLabelSandboxName: "test-sandbox",
							agentsv1alpha1.CheckpointLabelType:        agentsv1alpha1.CheckpointPersistentContentPodInfo,
						},
					},
				},
			},
			sandboxUID:      sandboxUID,
			expectEmpty:     false,
			expectError:     "",
			expectedCPName:  "test-sandbox-active",
			expectedCPCount: 1,
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
			cpList, err := listCheckpointsForSandbox(context.TODO(), cli, box, agentsv1alpha1.CheckpointPersistentContentPodInfo)
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
				agentsv1alpha1.CheckpointLabelType:        agentsv1alpha1.CheckpointPersistentContentPodInfo,
			},
		},
		Spec: agentsv1alpha1.CheckpointSpec{
			PersistentContents: []string{agentsv1alpha1.CheckpointPersistentContentPodInfo},
		},
		Status: agentsv1alpha1.CheckpointStatus{
			Phase: phase,
		},
	}
}

// testPodInfoScope is the default pause-flow checkpoint scope used in tests:
// pod-info type with image validation.
var testPodInfoScope = CheckpointScope{
	PersistentContents: []string{agentsv1alpha1.CheckpointPersistentContentPodInfo},
	ValidateImages:     true,
}

func TestCheckpointContentsForPause(t *testing.T) {
	tests := []struct {
		name     string
		contents []string
		expected []string
	}{
		{
			name:     "no persistent contents - podInfo only",
			expected: []string{agentsv1alpha1.CheckpointPersistentContentPodInfo},
		},
		{
			name:     "filesystem only - dump content carries pod info, not combined",
			contents: []string{agentsv1alpha1.PersistentContentFilesystem},
			expected: []string{agentsv1alpha1.CheckpointPersistentContentFilesystem},
		},
		{
			name:     "filesystem and memory - dump contents only",
			contents: []string{agentsv1alpha1.PersistentContentFilesystem, agentsv1alpha1.PersistentContentMemory},
			expected: []string{
				agentsv1alpha1.CheckpointPersistentContentFilesystem,
				agentsv1alpha1.CheckpointPersistentContentMemory,
			},
		},
		{
			name:     "memory only - dump content carries pod info, not combined",
			contents: []string{agentsv1alpha1.PersistentContentMemory},
			expected: []string{agentsv1alpha1.CheckpointPersistentContentMemory},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			box := newCheckpointTestSandbox()
			box.Spec.PersistentContents = tt.contents
			assert.Equal(t, tt.expected, checkpointContentsForPause(box))
		})
	}
}

func TestCheckpointLabelForContents(t *testing.T) {
	tests := []struct {
		name     string
		contents []string
		expected string
	}{
		{
			name:     "empty contents - empty label",
			contents: nil,
			expected: "",
		},
		{
			name:     "single podInfo",
			contents: []string{agentsv1alpha1.CheckpointPersistentContentPodInfo},
			expected: agentsv1alpha1.CheckpointPersistentContentPodInfo,
		},
		{
			name:     "single filesystem",
			contents: []string{agentsv1alpha1.CheckpointPersistentContentFilesystem},
			expected: agentsv1alpha1.CheckpointPersistentContentFilesystem,
		},
		{
			name: "podInfo and filesystem joined in sorted order",
			contents: []string{
				agentsv1alpha1.CheckpointPersistentContentPodInfo,
				agentsv1alpha1.CheckpointPersistentContentFilesystem,
			},
			expected: agentsv1alpha1.CheckpointPersistentContentFilesystem + "-" + agentsv1alpha1.CheckpointPersistentContentPodInfo,
		},
		{
			name: "three contents sorted regardless of input order",
			contents: []string{
				agentsv1alpha1.CheckpointPersistentContentPodInfo,
				agentsv1alpha1.CheckpointPersistentContentMemory,
				agentsv1alpha1.CheckpointPersistentContentFilesystem,
			},
			expected: agentsv1alpha1.CheckpointPersistentContentFilesystem + "-" +
				agentsv1alpha1.CheckpointPersistentContentMemory + "-" +
				agentsv1alpha1.CheckpointPersistentContentPodInfo,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := tt.contents
			assert.Equal(t, tt.expected, checkpointLabelForContents(tt.contents))
			// The input slice must not be mutated by the sort.
			assert.Equal(t, before, tt.contents)
		})
	}
}

func TestAssumePodCheckpointed(t *testing.T) {
	tests := []struct {
		name         string
		pod          *corev1.Pod
		box          *agentsv1alpha1.Sandbox
		existingCPs  []client.Object
		condReason   string
		expectWait   bool
		expectReason string
	}{
		{
			name:         "non-checkpoint reason - returns false immediately",
			pod:          newCheckpointTestPod(),
			box:          newCheckpointTestSandbox(),
			condReason:   agentsv1alpha1.SandboxPausedReasonStopPauseSucceed,
			expectWait:   false,
			expectReason: agentsv1alpha1.SandboxPausedReasonStopPauseSucceed,
		},
		{
			name: "image changed - pause rejected",
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
			pod:          newCheckpointTestPod(),
			box:          newCheckpointTestSandbox(),
			expectWait:   true,
			expectReason: agentsv1alpha1.SandboxPausedReasonCheckpointCreating,
		},
		{
			name: "checkpoint succeeded - returns false",
			pod:  newCheckpointTestPod(),
			box:  newCheckpointTestSandbox(),
			existingCPs: []client.Object{
				newCheckpointTestCP("test-sandbox-cp1", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointSucceeded),
			},
			condReason:   agentsv1alpha1.SandboxPausedReasonCheckpointCreating,
			expectWait:   false,
			expectReason: agentsv1alpha1.SandboxPausedReasonCheckpointSucceeded,
		},
		{
			name: "checkpoint failed - returns true",
			pod:  newCheckpointTestPod(),
			box:  newCheckpointTestSandbox(),
			existingCPs: []client.Object{
				newCheckpointTestCP("test-sandbox-cp1", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointFailed),
			},
			condReason:   agentsv1alpha1.SandboxPausedReasonCheckpointCreating,
			expectWait:   true,
			expectReason: agentsv1alpha1.SandboxPausedReasonCheckpointFailed,
		},
		{
			name:         "checkpoint failed reason - retry creates checkpoint",
			pod:          newCheckpointTestPod(),
			box:          newCheckpointTestSandbox(),
			condReason:   agentsv1alpha1.SandboxPausedReasonCheckpointFailed,
			expectWait:   true,
			expectReason: agentsv1alpha1.SandboxPausedReasonCheckpointFailed,
		},
		{
			name: "checkpoint in progress - waits",
			pod:  newCheckpointTestPod(),
			box:  newCheckpointTestSandbox(),
			existingCPs: []client.Object{
				newCheckpointTestCP("test-sandbox-cp1", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointCreating),
			},
			condReason:   agentsv1alpha1.SandboxPausedReasonCheckpointCreating,
			expectWait:   true,
			expectReason: agentsv1alpha1.SandboxPausedReasonCheckpointCreating,
		},
		{
			name: "existing succeeded checkpoint with fresh entry - returns false",
			pod:  newCheckpointTestPod(),
			box:  newCheckpointTestSandbox(),
			existingCPs: []client.Object{
				newCheckpointTestCP("test-sandbox-stale", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointSucceeded),
			},
			expectWait:   false,
			expectReason: agentsv1alpha1.SandboxPausedReasonCheckpointSucceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl, _ := newCheckpointTestControl(tt.existingCPs...)
			newStatus := &agentsv1alpha1.SandboxStatus{}
			cond := &metav1.Condition{
				Type:               string(agentsv1alpha1.SandboxConditionPaused),
				Status:             metav1.ConditionFalse,
				Reason:             tt.condReason,
				LastTransitionTime: metav1.Now(),
			}

			wait := ctrl.AssumePodCheckpointed(context.TODO(), tt.pod, tt.box, newStatus, cond, testPodInfoScope)
			assert.Equal(t, tt.expectWait, wait)
			if tt.expectReason != "" {
				assert.Equal(t, tt.expectReason, cond.Reason)
			}
		})
	}
}

func TestGetCheckpointResumeData(t *testing.T) {
	podInfoCPWithDelta := func() *agentsv1alpha1.Checkpoint {
		cp := newCheckpointTestCP("test-sandbox-podinfo", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointSucceeded)
		cp.Status.PodTemplateDelta = runtime.RawExtension{Raw: []byte(`{"spec":{"containers":[]}}`)}
		return cp
	}
	upgradeCPWithID := func() *agentsv1alpha1.Checkpoint {
		cp := newUpgradeCheckpointTestCP("test-sandbox-upgrade", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointSucceeded)
		cp.Status.CheckpointId = "cp-id-123"
		return cp
	}

	tests := []struct {
		name         string
		existingCPs  []client.Object
		interceptors interceptor.Funcs
		expectNil    bool
		expectID     string
	}{
		{
			name:      "no checkpoints - returns nil delta and empty ID",
			expectNil: true,
			expectID:  "",
		},
		{
			name:        "pod-info checkpoint with delta only",
			existingCPs: []client.Object{podInfoCPWithDelta()},
			expectNil:   false,
			expectID:    "",
		},
		{
			// Dump checkpoints record the pod template delta as well, so a
			// filesystem-only checkpoint must also provide the delta.
			name: "filesystem checkpoint with delta only",
			existingCPs: []client.Object{
				func() *agentsv1alpha1.Checkpoint {
					cp := newUpgradeCheckpointTestCP("test-sandbox-fs", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointSucceeded)
					cp.Status.PodTemplateDelta = runtime.RawExtension{Raw: []byte(`{"spec":{"containers":[]}}`)}
					return cp
				}(),
			},
			expectNil: false,
			expectID:  "",
		},
		{
			// The delta is legitimately empty when the pod carries no resource
			// drift and no injected containers, so a checkpoint recording only
			// its ID must still be selected; otherwise a CheckpointRestore
			// upgrade cannot find the checkpoint to restore from.
			name:        "upgrade checkpoint with ID only - selected",
			existingCPs: []client.Object{upgradeCPWithID()},
			expectNil:   true,
			expectID:    "cp-id-123",
		},
		{
			name: "single checkpoint with both delta and ID",
			existingCPs: []client.Object{
				func() *agentsv1alpha1.Checkpoint {
					cp := newCheckpointTestCP("test-sandbox-both", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointSucceeded)
					cp.Status.PodTemplateDelta = runtime.RawExtension{Raw: []byte(`{"spec":{"containers":[]}}`)}
					cp.Status.CheckpointId = "cp-id-456"
					return cp
				}(),
			},
			expectNil: false,
			expectID:  "cp-id-456",
		},
		{
			// The delta and the ID always come from the same checkpoint: the
			// first one with resume data wins, and the ID of a later
			// checkpoint is not picked up.
			name: "multiple checkpoints - delta and ID from the same checkpoint",
			existingCPs: []client.Object{
				func() *agentsv1alpha1.Checkpoint {
					cp := newUpgradeCheckpointTestCP("test-sandbox-fs", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointSucceeded)
					cp.Status.PodTemplateDelta = runtime.RawExtension{Raw: []byte(`{"spec":{"containers":[]}}`)}
					cp.Status.CheckpointId = "cp-id-first"
					return cp
				}(),
				upgradeCPWithID(),
			},
			expectNil: false,
			expectID:  "cp-id-first",
		},
		{
			// A checkpoint without delta and without ID carries no resume data
			// (e.g. still in progress), so nothing is returned.
			name:        "checkpoint with empty delta - returns nil",
			existingCPs: []client.Object{newCheckpointTestCP("test-sandbox-cp1", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointSucceeded)},
			expectNil:   true,
			expectID:    "",
		},
		{
			name: "list error - returns nil delta and empty ID",
			interceptors: interceptor.Funcs{List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return fmt.Errorf("list error")
			}},
			expectNil: true,
			expectID:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			_ = agentsv1alpha1.AddToScheme(scheme)
			builder := fake.NewClientBuilder().WithScheme(scheme).
				WithIndex(&agentsv1alpha1.Checkpoint{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc)
			if tt.interceptors.List != nil {
				builder = builder.WithInterceptorFuncs(tt.interceptors)
			}
			for _, o := range tt.existingCPs {
				builder = builder.WithObjects(o)
			}
			cli := builder.Build()
			ctrl := NewCheckpointControl(cli, record.NewFakeRecorder(10))
			box := newCheckpointTestSandbox()

			delta, id := ctrl.GetCheckpointResumeData(context.TODO(), box)
			if tt.expectNil {
				assert.Nil(t, delta)
			} else {
				assert.NotNil(t, delta)
				assert.NotEmpty(t, delta.Raw)
			}
			assert.Equal(t, tt.expectID, id)
		})
	}
}

func TestCleanup(t *testing.T) {
	tests := []struct {
		name    string
		cpCount int
	}{
		{
			name:    "single checkpoint - deletes all checkpoints",
			cpCount: 1,
		},
		{
			name:    "multiple checkpoints - deletes all checkpoints",
			cpCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			box := newCheckpointTestSandbox()
			var objs []client.Object
			for i := 0; i < tt.cpCount; i++ {
				objs = append(objs, newCheckpointTestCP(
					box.Name+"-cp"+string(rune('0'+i)), box, agentsv1alpha1.CheckpointSucceeded,
				))
			}
			ctrl, cli := newCheckpointTestControl(objs...)

			ctrl.CleanupCheckpoints(context.TODO(), box)

			remaining := &agentsv1alpha1.CheckpointList{}
			_ = cli.List(context.TODO(), remaining, client.InNamespace(box.Namespace))
			assert.Empty(t, remaining.Items)
		})
	}
}

func TestCreateCheckpoint(t *testing.T) {
	box := newCheckpointTestSandbox()
	ctrl, cli, recorder := newCheckpointTestControlWithRecorder()

	name, err := ctrl.createCheckpoint(context.TODO(), box, []string{agentsv1alpha1.CheckpointPersistentContentPodInfo})
	assert.NoError(t, err)
	assert.NotEmpty(t, name)

	cpList := &agentsv1alpha1.CheckpointList{}
	err = cli.List(context.TODO(), cpList, client.InNamespace(box.Namespace))
	assert.NoError(t, err)
	assert.Len(t, cpList.Items, 1)

	cp := cpList.Items[0]
	assert.Equal(t, box.Name, *cp.Spec.SandboxName)
	assert.Nil(t, cp.Spec.PodName)
	assert.Equal(t, box.Name, cp.Labels[agentsv1alpha1.CheckpointLabelSandboxName])
	assert.Equal(t, agentsv1alpha1.CheckpointPersistentContentPodInfo, cp.Labels[agentsv1alpha1.CheckpointLabelType])
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
			box := newCheckpointTestSandbox()
			ctrl, _, recorder := newCheckpointTestControlWithRecorder(tt.checkpoint)
			newStatus := &agentsv1alpha1.SandboxStatus{}
			cond := &metav1.Condition{
				Type:               string(agentsv1alpha1.SandboxConditionPaused),
				Status:             metav1.ConditionFalse,
				Reason:             agentsv1alpha1.SandboxPausedReasonCheckpointCreating,
				LastTransitionTime: metav1.Now(),
			}

			wait := ctrl.AssumePodCheckpointed(context.TODO(), newCheckpointTestPod(), box, newStatus, cond, testPodInfoScope)

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

	wait := ctrl.AssumePodCheckpointed(context.TODO(), pod, box, newStatus, cond, testPodInfoScope)
	assert.True(t, wait)
	assert.Equal(t, agentsv1alpha1.SandboxPausedReasonCheckpointFailed, cond.Reason)
	assert.Contains(t, cond.Message, "list unavailable")
}

func TestAssumePodCheckpointed_CreateError(t *testing.T) {
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

	wait := ctrl.AssumePodCheckpointed(context.TODO(), pod, box, newStatus, cond, testPodInfoScope)
	assert.True(t, wait)
	assert.Equal(t, agentsv1alpha1.SandboxPausedReasonCheckpointFailed, cond.Reason)
	assert.Contains(t, cond.Message, "create denied")
}

func TestCleanup_ListError(t *testing.T) {
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
	ctrl.CleanupCheckpoints(context.TODO(), box)
}

func TestCleanup_DeleteError(t *testing.T) {
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

	ctrl.CleanupCheckpoints(context.TODO(), box)
}

func newUpgradeCheckpointTestCP(name string, box *agentsv1alpha1.Sandbox, phase agentsv1alpha1.CheckpointPhase) *agentsv1alpha1.Checkpoint {
	cp := newCheckpointTestCP(name, box, phase)
	cp.Labels[agentsv1alpha1.CheckpointLabelType] = agentsv1alpha1.CheckpointPersistentContentFilesystem
	cp.Spec.PersistentContents = []string{agentsv1alpha1.CheckpointPersistentContentFilesystem}
	return cp
}

func TestEnsureCheckpointForUpgrade(t *testing.T) {
	tests := []struct {
		name          string
		existingCPs   []client.Object
		interceptors  interceptor.Funcs
		expectDone    bool
		expectError   string
		expectCreated bool
	}{
		{
			name:          "no existing checkpoint - creates one and returns false",
			existingCPs:   nil,
			expectDone:    false,
			expectError:   "",
			expectCreated: true,
		},
		{
			name: "checkpoint in progress - returns false",
			existingCPs: []client.Object{
				newUpgradeCheckpointTestCP("test-sandbox-cp1", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointCreating),
			},
			expectDone:  false,
			expectError: "",
		},
		{
			name: "checkpoint succeeded - returns true",
			existingCPs: []client.Object{
				newUpgradeCheckpointTestCP("test-sandbox-cp1", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointSucceeded),
			},
			expectDone:  true,
			expectError: "",
		},
		{
			name: "checkpoint failed - returns error",
			existingCPs: []client.Object{
				func() *agentsv1alpha1.Checkpoint {
					cp := newUpgradeCheckpointTestCP("test-sandbox-cp1", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointFailed)
					cp.Status.Message = "checkpoint timeout"
					return cp
				}(),
			},
			expectDone:  false,
			expectError: "checkpoint test-sandbox-cp1 failed during upgrade",
		},
		{
			name: "list error - returns error",
			interceptors: interceptor.Funcs{List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return fmt.Errorf("list unavailable")
			}},
			expectDone:  false,
			expectError: "failed to list checkpoints",
		},
		{
			name: "create error - returns error",
			interceptors: interceptor.Funcs{Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				return fmt.Errorf("create denied")
			}},
			expectDone:  false,
			expectError: "failed to create checkpoint",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			_ = agentsv1alpha1.AddToScheme(scheme)
			builder := fake.NewClientBuilder().WithScheme(scheme).
				WithIndex(&agentsv1alpha1.Checkpoint{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc).
				WithStatusSubresource(&agentsv1alpha1.Checkpoint{})
			if tt.interceptors.List != nil || tt.interceptors.Create != nil {
				builder = builder.WithInterceptorFuncs(tt.interceptors)
			}
			for _, o := range tt.existingCPs {
				builder = builder.WithObjects(o)
			}
			cli := builder.Build()
			ctrl := NewCheckpointControl(cli, record.NewFakeRecorder(10))
			box := newCheckpointTestSandbox()
			box.Spec.UpgradePolicy = &agentsv1alpha1.SandboxUpgradePolicy{
				Type: agentsv1alpha1.SandboxUpgradePolicyCheckpointRestore,
			}

			done, _, err := ctrl.EnsureCheckpointForUpgrade(context.TODO(), box)
			assert.Equal(t, tt.expectDone, done)
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
			if tt.expectCreated {
				cpList := &agentsv1alpha1.CheckpointList{}
				_ = cli.List(context.TODO(), cpList, client.InNamespace(box.Namespace))
				assert.Len(t, cpList.Items, 1)
				assert.Equal(t, agentsv1alpha1.CheckpointPersistentContentFilesystem, cpList.Items[0].Labels[agentsv1alpha1.CheckpointLabelType])
			}
		})
	}
}

func TestCleanupCheckpoints_Upgrade(t *testing.T) {
	tests := []struct {
		name         string
		existingCPs  []client.Object
		interceptors interceptor.Funcs
		expectRemain int
	}{
		{
			name: "deletes all upgrade checkpoints",
			existingCPs: []client.Object{
				newUpgradeCheckpointTestCP("test-sandbox-cp1", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointSucceeded),
				newUpgradeCheckpointTestCP("test-sandbox-cp2", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointSucceeded),
			},
			expectRemain: 0,
		},
		{
			name:         "no checkpoints - no-op",
			expectRemain: 0,
		},
		{
			name: "list error - no panic",
			interceptors: interceptor.Funcs{List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return fmt.Errorf("list error")
			}},
			expectRemain: 0,
		},
		{
			name: "delete error - continues and logs",
			existingCPs: []client.Object{
				newUpgradeCheckpointTestCP("test-sandbox-cp1", newCheckpointTestSandbox(), agentsv1alpha1.CheckpointSucceeded),
			},
			interceptors: interceptor.Funcs{Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
				return fmt.Errorf("delete forbidden")
			}},
			expectRemain: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			_ = agentsv1alpha1.AddToScheme(scheme)
			builder := fake.NewClientBuilder().WithScheme(scheme).
				WithIndex(&agentsv1alpha1.Checkpoint{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc)
			if tt.interceptors.List != nil || tt.interceptors.Delete != nil {
				builder = builder.WithInterceptorFuncs(tt.interceptors)
			}
			for _, o := range tt.existingCPs {
				builder = builder.WithObjects(o)
			}
			cli := builder.Build()
			ctrl := NewCheckpointControl(cli, record.NewFakeRecorder(10))
			box := newCheckpointTestSandbox()

			ctrl.CleanupCheckpoints(context.TODO(), box)

			remaining := &agentsv1alpha1.CheckpointList{}
			_ = cli.List(context.TODO(), remaining, client.InNamespace(box.Namespace))
			assert.Len(t, remaining.Items, tt.expectRemain)
		})
	}
}

func TestCreateCheckpoint_AlreadyExists(t *testing.T) {
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

	_, err := ctrl.createCheckpoint(context.TODO(), box, []string{agentsv1alpha1.CheckpointPersistentContentPodInfo})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

// TestCleanupCheckpoints_NotFoundSettlesExpectation pins that a delete which
// 404s still settles the scale expectation. The checkpoint is already gone, so
// no delete event reaches CheckpointEventHandler and nothing else can observe
// it. Leaving it unobserved blocks the Sandbox reconcile in
// SatisfiedExpectations until ExpectationTimeout, which is one minute.
func TestCleanupCheckpoints_NotFoundSettlesExpectation(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)

	box := newCheckpointTestSandbox()
	cp := newCheckpointTestCP("test-sandbox-cp1", box, agentsv1alpha1.CheckpointSucceeded)

	cli := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&agentsv1alpha1.Checkpoint{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc).
		WithObjects(cp).
		WithInterceptorFuncs(interceptor.Funcs{
			// The list came from a cache that still holds the checkpoint; the
			// delete reaches the API server and finds it already gone.
			Delete: func(_ context.Context, _ client.WithWatch, o client.Object, _ ...client.DeleteOption) error {
				return errors.NewNotFound(schema.GroupResource{Resource: "checkpoints"}, o.GetName())
			},
		}).Build()

	ctrl := NewCheckpointControl(cli, record.NewFakeRecorder(10))
	key := GetControllerKey(box)
	ScaleExpectation.DeleteExpectations(key)
	t.Cleanup(func() { ScaleExpectation.DeleteExpectations(key) })

	ctrl.CleanupCheckpoints(context.TODO(), box)

	satisfied, _, unmet := ScaleExpectation.SatisfiedExpectations(key)
	assert.True(t, satisfied, "expectation must be settled after a NotFound delete, unmet: %v", unmet)
}

// TestCleanupCheckpoints_DeleteErrorSettlesExpectation pins the same property
// for a retriable failure, where the delete never happened at all.
func TestCleanupCheckpoints_DeleteErrorSettlesExpectation(t *testing.T) {
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

	ctrl := NewCheckpointControl(cli, record.NewFakeRecorder(10))
	key := GetControllerKey(box)
	ScaleExpectation.DeleteExpectations(key)
	t.Cleanup(func() { ScaleExpectation.DeleteExpectations(key) })

	ctrl.CleanupCheckpoints(context.TODO(), box)

	satisfied, _, unmet := ScaleExpectation.SatisfiedExpectations(key)
	assert.True(t, satisfied, "expectation must be settled after a failed delete, unmet: %v", unmet)
}
