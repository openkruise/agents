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

func TestScaleSandboxSet(t *testing.T) {
	baseSandboxSet := func() *agentsv1alpha1.SandboxSet {
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
							Containers: []corev1.Container{
								{Name: "main", Image: "nginx:1.0"},
							},
						},
					},
				},
			},
		}
	}

	tests := []struct {
		name        string
		sbsName     string
		namespace   string
		replicas    int32
		objects     []runtime.Object
		expectError string
	}{
		{
			name:      "scale up successfully",
			sbsName:   "test-sbs",
			namespace: "default",
			replicas:  5,
			objects:   []runtime.Object{baseSandboxSet()},
		},
		{
			name:      "scale down to zero",
			sbsName:   "test-sbs",
			namespace: "default",
			replicas:  0,
			objects:   []runtime.Object{baseSandboxSet()},
		},
		{
			name:        "negative replicas",
			sbsName:     "test-sbs",
			namespace:   "default",
			replicas:    -1,
			objects:     []runtime.Object{baseSandboxSet()},
			expectError: "--replicas must be >= 0",
		},
		{
			name:        "sandboxset not found",
			sbsName:     "nonexistent",
			namespace:   "default",
			replicas:    2,
			objects:     []runtime.Object{},
			expectError: "not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset(tt.objects...)

			opts := &scaleOptions{
				global: &GlobalOptions{
					Namespace: tt.namespace,
				},
				replicas: tt.replicas,
			}

			// Override AgentsClient by calling the fake client directly.
			err := runScaleWithClient(cs.ApiV1alpha1(), opts, tt.sbsName)

			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)

				// Verify the patch was applied.
				updated, getErr := cs.ApiV1alpha1().SandboxSets(tt.namespace).Get(
					context.TODO(), tt.sbsName, metav1.GetOptions{},
				)
				assert.NoError(t, getErr)
				assert.Equal(t, tt.replicas, updated.Spec.Replicas)
			}
		})
	}
}
