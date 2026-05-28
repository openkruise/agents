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

package proxyutils

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

func TestGetRuntimeURL(t *testing.T) {
	tests := []struct {
		name     string
		sandbox  *agentsv1alpha1.Sandbox
		expected string
	}{
		{
			name:     "nil sandbox returns empty string",
			sandbox:  nil,
			expected: "",
		},
		{
			name: "runtime-url annotation hits and is returned directly",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationRuntimeURL: "http://runtime.example.com",
					},
				},
				Status: agentsv1alpha1.SandboxStatus{
					PodInfo: agentsv1alpha1.PodInfo{PodIP: "1.2.3.4"},
				},
			},
			expected: "http://runtime.example.com",
		},
		{
			name: "legacy envd-url annotation hits when runtime-url missing",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationEnvdURL: "http://envd.example.com",
					},
				},
			},
			expected: "http://envd.example.com",
		},
		{
			name: "runtime-url takes precedence over legacy envd-url",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationRuntimeURL: "http://runtime.example.com",
						agentsv1alpha1.AnnotationEnvdURL:    "http://envd.example.com",
					},
				},
			},
			expected: "http://runtime.example.com",
		},
		{
			name: "no annotation but PodIP present falls back to ip:port",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "10.0.0.1",
					},
				},
			},
			expected: fmt.Sprintf("http://10.0.0.1:%d", utils.RuntimePort),
		},
		{
			name: "empty annotation value falls back to ip:port",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationRuntimeURL: "",
						agentsv1alpha1.AnnotationEnvdURL:    "",
					},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "10.0.0.2",
					},
				},
			},
			expected: fmt.Sprintf("http://10.0.0.2:%d", utils.RuntimePort),
		},
		{
			name:     "no annotation and no PodIP returns empty string",
			sandbox:  &agentsv1alpha1.Sandbox{},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, GetRuntimeURL(tt.sandbox))
		})
	}
}
