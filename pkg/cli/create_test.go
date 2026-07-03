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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned/fake"
)

func TestCreateSuo(t *testing.T) {
	tests := []struct {
		name        string
		selector    string
		imageArgs   []string
		expectError string
		seedSandbox bool
	}{
		{
			name:        "update single container via SUO",
			selector:    "app=my-app",
			imageArgs:   []string{"main=nginx:2.0"},
			seedSandbox: true,
		},
		{
			name:        "update multiple containers via SUO",
			selector:    "app=my-app",
			imageArgs:   []string{"main=nginx:2.0", "sidecar=envoy:2.0"},
			seedSandbox: true,
		},
		{
			name:        "missing selector",
			selector:    "",
			imageArgs:   []string{"main=nginx:2.0"},
			expectError: "--selector (-l) is required",
		},
		{
			name:        "invalid image argument format",
			selector:    "app=my-app",
			imageArgs:   []string{"bad-format"},
			expectError: "invalid container=image argument",
		},
		{
			name:        "no sandboxes match selector",
			selector:    "app=nonexistent",
			imageArgs:   []string{"main=nginx:2.0"},
			expectError: "no sandboxes found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset()

			if tt.seedSandbox {
				sbx := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
						Labels:    map[string]string{"app": "my-app"},
					},
				}
				_, createErr := cs.ApiV1alpha1().Sandboxes("default").Create(
					context.TODO(), sbx, metav1.CreateOptions{},
				)
				assert.NoError(t, createErr)
			}

			opts := &createSuoOptions{
				global: &GlobalOptions{
					Namespace: "default",
				},
				selector: tt.selector,
			}

			err := runCreateSuoWithClient(cs.ApiV1alpha1(), opts, tt.imageArgs)

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
