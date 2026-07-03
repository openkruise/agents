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

package cli

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned/fake"
)

func TestSetImageSandboxSet(t *testing.T) {
	inlineSandboxSet := func() *agentsv1alpha1.SandboxSet {
		return &agentsv1alpha1.SandboxSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-sbs",
				Namespace: "default",
			},
			Spec: agentsv1alpha1.SandboxSetSpec{
				Replicas: 3,
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							InitContainers: []corev1.Container{
								{Name: "init", Image: "busybox:1.0"},
							},
							Containers: []corev1.Container{
								{Name: "main", Image: "nginx:1.0"},
								{Name: "sidecar", Image: "envoy:1.0"},
							},
						},
					},
				},
			},
		}
	}

	templateRefSandboxSet := func() *agentsv1alpha1.SandboxSet {
		return &agentsv1alpha1.SandboxSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ref-sbs",
				Namespace: "default",
			},
			Spec: agentsv1alpha1.SandboxSetSpec{
				Replicas: 1,
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					TemplateRef: &agentsv1alpha1.SandboxTemplateRef{Name: "my-template"},
				},
			},
		}
	}

	tests := []struct {
		name              string
		sbsName           string
		namespace         string
		imageArgs         []string
		objects           []runtime.Object
		expectError       string
		expectNotContains string
		expectedImages    map[string]string
	}{
		{
			name:      "update single container image",
			sbsName:   "test-sbs",
			namespace: "default",
			imageArgs: []string{"main=nginx:2.0"},
			objects:   []runtime.Object{inlineSandboxSet()},
			expectedImages: map[string]string{
				"main":    "nginx:2.0",
				"sidecar": "envoy:1.0",
			},
		},
		{
			name:      "update multiple container images",
			sbsName:   "test-sbs",
			namespace: "default",
			imageArgs: []string{"main=nginx:2.0", "sidecar=envoy:2.0"},
			objects:   []runtime.Object{inlineSandboxSet()},
			expectedImages: map[string]string{
				"main":    "nginx:2.0",
				"sidecar": "envoy:2.0",
			},
		},
		{
			name:      "update init container image",
			sbsName:   "test-sbs",
			namespace: "default",
			imageArgs: []string{"init=busybox:2.0"},
			objects:   []runtime.Object{inlineSandboxSet()},
			expectedImages: map[string]string{
				"init": "busybox:2.0",
			},
		},
		{
			name:              "container not found",
			sbsName:           "test-sbs",
			namespace:         "default",
			imageArgs:         []string{"nonexistent=foo:1.0"},
			objects:           []runtime.Object{inlineSandboxSet()},
			expectError:       "container \"nonexistent\" not found",
			expectNotContains: "failed to update sandboxset",
		},
		{
			name:              "sandboxset not found",
			sbsName:           "nonexistent",
			namespace:         "default",
			imageArgs:         []string{"main=nginx:2.0"},
			objects:           []runtime.Object{},
			expectError:       "failed to get sandboxset",
			expectNotContains: "failed to update sandboxset",
		},
		{
			name:              "sandboxset uses TemplateRef",
			sbsName:           "ref-sbs",
			namespace:         "default",
			imageArgs:         []string{"main=nginx:2.0"},
			objects:           []runtime.Object{templateRefSandboxSet()},
			expectError:       "uses a TemplateRef",
			expectNotContains: "failed to update sandboxset",
		},
		{
			name:        "invalid image argument format",
			sbsName:     "test-sbs",
			namespace:   "default",
			imageArgs:   []string{"invalid-format"},
			objects:     []runtime.Object{inlineSandboxSet()},
			expectError: "invalid container=image argument",
		},
		{
			name:        "empty container name",
			sbsName:     "test-sbs",
			namespace:   "default",
			imageArgs:   []string{"=nginx:2.0"},
			objects:     []runtime.Object{inlineSandboxSet()},
			expectError: "invalid container=image argument",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset(tt.objects...)

			opts := &setImageOptions{
				global: &GlobalOptions{
					Namespace: tt.namespace,
				},
			}

			err := runSetImageWithClient(cs.ApiV1alpha1(), opts, tt.sbsName, tt.imageArgs, false)

			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				if tt.expectNotContains != "" {
					assert.NotContains(t, err.Error(), tt.expectNotContains)
				}
			} else {
				assert.NoError(t, err)

				updated, getErr := cs.ApiV1alpha1().SandboxSets(tt.namespace).Get(
					context.TODO(), tt.sbsName, metav1.GetOptions{},
				)
				assert.NoError(t, getErr)

				allContainers := append(updated.Spec.Template.Spec.Containers, updated.Spec.Template.Spec.InitContainers...)
				for _, c := range allContainers {
					if expected, ok := tt.expectedImages[c.Name]; ok {
						assert.Equal(t, expected, c.Image, "container %s image mismatch", c.Name)
					}
				}
			}
		})
	}
}

func TestIsSandboxSetUpdateComplete(t *testing.T) {
	tests := []struct {
		name     string
		sbs      *agentsv1alpha1.SandboxSet
		expected bool
	}{
		{
			name: "update complete",
			sbs: &agentsv1alpha1.SandboxSet{
				Spec:   agentsv1alpha1.SandboxSetSpec{Replicas: 3},
				Status: agentsv1alpha1.SandboxSetStatus{UpdatedReplicas: 3, AvailableReplicas: 3, UpdatedAvailableReplicas: 3},
			},
			expected: true,
		},
		{
			name: "updating in progress",
			sbs: &agentsv1alpha1.SandboxSet{
				Spec:   agentsv1alpha1.SandboxSetSpec{Replicas: 3},
				Status: agentsv1alpha1.SandboxSetStatus{UpdatedReplicas: 1, AvailableReplicas: 3, UpdatedAvailableReplicas: 1},
			},
			expected: false,
		},
		{
			name: "updated but not available",
			sbs: &agentsv1alpha1.SandboxSet{
				Spec:   agentsv1alpha1.SandboxSetSpec{Replicas: 3},
				Status: agentsv1alpha1.SandboxSetStatus{UpdatedReplicas: 3, AvailableReplicas: 2, UpdatedAvailableReplicas: 2},
			},
			expected: false,
		},
		{
			name: "zero replicas",
			sbs: &agentsv1alpha1.SandboxSet{
				Spec:   agentsv1alpha1.SandboxSetSpec{Replicas: 0},
				Status: agentsv1alpha1.SandboxSetStatus{UpdatedReplicas: 0, AvailableReplicas: 0, UpdatedAvailableReplicas: 0},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isSandboxSetUpdateComplete(tt.sbs)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestSetImageStatus(t *testing.T) {
	tests := []struct {
		name        string
		sbsName     string
		namespace   string
		objects     []runtime.Object
		expectError string
		expectPhase string
	}{
		{
			name:        "sandboxset not found",
			sbsName:     "nonexistent",
			namespace:   "default",
			objects:     []runtime.Object{},
			expectError: "failed to get sandboxset",
		},
		{
			name:      "update complete",
			sbsName:   "test-sbs",
			namespace: "default",
			objects: []runtime.Object{
				&agentsv1alpha1.SandboxSet{
					ObjectMeta: metav1.ObjectMeta{Name: "test-sbs", Namespace: "default"},
					Spec:       agentsv1alpha1.SandboxSetSpec{Replicas: 3},
					Status:     agentsv1alpha1.SandboxSetStatus{UpdatedReplicas: 3, AvailableReplicas: 3, UpdatedAvailableReplicas: 3},
				},
			},
			expectPhase: "Complete",
		},
		{
			name:      "updating in progress",
			sbsName:   "test-sbs",
			namespace: "default",
			objects: []runtime.Object{
				&agentsv1alpha1.SandboxSet{
					ObjectMeta: metav1.ObjectMeta{Name: "test-sbs", Namespace: "default"},
					Spec:       agentsv1alpha1.SandboxSetSpec{Replicas: 3},
					Status:     agentsv1alpha1.SandboxSetStatus{UpdatedReplicas: 1, AvailableReplicas: 2, UpdatedAvailableReplicas: 1},
				},
			},
			expectPhase: "Updating",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset(tt.objects...)
			globalOpts := &GlobalOptions{Namespace: tt.namespace}

			err := runSetImageStatusWithClient(cs.ApiV1alpha1(), globalOpts, tt.sbsName)

			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestParseContainerImages(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		expected    map[string]string
		expectError string
	}{
		{
			name:     "single pair",
			args:     []string{"main=nginx:2.0"},
			expected: map[string]string{"main": "nginx:2.0"},
		},
		{
			name:     "multiple pairs",
			args:     []string{"main=nginx:2.0", "sidecar=envoy:2.0"},
			expected: map[string]string{"main": "nginx:2.0", "sidecar": "envoy:2.0"},
		},
		{
			name:     "image with registry and tag",
			args:     []string{"main=registry.example.com/org/nginx:v2.0.1"},
			expected: map[string]string{"main": "registry.example.com/org/nginx:v2.0.1"},
		},
		{
			name:        "missing equals sign",
			args:        []string{"invalid"},
			expectError: "invalid container=image argument",
		},
		{
			name:        "empty image",
			args:        []string{"main="},
			expectError: "invalid container=image argument",
		},
		{
			name:        "empty container",
			args:        []string{"=nginx:2.0"},
			expectError: "invalid container=image argument",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseImageArgs(tt.args)

			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestDiagnoseSandboxSetUpdate(t *testing.T) {
	// Override inClusterConfigFn for tests
	origFn := inClusterConfigFn
	defer func() { inClusterConfigFn = origFn }()
	inClusterConfigFn = func() (*rest.Config, error) {
		return nil, fmt.Errorf("not in cluster")
	}

	tests := []struct {
		name       string
		sbs        *agentsv1alpha1.SandboxSet
		sandboxes  []*agentsv1alpha1.Sandbox
		pods       []*corev1.Pod
		expectSkip bool
	}{
		{
			name: "update complete, skip diagnosis",
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sbs", Namespace: "default"},
				Spec:       agentsv1alpha1.SandboxSetSpec{Replicas: 3},
				Status:     agentsv1alpha1.SandboxSetStatus{UpdatedReplicas: 3, AvailableReplicas: 3, UpdatedAvailableReplicas: 3},
			},
			expectSkip: true,
		},
		{
			name: "sandbox in Pending state with message",
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sbs", Namespace: "default"},
				Spec:       agentsv1alpha1.SandboxSetSpec{Replicas: 3},
				Status:     agentsv1alpha1.SandboxSetStatus{UpdatedReplicas: 1, AvailableReplicas: 1},
			},
			sandboxes: []*agentsv1alpha1.Sandbox{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sbx-1",
						Namespace: "default",
						Labels:    map[string]string{agentsv1alpha1.LabelSandboxTemplate: "test-sbs"},
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase:   agentsv1alpha1.SandboxPending,
						Message: "image pull timeout",
					},
				},
			},
		},
		{
			name: "sandbox in Failed state",
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sbs", Namespace: "default"},
				Spec:       agentsv1alpha1.SandboxSetSpec{Replicas: 3},
				Status:     agentsv1alpha1.SandboxSetStatus{UpdatedReplicas: 1, AvailableReplicas: 1},
			},
			sandboxes: []*agentsv1alpha1.Sandbox{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sbx-failed",
						Namespace: "default",
						Labels:    map[string]string{agentsv1alpha1.LabelSandboxTemplate: "test-sbs"},
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase:   agentsv1alpha1.SandboxFailed,
						Message: "OOMKilled",
					},
				},
			},
		},
		{
			name: "sandbox Pending without message, pod has scheduling failure",
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sbs", Namespace: "default"},
				Spec:       agentsv1alpha1.SandboxSetSpec{Replicas: 3},
				Status:     agentsv1alpha1.SandboxSetStatus{UpdatedReplicas: 1, AvailableReplicas: 1},
			},
			sandboxes: []*agentsv1alpha1.Sandbox{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sbx-sched",
						Namespace: "default",
						Labels:    map[string]string{agentsv1alpha1.LabelSandboxTemplate: "test-sbs"},
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase:   agentsv1alpha1.SandboxPending,
						Message: "",
					},
				},
			},
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "sbx-sched", Namespace: "default"},
					Status: corev1.PodStatus{
						Conditions: []corev1.PodCondition{
							{
								Type:    corev1.PodScheduled,
								Status:  corev1.ConditionFalse,
								Message: "0/3 nodes are available: insufficient memory",
							},
						},
					},
				},
			},
		},
		{
			name: "sandbox Pending without message, pod has ImagePullBackOff",
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sbs", Namespace: "default"},
				Spec:       agentsv1alpha1.SandboxSetSpec{Replicas: 3},
				Status:     agentsv1alpha1.SandboxSetStatus{UpdatedReplicas: 1, AvailableReplicas: 1},
			},
			sandboxes: []*agentsv1alpha1.Sandbox{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sbx-pull",
						Namespace: "default",
						Labels:    map[string]string{agentsv1alpha1.LabelSandboxTemplate: "test-sbs"},
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase:   agentsv1alpha1.SandboxPending,
						Message: "",
					},
				},
			},
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "sbx-pull", Namespace: "default"},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name: "main",
								State: corev1.ContainerState{
									Waiting: &corev1.ContainerStateWaiting{
										Reason: "ImagePullBackOff",
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "sandbox Pending without message, no pod found",
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sbs", Namespace: "default"},
				Spec:       agentsv1alpha1.SandboxSetSpec{Replicas: 3},
				Status:     agentsv1alpha1.SandboxSetStatus{UpdatedReplicas: 1, AvailableReplicas: 1},
			},
			sandboxes: []*agentsv1alpha1.Sandbox{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sbx-nopod",
						Namespace: "default",
						Labels:    map[string]string{agentsv1alpha1.LabelSandboxTemplate: "test-sbs"},
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase:   agentsv1alpha1.SandboxPending,
						Message: "",
					},
				},
			},
			pods: nil,
		},
		{
			name: "sandbox Running is skipped",
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sbs", Namespace: "default"},
				Spec:       agentsv1alpha1.SandboxSetSpec{Replicas: 3},
				Status:     agentsv1alpha1.SandboxSetStatus{UpdatedReplicas: 1, AvailableReplicas: 1},
			},
			sandboxes: []*agentsv1alpha1.Sandbox{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sbx-running",
						Namespace: "default",
						Labels:    map[string]string{agentsv1alpha1.LabelSandboxTemplate: "test-sbs"},
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxRunning,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build fake agents client with sandboxes
			agentsCS := fake.NewSimpleClientset()
			for _, sbx := range tt.sandboxes {
				_, err := agentsCS.ApiV1alpha1().Sandboxes(sbx.Namespace).Create(
					context.TODO(), sbx, metav1.CreateOptions{},
				)
				assert.NoError(t, err)
			}

			// Build fake kubernetes client with pods
			var objs []runtime.Object
			for _, pod := range tt.pods {
				objs = append(objs, pod)
			}
			kubeCS := kubernetesfake.NewSimpleClientset(objs...)

			reported := make(map[string]bool)
			diagnoseSandboxSetUpdate(agentsCS.ApiV1alpha1(), kubeCS, "default", tt.sbs, reported)

			if tt.expectSkip {
				// When update is complete, the function returns early
				assert.Empty(t, reported)
				return
			}

			// For non-skip cases, verify the function ran without panic
			// and reported maps problem sandboxes correctly
			for _, sbx := range tt.sandboxes {
				if sbx.Status.Phase == agentsv1alpha1.SandboxPending || sbx.Status.Phase == agentsv1alpha1.SandboxFailed {
					assert.True(t, reported[sbx.Name], "sandbox %q should be reported", sbx.Name)
				}
			}
		})
	}
}

func TestNewSetCommand(t *testing.T) {
	globalOpts := NewGlobalOptions()
	cmd := NewSetCommand(globalOpts)

	assert.Equal(t, "set SUBCOMMAND", cmd.Use)
	assert.NotEmpty(t, cmd.Short)
	assert.True(t, cmd.HasSubCommands())

	// Verify "image" subcommand exists
	imageCmd, _, err := cmd.Find([]string{"image"})
	assert.NoError(t, err)
	assert.NotNil(t, imageCmd)
}

func TestNewSetImageCommandUnsupportedResourceType(t *testing.T) {
	globalOpts := NewGlobalOptions()
	cmd := NewSetCommand(globalOpts)

	// Execute with unsupported resource type
	cmd.SetArgs([]string{"image", "deployment", "my-deploy", "main=nginx:2.0"})
	err := cmd.Execute()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported resource type")
}

func TestUpdateContainerImages(t *testing.T) {
	tests := []struct {
		name          string
		containers    []corev1.Container
		images        map[string]string
		expectUpdated []string
		expectImages  map[string]string
	}{
		{
			name: "update single container",
			containers: []corev1.Container{
				{Name: "main", Image: "nginx:1.0"},
				{Name: "sidecar", Image: "envoy:1.0"},
			},
			images:        map[string]string{"main": "nginx:2.0"},
			expectUpdated: []string{"main"},
			expectImages:  map[string]string{"main": "nginx:2.0", "sidecar": "envoy:1.0"},
		},
		{
			name: "update multiple containers",
			containers: []corev1.Container{
				{Name: "main", Image: "nginx:1.0"},
				{Name: "sidecar", Image: "envoy:1.0"},
			},
			images:        map[string]string{"main": "nginx:2.0", "sidecar": "envoy:2.0"},
			expectUpdated: []string{"main", "sidecar"},
			expectImages:  map[string]string{"main": "nginx:2.0", "sidecar": "envoy:2.0"},
		},
		{
			name: "no matching container",
			containers: []corev1.Container{
				{Name: "main", Image: "nginx:1.0"},
			},
			images:        map[string]string{"nonexistent": "foo:1.0"},
			expectUpdated: nil,
			expectImages:  map[string]string{"main": "nginx:1.0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updated := updateContainerImages(tt.containers, tt.images)
			assert.Equal(t, tt.expectUpdated, updated)

			for _, c := range tt.containers {
				if expected, ok := tt.expectImages[c.Name]; ok {
					assert.Equal(t, expected, c.Image)
				}
			}
		})
	}
}

func TestPrintSandboxSetStatus(t *testing.T) {
	tests := []struct {
		name string
		sbs  *agentsv1alpha1.SandboxSet
	}{
		{
			name: "updating",
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sbs"},
				Spec:       agentsv1alpha1.SandboxSetSpec{Replicas: 3},
				Status:     agentsv1alpha1.SandboxSetStatus{UpdatedReplicas: 1, UpdatedAvailableReplicas: 1},
			},
		},
		{
			name: "complete",
			sbs: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sbs"},
				Spec:       agentsv1alpha1.SandboxSetSpec{Replicas: 3},
				Status:     agentsv1alpha1.SandboxSetStatus{UpdatedReplicas: 3, AvailableReplicas: 3, UpdatedAvailableReplicas: 3},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Just verify it doesn't panic
			printSandboxSetStatus(tt.sbs)
		})
	}
}

func TestWaitForSandboxSetUpdate(t *testing.T) {
	// Test --wait where SandboxSet is already complete
	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sbs", Namespace: "default"},
		Spec:       agentsv1alpha1.SandboxSetSpec{Replicas: 3},
		Status:     agentsv1alpha1.SandboxSetStatus{UpdatedReplicas: 3, AvailableReplicas: 3, UpdatedAvailableReplicas: 3},
	}

	cs := fake.NewSimpleClientset(sbs)
	globalOpts := &GlobalOptions{Namespace: "default"}

	err := waitForSandboxSetUpdate(cs.ApiV1alpha1(), context.TODO(), "default", "test-sbs", globalOpts)
	assert.NoError(t, err)
}

func TestWaitForSandboxSetUpdateTimeout(t *testing.T) {
	// Test --wait timeout when SandboxSet never completes
	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sbs-timeout", Namespace: "default"},
		Spec:       agentsv1alpha1.SandboxSetSpec{Replicas: 3},
		Status:     agentsv1alpha1.SandboxSetStatus{UpdatedReplicas: 0, AvailableReplicas: 0},
	}

	cs := fake.NewSimpleClientset(sbs)
	globalOpts := &GlobalOptions{Namespace: "default"}

	ctx, cancel := context.WithTimeout(context.TODO(), 100*time.Millisecond)
	defer cancel()

	err := waitForSandboxSetUpdate(cs.ApiV1alpha1(), ctx, "default", "test-sbs-timeout", globalOpts)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
}

func TestPrintSuoStatus(t *testing.T) {
	tests := []struct {
		name string
		suo  *agentsv1alpha1.SandboxUpdateOps
	}{
		{
			name: "updating",
			suo: &agentsv1alpha1.SandboxUpdateOps{
				ObjectMeta: metav1.ObjectMeta{Name: "suo-test"},
				Status: agentsv1alpha1.SandboxUpdateOpsStatus{
					Phase:            agentsv1alpha1.SandboxUpdateOpsUpdating,
					Replicas:         2,
					UpdatedReplicas:  0,
					UpdatingReplicas: 1,
					FailedReplicas:   0,
				},
			},
		},
		{
			name: "completed",
			suo: &agentsv1alpha1.SandboxUpdateOps{
				ObjectMeta: metav1.ObjectMeta{Name: "suo-test"},
				Status: agentsv1alpha1.SandboxUpdateOpsStatus{
					Phase:            agentsv1alpha1.SandboxUpdateOpsCompleted,
					Replicas:         2,
					UpdatedReplicas:  2,
					UpdatingReplicas: 0,
					FailedReplicas:   0,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Just verify it doesn't panic
			printSuoStatus(tt.suo)
		})
	}
}

func TestRunSuoStatusWithClient(t *testing.T) {
	tests := []struct {
		name        string
		suoName     string
		namespace   string
		suo         *agentsv1alpha1.SandboxUpdateOps
		expectError string
	}{
		{
			name:        "suo not found",
			suoName:     "nonexistent",
			namespace:   "default",
			suo:         nil,
			expectError: "failed to get sandboxupdateops",
		},
		{
			name:      "suo updating",
			suoName:   "suo-test",
			namespace: "default",
			suo: &agentsv1alpha1.SandboxUpdateOps{
				ObjectMeta: metav1.ObjectMeta{Name: "suo-test", Namespace: "default"},
				Spec:       agentsv1alpha1.SandboxUpdateOpsSpec{},
				Status: agentsv1alpha1.SandboxUpdateOpsStatus{
					Phase:            agentsv1alpha1.SandboxUpdateOpsUpdating,
					Replicas:         2,
					UpdatedReplicas:  1,
					UpdatingReplicas: 1,
				},
			},
		},
		{
			name:      "suo completed",
			suoName:   "suo-test",
			namespace: "default",
			suo: &agentsv1alpha1.SandboxUpdateOps{
				ObjectMeta: metav1.ObjectMeta{Name: "suo-test", Namespace: "default"},
				Spec:       agentsv1alpha1.SandboxUpdateOpsSpec{},
				Status: agentsv1alpha1.SandboxUpdateOpsStatus{
					Phase:            agentsv1alpha1.SandboxUpdateOpsCompleted,
					Replicas:         2,
					UpdatedReplicas:  2,
					UpdatingReplicas: 0,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			if tt.suo != nil {
				_, err := cs.ApiV1alpha1().Sandboxupdateops(tt.namespace).Create(
					context.TODO(), tt.suo, metav1.CreateOptions{},
				)
				assert.NoError(t, err)
			}
			globalOpts := &GlobalOptions{Namespace: tt.namespace}

			err := runSuoStatusWithClient(cs.ApiV1alpha1(), globalOpts, tt.suoName)

			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
