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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakedynamic "k8s.io/client-go/dynamic/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned/fake"
)

func newDynamicScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "apps.kruise.io", Version: "v1alpha1", Kind: "ContainerRecreateRequest"},
		&unstructured.Unstructured{},
	)
	s.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "apps.kruise.io", Version: "v1alpha1", Kind: "ContainerRecreateRequestList"},
		&unstructured.UnstructuredList{},
	)
	return s
}

func TestRestartSandbox(t *testing.T) {
	inlineSandbox := func() *agentsv1alpha1.Sandbox {
		return &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-sbx",
				Namespace: "default",
			},
			Spec: agentsv1alpha1.SandboxSpec{
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

	templateRefSandbox := func() *agentsv1alpha1.Sandbox {
		return &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ref-sbx",
				Namespace: "default",
			},
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					TemplateRef: &agentsv1alpha1.SandboxTemplateRef{Name: "my-template"},
				},
			},
		}
	}

	tests := []struct {
		name           string
		sandboxName    string
		namespace      string
		containers     []string
		seedSandboxes  []*agentsv1alpha1.Sandbox
		expectError    string
		expectCreated  bool
		expectContains []string
	}{
		{
			name:           "restart specific container",
			sandboxName:    "test-sbx",
			namespace:      "default",
			containers:     []string{"main"},
			seedSandboxes:  []*agentsv1alpha1.Sandbox{inlineSandbox()},
			expectCreated:  true,
			expectContains: []string{"main"},
		},
		{
			name:           "restart multiple containers",
			sandboxName:    "test-sbx",
			namespace:      "default",
			containers:     []string{"main", "sidecar"},
			seedSandboxes:  []*agentsv1alpha1.Sandbox{inlineSandbox()},
			expectCreated:  true,
			expectContains: []string{"main", "sidecar"},
		},
		{
			name:           "restart all containers when none specified",
			sandboxName:    "test-sbx",
			namespace:      "default",
			containers:     nil,
			seedSandboxes:  []*agentsv1alpha1.Sandbox{inlineSandbox()},
			expectCreated:  true,
			expectContains: []string{"main", "sidecar"},
		},
		{
			name:        "container not found",
			sandboxName: "test-sbx",
			namespace:   "default",
			containers:  []string{"nonexistent"},
			seedSandboxes: []*agentsv1alpha1.Sandbox{inlineSandbox()},
			expectError: "container \"nonexistent\" not found",
		},
		{
			name:        "sandbox not found",
			sandboxName: "nonexistent",
			namespace:   "default",
			containers:  []string{"main"},
			expectError: "failed to get sandbox",
		},
		{
			name:        "templateRef sandbox without -c flag",
			sandboxName: "ref-sbx",
			namespace:   "default",
			containers:  nil,
			seedSandboxes: []*agentsv1alpha1.Sandbox{templateRefSandbox()},
			expectError: "uses a TemplateRef",
		},
		{
			name:           "templateRef sandbox with explicit -c flag",
			sandboxName:    "ref-sbx",
			namespace:      "default",
			containers:     []string{"main"},
			seedSandboxes:  []*agentsv1alpha1.Sandbox{templateRefSandbox()},
			expectCreated:  true,
			expectContains: []string{"main"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agentsCS := fake.NewSimpleClientset()
			for _, sbx := range tt.seedSandboxes {
				_, err := agentsCS.ApiV1alpha1().Sandboxes(sbx.Namespace).Create(
					context.TODO(), sbx, metav1.CreateOptions{},
				)
				assert.NoError(t, err)
			}

			dynCS := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(
				newDynamicScheme(),
				map[schema.GroupVersionResource]string{
					containerRecreateRequestGVR: "SandboxContainerRestartList",
				},
			)

			o := &restartOptions{
				global: &GlobalOptions{
					Namespace: tt.namespace,
				},
				containers: tt.containers,
			}

			err := runRestartWithClients(agentsCS.ApiV1alpha1(), dynCS, o, tt.sandboxName)

			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}

			if tt.expectCreated {
				list, listErr := dynCS.Resource(containerRecreateRequestGVR).Namespace(tt.namespace).List(
					context.TODO(), metav1.ListOptions{},
				)
				assert.NoError(t, listErr)
				assert.Len(t, list.Items, 1)

				created := list.Items[0]
				spec, _ := created.Object["spec"].(map[string]interface{})
				assert.Equal(t, tt.sandboxName, spec["podName"])

				containers, _ := spec["containers"].([]interface{})
				var containerNames []string
				for _, c := range containers {
					cm, _ := c.(map[string]interface{})
					containerNames = append(containerNames, cm["name"].(string))
				}
				assert.Equal(t, tt.expectContains, containerNames)
			}
		})
	}
}

func TestExtractContainerNames(t *testing.T) {
	tests := []struct {
		name        string
		sandbox     *agentsv1alpha1.Sandbox
		expected    []string
		expectError string
	}{
		{
			name: "inline template with containers",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "app"},
									{Name: "sidecar"},
								},
							},
						},
					},
				},
			},
			expected: []string{"app", "sidecar"},
		},
		{
			name: "templateRef returns error",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "ref-test"},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &agentsv1alpha1.SandboxTemplateRef{Name: "tpl"},
					},
				},
			},
			expectError: "uses a TemplateRef",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := extractContainerNames(tt.sandbox)

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

func TestValidateContainerNames(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						InitContainers: []corev1.Container{{Name: "init"}},
						Containers:     []corev1.Container{{Name: "main"}, {Name: "sidecar"}},
					},
				},
			},
		},
	}

	tests := []struct {
		name        string
		containers  []string
		expectError string
	}{
		{
			name:       "valid container",
			containers: []string{"main"},
		},
		{
			name:       "valid init container",
			containers: []string{"init"},
		},
		{
			name:       "multiple valid containers",
			containers: []string{"main", "sidecar", "init"},
		},
		{
			name:        "unknown container",
			containers:  []string{"unknown"},
			expectError: "container \"unknown\" not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateContainerNames(sbx, tt.containers)

			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
