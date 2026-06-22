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
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned/fake"
)

func TestCreateSuo(t *testing.T) {
	makeSandbox := func(name string, labels map[string]string, containers []corev1.Container) *agentsv1alpha1.Sandbox {
		return &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels:    labels,
			},
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: containers,
						},
					},
				},
			},
		}
	}

	tests := []struct {
		name          string
		selector      string
		imageArgs     []string
		seedSandboxes []*agentsv1alpha1.Sandbox
		expectError   string
	}{
		{
			name:      "update single container via SUO",
			selector:  "app=my-app",
			imageArgs: []string{"main=nginx:2.0"},
			seedSandboxes: []*agentsv1alpha1.Sandbox{
				makeSandbox("sbx-1", map[string]string{"app": "my-app"}, []corev1.Container{
					{Name: "main", Image: "nginx:1.0"},
					{Name: "sidecar", Image: "envoy:1.0"},
				}),
			},
		},
		{
			name:      "update multiple containers via SUO",
			selector:  "app=my-app",
			imageArgs: []string{"main=nginx:2.0", "sidecar=envoy:2.0"},
			seedSandboxes: []*agentsv1alpha1.Sandbox{
				makeSandbox("sbx-1", map[string]string{"app": "my-app"}, []corev1.Container{
					{Name: "main", Image: "nginx:1.0"},
					{Name: "sidecar", Image: "envoy:1.0"},
				}),
			},
		},
		{
			name:          "missing selector",
			selector:      "",
			imageArgs:     []string{"main=nginx:2.0"},
			seedSandboxes: nil,
			expectError:   "--selector (-l) is required",
		},
		{
			name:          "no matching sandboxes",
			selector:      "app=nonexistent",
			imageArgs:     []string{"main=nginx:2.0"},
			seedSandboxes: nil,
			expectError:   "no sandboxes found",
		},
		{
			name:      "container not found in sandbox",
			selector:  "app=my-app",
			imageArgs: []string{"nonexistent=foo:1.0"},
			seedSandboxes: []*agentsv1alpha1.Sandbox{
				makeSandbox("sbx-1", map[string]string{"app": "my-app"}, []corev1.Container{
					{Name: "main", Image: "nginx:1.0"},
				}),
			},
			expectError: "container \"nonexistent\" not found",
		},
		{
			name:          "invalid image argument format",
			selector:      "app=my-app",
			imageArgs:     []string{"bad-format"},
			seedSandboxes: nil,
			expectError:   "invalid container=image argument",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			for _, sbx := range tt.seedSandboxes {
				_, err := cs.ApiV1alpha1().Sandboxes(sbx.Namespace).Create(
					context.TODO(), sbx, metav1.CreateOptions{},
				)
				assert.NoError(t, err)
			}

			o := &createSuoOptions{
				global: &GlobalOptions{
					Namespace: "default",
				},
				selector: tt.selector,
			}

			err := runCreateSuoWithClient(cs.ApiV1alpha1(), o, tt.imageArgs)

			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)

				// Verify SandboxUpdateOps was created
				suoList, listErr := cs.ApiV1alpha1().Sandboxupdateops("default").List(
					context.TODO(), metav1.ListOptions{},
				)
				assert.NoError(t, listErr)
				assert.Len(t, suoList.Items, 1, "expected exactly one SandboxUpdateOps")

				suo := suoList.Items[0]
				assert.NotNil(t, suo.Spec.Selector)
				assert.NotEmpty(t, suo.Spec.Patch.Raw, "patch should not be empty")
			}
		})
	}
}

func TestDeleteActiveSandboxUpdateOps(t *testing.T) {
	tests := []struct {
		name          string
		existingSUOs  []*agentsv1alpha1.SandboxUpdateOps
		expectDeleted int
		expectError   string
	}{
		{
			name:          "no existing SUOs",
			existingSUOs:  nil,
			expectDeleted: 0,
		},
		{
			name: "delete Pending SUO",
			existingSUOs: []*agentsv1alpha1.SandboxUpdateOps{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "suo-pending", Namespace: "default"},
					Spec:       agentsv1alpha1.SandboxUpdateOpsSpec{Selector: &metav1.LabelSelector{}},
					Status:     agentsv1alpha1.SandboxUpdateOpsStatus{Phase: agentsv1alpha1.SandboxUpdateOpsPending},
				},
			},
			expectDeleted: 1,
		},
		{
			name: "delete Updating SUO",
			existingSUOs: []*agentsv1alpha1.SandboxUpdateOps{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "suo-updating", Namespace: "default"},
					Spec:       agentsv1alpha1.SandboxUpdateOpsSpec{Selector: &metav1.LabelSelector{}},
					Status:     agentsv1alpha1.SandboxUpdateOpsStatus{Phase: agentsv1alpha1.SandboxUpdateOpsUpdating},
				},
			},
			expectDeleted: 1,
		},
		{
			name: "delete Completed SUO",
			existingSUOs: []*agentsv1alpha1.SandboxUpdateOps{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "suo-completed", Namespace: "default"},
					Spec:       agentsv1alpha1.SandboxUpdateOpsSpec{Selector: &metav1.LabelSelector{}},
					Status:     agentsv1alpha1.SandboxUpdateOpsStatus{Phase: agentsv1alpha1.SandboxUpdateOpsCompleted},
				},
			},
			expectDeleted: 1,
		},
		{
			name: "delete Failed SUO",
			existingSUOs: []*agentsv1alpha1.SandboxUpdateOps{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "suo-failed", Namespace: "default"},
					Spec:       agentsv1alpha1.SandboxUpdateOpsSpec{Selector: &metav1.LabelSelector{}},
					Status:     agentsv1alpha1.SandboxUpdateOpsStatus{Phase: agentsv1alpha1.SandboxUpdateOpsFailed},
				},
			},
			expectDeleted: 1,
		},
		{
			name: "delete multiple SUOs of different states",
			existingSUOs: []*agentsv1alpha1.SandboxUpdateOps{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "suo-1", Namespace: "default"},
					Spec:       agentsv1alpha1.SandboxUpdateOpsSpec{Selector: &metav1.LabelSelector{}},
					Status:     agentsv1alpha1.SandboxUpdateOpsStatus{Phase: agentsv1alpha1.SandboxUpdateOpsUpdating},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "suo-2", Namespace: "default"},
					Spec:       agentsv1alpha1.SandboxUpdateOpsSpec{Selector: &metav1.LabelSelector{}},
					Status:     agentsv1alpha1.SandboxUpdateOpsStatus{Phase: agentsv1alpha1.SandboxUpdateOpsCompleted},
				},
			},
			expectDeleted: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			for _, suo := range tt.existingSUOs {
				_, err := cs.ApiV1alpha1().Sandboxupdateops("default").Create(
					context.TODO(), suo, metav1.CreateOptions{},
				)
				assert.NoError(t, err)
			}

			err := deleteActiveSandboxUpdateOps(cs.ApiV1alpha1(), "default")

			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)

				// Verify all SUOs were deleted
				suoList, listErr := cs.ApiV1alpha1().Sandboxupdateops("default").List(
					context.TODO(), metav1.ListOptions{},
				)
				assert.NoError(t, listErr)
				assert.Len(t, suoList.Items, 0, "all SUOs should be deleted")
			}
		})
	}
}

func TestRemoveSUOFinalizer(t *testing.T) {
	cs := fake.NewSimpleClientset()
	suo := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-suo",
			Namespace:  "default",
			Finalizers: []string{"agents.kruise.io/sandboxupdateops-protection"},
		},
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{Selector: &metav1.LabelSelector{}},
	}
	_, err := cs.ApiV1alpha1().Sandboxupdateops("default").Create(context.TODO(), suo, metav1.CreateOptions{})
	assert.NoError(t, err)

	err = removeSUOFinalizer(cs.ApiV1alpha1(), "default", "test-suo")
	assert.NoError(t, err)

	// Verify finalizer was removed
	updated, getErr := cs.ApiV1alpha1().Sandboxupdateops("default").Get(context.TODO(), "test-suo", metav1.GetOptions{})
	assert.NoError(t, getErr)
	assert.Empty(t, updated.Finalizers, "finalizers should be empty after removal")
}

func TestWaitForSUODeletion(t *testing.T) {
	tests := []struct {
		name        string
		existSUO    bool
		expectError string
	}{
		{
			name:     "SUO already deleted (not found)",
			existSUO: false,
		},
		{
			name:     "SUO exists then gets deleted during wait",
			existSUO: true, // fake client returns "not found" after Delete is called
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			if tt.existSUO {
				suo := &agentsv1alpha1.SandboxUpdateOps{
					ObjectMeta: metav1.ObjectMeta{Name: "suo-wait", Namespace: "default"},
					Spec:       agentsv1alpha1.SandboxUpdateOpsSpec{Selector: &metav1.LabelSelector{}},
				}
				_, err := cs.ApiV1alpha1().Sandboxupdateops("default").Create(context.TODO(), suo, metav1.CreateOptions{})
				assert.NoError(t, err)
				// Delete it so waitForSUODeletion finds it "not found"
				err = cs.ApiV1alpha1().Sandboxupdateops("default").Delete(context.TODO(), "suo-wait", metav1.DeleteOptions{})
				assert.NoError(t, err)
			}

			err := waitForSUODeletion(cs.ApiV1alpha1(), "default", "suo-wait")

			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestWaitForSandboxSetUpdateImmediateComplete(t *testing.T) {
	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sbs", Namespace: "default"},
		Spec:       agentsv1alpha1.SandboxSetSpec{Replicas: 3},
		Status: agentsv1alpha1.SandboxSetStatus{
			UpdatedReplicas:          3,
			AvailableReplicas:        3,
			UpdatedAvailableReplicas: 3,
		},
	}

	cs := fake.NewSimpleClientset(sbs)
	globalOpts := &GlobalOptions{Namespace: "default"}

	err := waitForSandboxSetUpdate(cs.ApiV1alpha1(), context.TODO(), "default", "test-sbs", globalOpts)
	assert.NoError(t, err)
}

func TestFormatSuoImagePairs(t *testing.T) {
	images := map[string]string{"app": "nginx:2.0", "sidecar": "envoy:2.0"}
	pairs := formatSuoImagePairs(images)
	assert.Len(t, pairs, 2)
	// Since map iteration order is not guaranteed, check both possibilities
	assert.Contains(t, pairs, "app=nginx:2.0")
	assert.Contains(t, pairs, "sidecar=envoy:2.0")
}

func TestValidateSuoImageContainers(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sbx", Namespace: "default"},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "app", Image: "nginx:1.0"},
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name        string
		images      map[string]string
		expectError string
	}{
		{
			name:   "container exists",
			images: map[string]string{"app": "nginx:2.0"},
		},
		{
			name:        "container not found",
			images:      map[string]string{"nonexistent": "nginx:2.0"},
			expectError: "container \"nonexistent\" not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSuoImageContainers(sbx, tt.images)
			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestCreateSuoCleansUpExistingSUOs(t *testing.T) {
	// Seed an existing Updating SUO and a matching sandbox
	cs := fake.NewSimpleClientset()

	existingSUO := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Name: "old-suo", Namespace: "default"},
		Spec:       agentsv1alpha1.SandboxUpdateOpsSpec{Selector: &metav1.LabelSelector{}},
		Status:     agentsv1alpha1.SandboxUpdateOpsStatus{Phase: agentsv1alpha1.SandboxUpdateOpsUpdating},
	}
	_, err := cs.ApiV1alpha1().Sandboxupdateops("default").Create(context.TODO(), existingSUO, metav1.CreateOptions{})
	assert.NoError(t, err)

	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "my-app"},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "main", Image: "nginx:1.0"}},
					},
				},
			},
		},
	}
	_, err = cs.ApiV1alpha1().Sandboxes("default").Create(context.TODO(), sbx, metav1.CreateOptions{})
	assert.NoError(t, err)

	o := &createSuoOptions{
		global:   &GlobalOptions{Namespace: "default"},
		selector: "app=my-app",
	}

	err = runCreateSuoWithClient(cs.ApiV1alpha1(), o, []string{"main=nginx:2.0"})
	assert.NoError(t, err)

	// Verify old SUO was deleted and new one was created
	suoList, listErr := cs.ApiV1alpha1().Sandboxupdateops("default").List(context.TODO(), metav1.ListOptions{})
	assert.NoError(t, listErr)
	assert.Len(t, suoList.Items, 1, "old SUO should be deleted, new one created")
	assert.NotEqual(t, "old-suo", suoList.Items[0].Name, "new SUO should have a different name")
}

func TestBuildSuoImagePatch(t *testing.T) {
	tests := []struct {
		name     string
		images   map[string]string
		contains string
	}{
		{
			name:     "single container",
			images:   map[string]string{"app": "nginx:2.0"},
			contains: `"name":"app"`,
		},
		{
			name:     "multiple containers",
			images:   map[string]string{"app": "nginx:2.0", "sidecar": "envoy:2.0"},
			contains: `"name":"app"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := buildSuoImagePatch(tt.images)
			assert.NoError(t, err)
			assert.Contains(t, string(data), tt.contains)
			assert.Contains(t, string(data), `"containers"`)
			assert.NotContains(t, string(data), `"template"`, "patch should not contain 'template' layer - SUO patch is applied directly to PodTemplateSpec")
		})
	}
}

func TestParseSuoSelectorToMap(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		expected map[string]string
	}{
		{
			name:     "single pair",
			selector: "app=my-app",
			expected: map[string]string{"app": "my-app"},
		},
		{
			name:     "multiple pairs",
			selector: "app=my-app,env=prod",
			expected: map[string]string{"app": "my-app", "env": "prod"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseSuoSelectorToMap(tt.selector)
			assert.Equal(t, tt.expected, result)
		})
	}
}
