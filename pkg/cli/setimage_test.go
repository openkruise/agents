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
	"k8s.io/apimachinery/pkg/runtime"

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
		name           string
		sbsName        string
		namespace      string
		imageArgs      []string
		objects        []runtime.Object
		expectError    string
		expectedImages map[string]string
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
			name:        "container not found",
			sbsName:     "test-sbs",
			namespace:   "default",
			imageArgs:   []string{"nonexistent=foo:1.0"},
			objects:     []runtime.Object{inlineSandboxSet()},
			expectError: "container \"nonexistent\" not found",
		},
		{
			name:        "sandboxset not found",
			sbsName:     "nonexistent",
			namespace:   "default",
			imageArgs:   []string{"main=nginx:2.0"},
			objects:     []runtime.Object{},
			expectError: "failed to get sandboxset",
		},
		{
			name:        "sandboxset uses TemplateRef",
			sbsName:     "ref-sbs",
			namespace:   "default",
			imageArgs:   []string{"main=nginx:2.0"},
			objects:     []runtime.Object{templateRefSandboxSet()},
			expectError: "uses a TemplateRef",
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

			o := &setImageOptions{
				global: &GlobalOptions{
					Namespace: tt.namespace,
				},
			}

			err := runSetImageWithClient(cs.ApiV1alpha1(), o, tt.sbsName, tt.imageArgs)

			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
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
			result, err := parseContainerImages(tt.args)

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
