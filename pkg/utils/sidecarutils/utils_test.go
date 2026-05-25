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

package sidecarutils

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

func TestInjectPodTemplateCSIAndRuntimeSidecar(t *testing.T) {
	tests := []struct {
		name                   string
		sandbox                *agentsv1alpha1.Sandbox
		pod                    *corev1.Pod
		injectionConfigData    map[string]string
		expectInjection        bool
		expectRuntimeContainer bool
		expectCSIContainer     bool
		expectRuntimeEnvCount  int
		expectCSIVolumeMounts  int
		expectEgressContainer  bool
		expectInitContainers   int // expected InitContainer count after injection
		expectContainers       int // expected Container count after injection
		initialContainerCount  int // base Container count before injection (default 1)
	}{
		{
			name: "no injection needed",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx"},
					},
				},
			},
			expectInjection: false,
		},
		{
			name: "inject runtime sidecar only",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{},
					},
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime,
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx"},
					},
				},
			},
			injectionConfigData: map[string]string{
				KEY_RUNTIME_INJECTION_CONFIG: `{
					"mainContainer": {
						"name": "",
						"env": [
							{"name": "RUNTIME_ENV", "value": "test"},
							{"name": "DEBUG", "value": "true"}
						],
						"volumeMounts": []
					},
					"csiSidecar": [{
						"name": "runtime-sidecar",
						"image": "runtime:v1"
					}],
					"volume": []
				}`,
			},
			expectInjection:        true,
			expectRuntimeContainer: true,
			expectRuntimeEnvCount:  2,
		},
		{
			name: "inject csi sidecar only",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{},
					},
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount,
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx"},
					},
				},
			},
			injectionConfigData: map[string]string{
				KEY_CSI_INJECTION_CONFIG: `{
					"mainContainer": {
						"name": "",
						"volumeMounts": [
							{"name": "csi-volume", "mountPath": "/mnt/csi"},
							{"name": "data-volume", "mountPath": "/data"}
						]
					},
					"csiSidecar": [{
						"name": "csi-sidecar",
						"image": "csi:v1"
					}],
					"volume": [
						{"name": "csi-volume", "emptyDir": {}},
						{"name": "data-volume", "emptyDir": {}}
					]
				}`,
			},
			expectInjection:       true,
			expectCSIContainer:    true,
			expectCSIVolumeMounts: 2,
		},
		{
			name: "inject both runtime and csi sidecars",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{},
					},
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime,
						},
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount,
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx"},
					},
				},
			},
			injectionConfigData: map[string]string{
				KEY_RUNTIME_INJECTION_CONFIG: `{
					"mainContainer": {
						"name": "",
						"env": [{"name": "RUNTIME", "value": "enabled"}],
						"volumeMounts": []
					},
					"csiSidecar": [{
						"name": "runtime-sidecar",
						"image": "runtime:v1"
					}],
					"volume": []
				}`,
				KEY_CSI_INJECTION_CONFIG: `{
					"mainContainer": {
						"name": "",
						"volumeMounts": [{"name": "csi-vol", "mountPath": "/csi"}]
					},
					"csiSidecar": [{
						"name": "csi-sidecar",
						"image": "csi:v1"
					}],
					"volume": [{"name": "csi-vol", "emptyDir": {}}]
				}`,
			},
			expectInjection:        true,
			expectRuntimeContainer: true,
			expectCSIContainer:     true,
			expectRuntimeEnvCount:  1,
			expectCSIVolumeMounts:  1,
		},
		{
			name: "inject traffic proxy sidecar only",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{},
					},
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectTrafficProxy,
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx"},
					},
				},
			},
			injectionConfigData: map[string]string{
				KEY_TRAFFIC_PROXY_INJECTION_CONFIG: `{
					"mainContainer": {},
					"csiSidecar": [],
					"volume": [{"name": "egress-vol", "emptyDir": {}}],
					"initContainers": [{"name": "egress-init", "image": "egress-init:v1"}],
					"containers": [{"name": "egress-sidecar", "image": "egress:v1"}],
					"labels": {"egress": "enabled"},
					"annotations": {"traffic-proxy": "true"}
				}`,
			},
			// Note: InjectPodTemplateCSIAndRuntimeSidecar does not return early when only egress is enabled;
			// it proceeds with egress injection via doSidecarInjection.
			expectInjection:       true,
			expectEgressContainer: true,
			expectContainers:      2, // main + egress sidecar
		},
		{
			name: "inject traffic proxy with health probe rewrite - early return without CSI/runtime",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{},
					},
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectTrafficProxy,
						},
					},
				},
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"networking.agents.kruise.io/health-probe-rewrite": "true",
						"networking.agents.kruise.io/sidecar-proxy":        "istio-proxy",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "nginx",
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt(8080),
									},
								},
							},
						},
						{
							Name:  "istio-proxy",
							Image: "istio/proxyv2:latest",
						},
					},
				},
			},
			injectionConfigData: map[string]string{
				KEY_TRAFFIC_PROXY_INJECTION_CONFIG: `{
					"mainContainer": {},
					"csiSidecar": [],
					"volume": [],
					"initContainers": [],
					"containers": [],
					"labels": {},
					"annotations": {}
				}`,
			},
			// Egress injection proceeds but with empty config, so pod remains unchanged.
			expectInjection:       true,
			expectEgressContainer: false,
			initialContainerCount: 2, // main + istio-proxy
		},
		{
			name: "inject all three: runtime, csi, and traffic proxy",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{},
					},
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime,
						},
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount,
						},
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectTrafficProxy,
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx"},
					},
				},
			},
			injectionConfigData: map[string]string{
				KEY_RUNTIME_INJECTION_CONFIG: `{
					"mainContainer": {"env": [{"name": "RUNTIME", "value": "on"}], "volumeMounts": []},
					"csiSidecar": [{"name": "runtime-sidecar", "image": "runtime:v1"}],
					"volume": []
				}`,
				KEY_CSI_INJECTION_CONFIG: `{
					"mainContainer": {"volumeMounts": [{"name": "csi-vol", "mountPath": "/csi"}]},
					"csiSidecar": [{"name": "csi-sidecar", "image": "csi:v1"}],
					"volume": [{"name": "csi-vol", "emptyDir": {}}]
				}`,
				KEY_TRAFFIC_PROXY_INJECTION_CONFIG: `{
					"mainContainer": {},
					"csiSidecar": [],
					"volume": [{"name": "egress-vol", "emptyDir": {}}],
					"initContainers": [],
					"containers": [{"name": "egress-sidecar", "image": "egress:v1"}],
					"labels": {"egress": "enabled"},
					"annotations": {}
				}`,
			},
			expectInjection:        true,
			expectRuntimeContainer: true,
			expectCSIContainer:     true,
			expectEgressContainer:  true,
			expectRuntimeEnvCount:  1,
			expectCSIVolumeMounts:  1,
			expectInitContainers:   2, // runtime + csi sidecars
			expectContainers:       2, // main + egress container
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake client with configmap if needed
			var objs []client.Object
			if tt.injectionConfigData != nil {
				cm := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      SandboxInjectionConfigName,
						Namespace: utils.DefaultSandboxDeployNamespace,
					},
					Data: tt.injectionConfigData,
				}
				objs = append(objs, cm)
			}
			fakeClient := fake.NewClientBuilder().WithObjects(objs...).Build()
			ctx := context.Background()

			err := InjectSandboxRuntimes(ctx, tt.sandbox, tt.pod, fakeClient)
			// Verify results
			if !tt.expectInjection {
				// Should return early without error
				if err != nil {
					t.Errorf("expected no error when no injection needed, got: %v", err)
				}
				// Pod template should not be modified
				initialContainers := 1
				if tt.initialContainerCount > 0 {
					initialContainers = tt.initialContainerCount
				}
				if len(tt.pod.Spec.Containers) != initialContainers {
					t.Errorf("expected %d containers (unchanged), got %d", initialContainers, len(tt.pod.Spec.Containers))
				}
				return
			}
			// Injection was expected
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// Count containers after injection
			runtimeContainerInjected := false
			csiContainerInjected := false
			// Check InitContainers for both runtime sidecar and csi sidecar (both are injected to InitContainers)
			// Use image to distinguish between pre-existing containers and injected containers
			for _, container := range tt.pod.Spec.InitContainers {
				// runtime sidecar injection uses image "runtime:v1"
				if container.Name == "runtime-sidecar" && container.Image == "runtime:v1" {
					runtimeContainerInjected = true
				}
				// csi sidecar injection uses image "csi:v1"
				if container.Name == "csi-sidecar" && container.Image == "csi:v1" {
					csiContainerInjected = true
				}
			}
			// Check main container in Containers
			mainContainerFound := false
			for _, container := range tt.pod.Spec.Containers {
				if container.Name == "main" {
					mainContainerFound = true
					// Check main container env count (from runtime injection)
					if tt.expectRuntimeEnvCount > 0 && len(container.Env) != tt.expectRuntimeEnvCount {
						t.Errorf("expected %d env vars in main container, got %d", tt.expectRuntimeEnvCount, len(container.Env))
					}
					// Check main container volume mounts (from csi injection)
					if tt.expectCSIVolumeMounts > 0 && len(container.VolumeMounts) != tt.expectCSIVolumeMounts {
						t.Errorf("expected %d volume mounts in main container, got %d", tt.expectCSIVolumeMounts, len(container.VolumeMounts))
					}
				}
			}
			// Verify expectations
			if tt.expectRuntimeContainer && !runtimeContainerInjected {
				t.Error("expected runtime sidecar container to be injected in InitContainers, but not found")
			}
			if tt.expectCSIContainer && !csiContainerInjected {
				t.Error("expected csi sidecar container to be injected in InitContainers, but not found")
			}
			// Main container should still exist
			if !mainContainerFound {
				t.Error("expected main container to still exist")
			}
			// Verify InitContainer and Container counts if specified
			if tt.expectInitContainers > 0 {
				if len(tt.pod.Spec.InitContainers) != tt.expectInitContainers {
					t.Errorf("expected %d InitContainers, got %d", tt.expectInitContainers, len(tt.pod.Spec.InitContainers))
				}
			}
			if tt.expectContainers > 0 {
				if len(tt.pod.Spec.Containers) != tt.expectContainers {
					t.Errorf("expected %d Containers, got %d", tt.expectContainers, len(tt.pod.Spec.Containers))
				}
			}
			// Count total containers (InitContainers + Containers)
			expectedTotal := 1
			if tt.initialContainerCount > 0 {
				expectedTotal = tt.initialContainerCount
			}
			if tt.expectRuntimeContainer {
				expectedTotal++
			}
			if tt.expectCSIContainer {
				expectedTotal++
			}
			if tt.expectEgressContainer {
				expectedTotal++
			}
			totalContainers := len(tt.pod.Spec.InitContainers) + len(tt.pod.Spec.Containers)
			if tt.expectInitContainers == 0 && tt.expectContainers == 0 {
				// Only verify total count when not explicitly checking init/container counts
				if totalContainers != expectedTotal {
					t.Errorf("expected %d total containers, got %d", expectedTotal, totalContainers)
				}
			}
		})
	}
}

func TestFetchInjectionConfiguration(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name          string
		configMap     *corev1.ConfigMap
		getError      error
		expectedData  map[string]string
		expectError   bool
		errorContains string
	}{
		{
			name: "successful fetch",
			configMap: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      SandboxInjectionConfigName,
					Namespace: getTestNamespace(),
				},
				Data: map[string]string{
					KEY_CSI_INJECTION_CONFIG:     `{"mainContainer":{},"csiSidecar":[],"volume":[]}`,
					KEY_RUNTIME_INJECTION_CONFIG: `{"mainContainer":{},"csiSidecar":[],"volume":[]}`,
				},
			},
			expectedData: map[string]string{
				KEY_CSI_INJECTION_CONFIG:     `{"mainContainer":{},"csiSidecar":[],"volume":[]}`,
				KEY_RUNTIME_INJECTION_CONFIG: `{"mainContainer":{},"csiSidecar":[],"volume":[]}`,
			},
			expectError: false,
		},
		{
			name:          "configmap not found",
			configMap:     nil,
			getError:      nil,
			expectedData:  nil,
			expectError:   false,
			errorContains: "",
		},
		{
			name: "empty configmap data",
			configMap: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      SandboxInjectionConfigName,
					Namespace: getTestNamespace(),
				},
				Data: map[string]string{},
			},
			expectedData: map[string]string{},
			expectError:  false,
		},
		{
			name: "configmap with partial data",
			configMap: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      SandboxInjectionConfigName,
					Namespace: getTestNamespace(),
				},
				Data: map[string]string{
					KEY_CSI_INJECTION_CONFIG: `{"mainContainer":{"env":[{"name":"ENV1","value":"val1"}]},"csiSidecar":[],"volume":[]}`,
				},
			},
			expectedData: map[string]string{
				KEY_CSI_INJECTION_CONFIG: `{"mainContainer":{"env":[{"name":"ENV1","value":"val1"}]},"csiSidecar":[],"volume":[]}`,
			},
			expectError: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake client
			initObjs := []client.Object{}
			if tt.configMap != nil {
				initObjs = append(initObjs, tt.configMap)
			}
			fakeClient := fake.NewClientBuilder().
				WithObjects(initObjs...).
				Build()
			// Call the function
			data, err := fetchInjectionConfiguration(ctx, fakeClient)
			// Verify results
			if tt.expectError {
				if err == nil {
					t.Error("expected error, got nil")
				} else if tt.errorContains != "" && !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("expected error to contain %q, got %q", tt.errorContains, err.Error())
				}
				if data != nil {
					t.Error("expected nil data on error, got non-nil")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				// Compare map lengths first for better error messages
				if len(data) != len(tt.expectedData) {
					t.Errorf("expected data length %d, got %d", len(tt.expectedData), len(data))
				}
				// Then compare individual keys and values
				for key, expectedValue := range tt.expectedData {
					if actualValue, exists := data[key]; !exists {
						t.Errorf("expected key %q to exist in data", key)
					} else if actualValue != expectedValue {
						t.Errorf("for key %q: expected value %q, got %q", key, expectedValue, actualValue)
					}
				}
			}
		})
	}
}

func TestDoSidecarInjection(t *testing.T) {
	tests := []struct {
		name                   string
		sandbox                *agentsv1alpha1.Sandbox
		pod                    *corev1.Pod
		injectConfigMap        map[string]string
		expectRuntimeContainer bool
		expectCSIContainer     bool
		expectMainEnvCount     int
		expectCSIVolumeCount   int
	}{
		{
			name: "runtime injection only",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime,
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx"},
					},
				},
			},
			injectConfigMap: map[string]string{
				KEY_RUNTIME_INJECTION_CONFIG: `{
					"mainContainer": {
						"name": "",
						"env": [
							{"name": "RUNTIME_ENV", "value": "test"},
							{"name": "DEBUG", "value": "true"}
						],
						"volumeMounts": []
					},
					"csiSidecar": [{
						"name": "runtime-sidecar",
						"image": "runtime:v1"
					}],
					"volume": []
				}`,
			},
			expectRuntimeContainer: true,
			expectMainEnvCount:     2,
		},
		{
			name: "csi injection only",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount,
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx"},
					},
				},
			},
			injectConfigMap: map[string]string{
				KEY_CSI_INJECTION_CONFIG: `{
					"mainContainer": {
						"name": "",
						"volumeMounts": [
							{"name": "csi-volume", "mountPath": "/mnt/csi"}
						]
					},
					"csiSidecar": [{
						"name": "csi-sidecar",
						"image": "csi:v1"
					}],
					"volume": [
						{"name": "csi-volume", "emptyDir": {}}
					]
				}`,
			},
			expectCSIContainer:   true,
			expectCSIVolumeCount: 1,
		},
		{
			name: "both runtime and csi injection",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime,
						},
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount,
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx"},
					},
				},
			},
			injectConfigMap: map[string]string{
				KEY_RUNTIME_INJECTION_CONFIG: `{
					"mainContainer": {
						"name": "",
						"env": [{"name": "RUNTIME", "value": "enabled"}],
						"volumeMounts": []
					},
					"csiSidecar": [{
						"name": "runtime-sidecar",
						"image": "runtime:v1"
					}],
					"volume": []
				}`,
				KEY_CSI_INJECTION_CONFIG: `{
					"mainContainer": {
						"name": "",
						"volumeMounts": [{"name": "csi-vol", "mountPath": "/csi"}]
					},
					"csiSidecar": [{
						"name": "csi-sidecar",
						"image": "csi:v1"
					}],
					"volume": [{"name": "csi-vol", "emptyDir": {}}]
				}`,
			},
			expectRuntimeContainer: true,
			expectCSIContainer:     true,
			expectMainEnvCount:     1,
			expectCSIVolumeCount:   1,
		},
		{
			name: "no injection needed - no annotations",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx"},
					},
				},
			},
			injectConfigMap: map[string]string{},
		},
		// Container name conflict test cases
		{
			name: "runtime sidecar name conflict with existing container - skip injection",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx"},
						{Name: "runtime-sidecar", Image: "existing:v1"}, // conflict with runtime sidecar in Containers
					},
				},
			},
			injectConfigMap: map[string]string{
				KEY_RUNTIME_INJECTION_CONFIG: `{
					"mainContainer": {
						"name": "",
						"env": [{"name": "RUNTIME_ENV", "value": "test"}],
						"volumeMounts": []
					},
					"csiSidecar": [{
						"name": "runtime-sidecar",
						"image": "runtime:v1"
					}],
					"volume": []
				}`,
			},
			expectRuntimeContainer: false, // should NOT inject due to conflict in Containers
			expectMainEnvCount:     0,
		},
		{
			name: "runtime sidecar name conflict with existing initContainer - skip injection",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{Name: "runtime-sidecar", Image: "existing-init:v1"}, // conflict in initContainers
					},
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx"},
					},
				},
			},
			injectConfigMap: map[string]string{
				KEY_RUNTIME_INJECTION_CONFIG: `{
					"mainContainer": {
						"name": "",
						"env": [{"name": "RUNTIME_ENV", "value": "test"}],
						"volumeMounts": []
					},
					"csiSidecar": [{
						"name": "runtime-sidecar",
						"image": "runtime:v1"
					}],
					"volume": []
				}`,
			},
			expectRuntimeContainer: false, // should NOT inject due to conflict in InitContainers
			expectMainEnvCount:     0,
		},
		{
			name: "csi sidecar name conflict with existing container - skip injection",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount,
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx"},
						{Name: "csi-sidecar", Image: "existing-csi:v1"}, // conflict with csi sidecar in Containers
					},
				},
			},
			injectConfigMap: map[string]string{
				KEY_CSI_INJECTION_CONFIG: `{
					"mainContainer": {
						"name": "",
						"volumeMounts": [{"name": "csi-volume", "mountPath": "/mnt/csi"}]
					},
					"csiSidecar": [{
						"name": "csi-sidecar",
						"image": "csi:v1"
					}],
					"volume": [{"name": "csi-volume", "emptyDir": {}}]
				}`,
			},
			expectCSIContainer:   false, // should NOT inject due to conflict in Containers
			expectCSIVolumeCount: 0,
		},
		{
			name: "csi sidecar name conflict with existing initContainer - skip injection",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount,
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{Name: "csi-sidecar", Image: "existing-init-csi:v1"}, // conflict in initContainers
					},
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx"},
					},
				},
			},
			injectConfigMap: map[string]string{
				KEY_CSI_INJECTION_CONFIG: `{
					"mainContainer": {
						"name": "",
						"volumeMounts": [{"name": "csi-volume", "mountPath": "/mnt/csi"}]
					},
					"csiSidecar": [{
						"name": "csi-sidecar",
						"image": "csi:v1"
					}],
					"volume": [{"name": "csi-volume", "emptyDir": {}}]
				}`,
			},
			expectCSIContainer:   false, // should NOT inject due to conflict in InitContainers
			expectCSIVolumeCount: 0,
		},
		{
			name: "both injections enabled but runtime conflicts in Containers - only csi injected",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime,
						},
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount,
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx"},
						{Name: "runtime-sidecar", Image: "existing:v1"}, // conflict with runtime sidecar
					},
				},
			},
			injectConfigMap: map[string]string{
				KEY_RUNTIME_INJECTION_CONFIG: `{
					"mainContainer": {
						"name": "",
						"env": [{"name": "RUNTIME", "value": "enabled"}],
						"volumeMounts": []
					},
					"csiSidecar": [{
						"name": "runtime-sidecar",
						"image": "runtime:v1"
					}],
					"volume": []
				}`,
				KEY_CSI_INJECTION_CONFIG: `{
					"mainContainer": {
						"name": "",
						"volumeMounts": [{"name": "csi-vol", "mountPath": "/csi"}]
					},
					"csiSidecar": [{
						"name": "csi-sidecar",
						"image": "csi:v1"
					}],
					"volume": [{"name": "csi-vol", "emptyDir": {}}]
				}`,
			},
			expectRuntimeContainer: false, // should NOT inject due to conflict
			expectCSIContainer:     true,  // should inject - no conflict
			expectMainEnvCount:     0,
			expectCSIVolumeCount:   1,
		},
		{
			name: "both injections enabled but csi conflicts in Containers - only runtime injected",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime,
						},
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount,
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx"},
						{Name: "csi-sidecar", Image: "existing-csi:v1"}, // conflict with csi sidecar
					},
				},
			},
			injectConfigMap: map[string]string{
				KEY_RUNTIME_INJECTION_CONFIG: `{
					"mainContainer": {
						"name": "",
						"env": [{"name": "RUNTIME", "value": "enabled"}],
						"volumeMounts": []
					},
					"csiSidecar": [{
						"name": "runtime-sidecar",
						"image": "runtime:v1"
					}],
					"volume": []
				}`,
				KEY_CSI_INJECTION_CONFIG: `{
					"mainContainer": {
						"name": "",
						"volumeMounts": [{"name": "csi-vol", "mountPath": "/csi"}]
					},
					"csiSidecar": [{
						"name": "csi-sidecar",
						"image": "csi:v1"
					}],
					"volume": [{"name": "csi-vol", "emptyDir": {}}]
				}`,
			},
			expectRuntimeContainer: true,  // should inject - no conflict
			expectCSIContainer:     false, // should NOT inject due to conflict
			expectMainEnvCount:     1,
			expectCSIVolumeCount:   0,
		},
		{
			name: "both injections enabled but both conflict in InitContainers - neither injected",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime,
						},
						{
							Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount,
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{Name: "runtime-sidecar", Image: "existing-init:v1"}, // conflict in initContainers
						{Name: "csi-sidecar", Image: "existing-init-csi:v1"}, // conflict in initContainers
					},
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx"},
					},
				},
			},
			injectConfigMap: map[string]string{
				KEY_RUNTIME_INJECTION_CONFIG: `{
					"mainContainer": {
						"name": "",
						"env": [{"name": "RUNTIME", "value": "enabled"}],
						"volumeMounts": []
					},
					"csiSidecar": [{
						"name": "runtime-sidecar",
						"image": "runtime:v1"
					}],
					"volume": []
				}`,
				KEY_CSI_INJECTION_CONFIG: `{
					"mainContainer": {
						"name": "",
						"volumeMounts": [{"name": "csi-vol", "mountPath": "/csi"}]
					},
					"csiSidecar": [{
						"name": "csi-sidecar",
						"image": "csi:v1"
					}],
					"volume": [{"name": "csi-vol", "emptyDir": {}}]
				}`,
			},
			expectRuntimeContainer: false, // should NOT inject due to conflict
			expectCSIContainer:     false, // should NOT inject due to conflict
			expectMainEnvCount:     0,
			expectCSIVolumeCount:   0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			// Call the function
			err := doSidecarInjection(ctx, tt.sandbox, tt.pod, tt.injectConfigMap)
			// Verify no error
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// Verify container injection - both runtime and csi sidecars are injected to InitContainers
			runtimeContainerInjected := false
			csiContainerInjected := false
			// Check InitContainers for both runtime sidecar and csi sidecar
			// Use image to distinguish between pre-existing containers (for conflict tests) and injected containers
			for _, container := range tt.pod.Spec.InitContainers {
				// runtime sidecar injection uses image "runtime:v1"
				if container.Name == "runtime-sidecar" && container.Image == "runtime:v1" {
					runtimeContainerInjected = true
				}
				// csi sidecar injection uses image "csi:v1"
				if container.Name == "csi-sidecar" && container.Image == "csi:v1" {
					csiContainerInjected = true
				}
			}
			// Check main container in Containers
			for _, container := range tt.pod.Spec.Containers {
				if container.Name == "main" {
					// Check main container env count
					if tt.expectMainEnvCount > 0 && len(container.Env) != tt.expectMainEnvCount {
						t.Errorf("expected %d env vars in main container, got %d", tt.expectMainEnvCount, len(container.Env))
					}
					// Check main container volume mounts
					if tt.expectCSIVolumeCount > 0 && len(container.VolumeMounts) != tt.expectCSIVolumeCount {
						t.Errorf("expected %d volume mounts in main container, got %d", tt.expectCSIVolumeCount, len(container.VolumeMounts))
					}
				}
			}
			// Verify sidecar containers
			if tt.expectRuntimeContainer && !runtimeContainerInjected {
				t.Error("expected runtime sidecar container to be injected in InitContainers, but not found")
			}
			if !tt.expectRuntimeContainer && runtimeContainerInjected {
				t.Error("expected NO runtime sidecar injection due to conflict, but injection occurred")
			}
			if tt.expectCSIContainer && !csiContainerInjected {
				t.Error("expected csi sidecar container to be injected in InitContainers, but not found")
			}
			if !tt.expectCSIContainer && csiContainerInjected {
				t.Error("expected NO csi sidecar injection due to conflict, but injection occurred")
			}
			// Verify volume injection
			if tt.expectCSIVolumeCount > 0 {
				actualVolumeCount := len(tt.pod.Spec.Volumes)
				if actualVolumeCount < tt.expectCSIVolumeCount {
					t.Errorf("expected at least %d volumes, got %d", tt.expectCSIVolumeCount, actualVolumeCount)
				}
			}
			// Main container should always exist
			mainContainerFound := false
			for _, container := range tt.pod.Spec.Containers {
				if container.Name == "main" {
					mainContainerFound = true
					break
				}
			}
			if !mainContainerFound {
				t.Error("expected main container to still exist")
			}
		})
	}
}

func TestInjectConflicts(t *testing.T) {
	tests := []struct {
		name          string
		pod           *corev1.Pod
		config        SidecarInjectConfig
		expectError   bool
		errorContains string
	}{
		{
			name: "no conflicts",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers:     []corev1.Container{{Name: "main", Image: "nginx"}},
					InitContainers: []corev1.Container{{Name: "init-main", Image: "busybox"}},
					Volumes:        []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
				},
			},
			config: SidecarInjectConfig{
				Containers:     []corev1.Container{{Name: "sidecar", Image: "proxy:v1"}},
				InitContainers: []corev1.Container{{Name: "init-sidecar", Image: "init:v1"}},
				Volumes:        []corev1.Volume{{Name: "sidecar-vol", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			},
			expectError: false,
		},
		{
			name: "container name conflict",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
				},
			},
			config: SidecarInjectConfig{
				Containers: []corev1.Container{{Name: "main", Image: "proxy:v1"}},
			},
			expectError:   true,
			errorContains: "inject conflicting with container: main",
		},
		{
			name: "init container name conflict",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers:     []corev1.Container{{Name: "main", Image: "nginx"}},
					InitContainers: []corev1.Container{{Name: "init-setup", Image: "busybox"}},
				},
			},
			config: SidecarInjectConfig{
				InitContainers: []corev1.Container{{Name: "init-setup", Image: "init:v1"}},
			},
			expectError:   true,
			errorContains: "inject conflicting with init container: init-setup",
		},
		{
			name: "volume name conflict",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
					Volumes:    []corev1.Volume{{Name: "shared-data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
				},
			},
			config: SidecarInjectConfig{
				Volumes: []corev1.Volume{{Name: "shared-data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			},
			expectError:   true,
			errorContains: "inject conflicting with volume: shared-data",
		},
		{
			name: "multiple conflicts - reports first init container conflict",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers:     []corev1.Container{{Name: "app", Image: "nginx"}},
					InitContainers: []corev1.Container{{Name: "init-app", Image: "busybox"}},
					Volumes:        []corev1.Volume{{Name: "vol-a", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
				},
			},
			config: SidecarInjectConfig{
				InitContainers: []corev1.Container{{Name: "init-app", Image: "init:v1"}},
				Containers:     []corev1.Container{{Name: "app", Image: "proxy:v1"}},
				Volumes:        []corev1.Volume{{Name: "vol-a", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			},
			expectError:   true,
			errorContains: "init container: init-app",
		},
		{
			name: "empty config - no conflicts",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
				},
			},
			config:      SidecarInjectConfig{},
			expectError: false,
		},
		{
			name: "empty pod - no conflicts",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{},
			},
			config: SidecarInjectConfig{
				Containers:     []corev1.Container{{Name: "sidecar", Image: "proxy:v1"}},
				InitContainers: []corev1.Container{{Name: "init-sidecar", Image: "init:v1"}},
				Volumes:        []corev1.Volume{{Name: "vol", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkInjectionConflicts(tt.pod, tt.config)
			if tt.expectError {
				if err == nil {
					t.Fatalf("expected error but got nil")
				}
				if !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("expected error containing %q, got %q", tt.errorContains, err.Error())
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestDoSidecarInjectionConflicts(t *testing.T) {
	tests := []struct {
		name            string
		sandbox         *agentsv1alpha1.Sandbox
		pod             *corev1.Pod
		injectConfigMap map[string]string
		expectError     bool
		errorContains   string
	}{
		{
			name: "traffic-proxy container conflict returns error",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{Name: agentsv1alpha1.RuntimeConfigForInjectTrafficProxy},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx"},
						{Name: "istio-proxy", Image: "existing-proxy:v1"},
					},
				},
			},
			injectConfigMap: map[string]string{
				KEY_TRAFFIC_PROXY_INJECTION_CONFIG: `{
					"mainContainer": {"name": ""},
					"csiSidecar": [],
					"volume": [],
					"containers": [{"name": "istio-proxy", "image": "istio:v1"}]
				}`,
			},
			expectError:   true,
			errorContains: "istio-proxy",
		},
		{
			name: "traffic-proxy init container conflict returns error",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{Name: agentsv1alpha1.RuntimeConfigForInjectTrafficProxy},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers:     []corev1.Container{{Name: "main", Image: "nginx"}},
					InitContainers: []corev1.Container{{Name: "istio-init", Image: "existing:v1"}},
				},
			},
			injectConfigMap: map[string]string{
				KEY_TRAFFIC_PROXY_INJECTION_CONFIG: `{
					"mainContainer": {"name": ""},
					"csiSidecar": [],
					"volume": [],
					"initContainers": [{"name": "istio-init", "image": "istio-init:v1"}],
					"containers": [{"name": "istio-proxy", "image": "istio:v1"}]
				}`,
			},
			expectError:   true,
			errorContains: "istio-init",
		},
		{
			name: "traffic-proxy no conflict succeeds and injects correctly",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{Name: agentsv1alpha1.RuntimeConfigForInjectTrafficProxy},
					},
				},
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"networking.agents.kruise.io/health-probe-rewrite": "false",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
				},
			},
			injectConfigMap: map[string]string{
				KEY_TRAFFIC_PROXY_INJECTION_CONFIG: `{
					"mainContainer": {"name": ""},
					"csiSidecar": [],
					"volume": [],
					"initContainers": [{"name": "istio-init", "image": "istio-init:v1"}],
					"containers": [{"name": "istio-proxy", "image": "istio:v1"}],
					"annotations": {"sidecar.istio.io/status": "injected"},
					"labels": {"app.mesh": "true"}
				}`,
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			err := doSidecarInjection(ctx, tt.sandbox, tt.pod, tt.injectConfigMap)
			if tt.expectError {
				if err == nil {
					t.Fatalf("expected error but got nil")
				}
				if !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("expected error containing %q, got %q", tt.errorContains, err.Error())
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				// Verify successful injection applied correctly
				if tt.name == "traffic-proxy no conflict succeeds and injects correctly" {
					if utils.FindContainer("istio-proxy", tt.pod.Spec.Containers) == nil {
						t.Error("expected istio-proxy container to be injected")
					}
					if utils.FindContainer("istio-init", tt.pod.Spec.InitContainers) == nil {
						t.Error("expected istio-init init container to be injected")
					}
					if tt.pod.Labels["app.mesh"] != "true" {
						t.Error("expected label app.mesh=true to be injected")
					}
					if tt.pod.Annotations["sidecar.istio.io/status"] != "injected" {
						t.Error("expected annotation sidecar.istio.io/status=injected to be injected")
					}
				}
			}
		})
	}
}

func TestDoSidecarInjectionDuplicateRuntimes(t *testing.T) {
	tests := []struct {
		name                   string
		sandbox                *agentsv1alpha1.Sandbox
		pod                    *corev1.Pod
		injectConfigMap        map[string]string
		expectRuntimeContainer bool
		expectCSIContainer     bool
		expectEgressContainer  bool
		expectInitContainers   int
		expectContainers       int
	}{
		{
			name: "duplicate runtime names - only first runtime processed",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime},
						{Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
				},
			},
			injectConfigMap: map[string]string{
				KEY_RUNTIME_INJECTION_CONFIG: `{
					"mainContainer": {"env": [{"name": "RUNTIME_ENV", "value": "test"}], "volumeMounts": []},
					"csiSidecar": [{"name": "runtime-sidecar", "image": "runtime:v1"}],
					"volume": []
				}`,
			},
			expectRuntimeContainer: true,
			expectInitContainers:   1, // only one runtime-sidecar, not duplicated
		},
		{
			name: "duplicate csi runtime names - only first csi processed",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount},
						{Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount},
						{Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
				},
			},
			injectConfigMap: map[string]string{
				KEY_CSI_INJECTION_CONFIG: `{
					"mainContainer": {"volumeMounts": [{"name": "csi-vol", "mountPath": "/csi"}]},
					"csiSidecar": [{"name": "csi-sidecar", "image": "csi:v1"}],
					"volume": [{"name": "csi-vol", "emptyDir": {}}]
				}`,
			},
			expectCSIContainer:   true,
			expectInitContainers: 1, // only one csi-sidecar despite 3 runtime entries
		},
		{
			name: "duplicate traffic-proxy runtime names - only first processed",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{Name: agentsv1alpha1.RuntimeConfigForInjectTrafficProxy},
						{Name: agentsv1alpha1.RuntimeConfigForInjectTrafficProxy},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
				},
			},
			injectConfigMap: map[string]string{
				KEY_TRAFFIC_PROXY_INJECTION_CONFIG: `{
					"mainContainer": {},
					"csiSidecar": [],
					"volume": [{"name": "egress-vol", "emptyDir": {}}],
					"initContainers": [],
					"containers": [{"name": "egress-sidecar", "image": "egress:v1"}],
					"labels": {"egress": "enabled"},
					"annotations": {}
				}`,
			},
			expectEgressContainer: true,
			expectContainers:       2, // main + egress sidecar
		},
		{
			name: "mixed unique and duplicate runtimes - order preserved, first wins",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: agentsv1alpha1.SandboxSpec{
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime},
						{Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount},
						{Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime}, // duplicate, should skip
						{Name: agentsv1alpha1.RuntimeConfigForInjectTrafficProxy},
						{Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount}, // duplicate, should skip
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
				},
			},
			injectConfigMap: map[string]string{
				KEY_RUNTIME_INJECTION_CONFIG: `{
					"mainContainer": {"env": [{"name": "RUNTIME", "value": "on"}], "volumeMounts": []},
					"csiSidecar": [{"name": "runtime-sidecar", "image": "runtime:v1"}],
					"volume": []
				}`,
				KEY_CSI_INJECTION_CONFIG: `{
					"mainContainer": {"volumeMounts": [{"name": "csi-vol", "mountPath": "/csi"}]},
					"csiSidecar": [{"name": "csi-sidecar", "image": "csi:v1"}],
					"volume": [{"name": "csi-vol", "emptyDir": {}}]
				}`,
				KEY_TRAFFIC_PROXY_INJECTION_CONFIG: `{
					"mainContainer": {},
					"csiSidecar": [],
					"volume": [{"name": "egress-vol", "emptyDir": {}}],
					"initContainers": [],
					"containers": [{"name": "egress-sidecar", "image": "egress:v1"}],
					"labels": {},
					"annotations": {}
				}`,
			},
			expectRuntimeContainer: true,
			expectCSIContainer:     true,
			expectEgressContainer:  true,
			expectInitContainers:   2, // runtime-sidecar + csi-sidecar (no duplicates)
			expectContainers:       2, // main + egress-sidecar
		},
		{
			name: "empty runtime list - no injection",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec:       agentsv1alpha1.SandboxSpec{},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
				},
			},
			injectConfigMap:  map[string]string{},
			expectContainers: 1, // unchanged
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			err := doSidecarInjection(ctx, tt.sandbox, tt.pod, tt.injectConfigMap)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			runtimeContainerInjected := false
			csiContainerInjected := false
			for _, container := range tt.pod.Spec.InitContainers {
				if container.Name == "runtime-sidecar" && container.Image == "runtime:v1" {
					runtimeContainerInjected = true
				}
				if container.Name == "csi-sidecar" && container.Image == "csi:v1" {
					csiContainerInjected = true
				}
			}
			egressContainerInjected := false
			for _, container := range tt.pod.Spec.Containers {
				if container.Name == "egress-sidecar" && container.Image == "egress:v1" {
					egressContainerInjected = true
				}
			}

			if tt.expectRuntimeContainer && !runtimeContainerInjected {
				t.Error("expected runtime sidecar to be injected, but not found")
			}
			if !tt.expectRuntimeContainer && runtimeContainerInjected {
				t.Error("expected NO runtime sidecar injection, but injection occurred")
			}
			if tt.expectCSIContainer && !csiContainerInjected {
				t.Error("expected csi sidecar to be injected, but not found")
			}
			if !tt.expectCSIContainer && csiContainerInjected {
				t.Error("expected NO csi sidecar injection, but injection occurred")
			}
			if tt.expectEgressContainer && !egressContainerInjected {
				t.Error("expected egress sidecar to be injected, but not found")
			}
			if !tt.expectEgressContainer && egressContainerInjected {
				t.Error("expected NO egress sidecar injection, but injection occurred")
			}

			if tt.expectInitContainers > 0 {
				if len(tt.pod.Spec.InitContainers) != tt.expectInitContainers {
					t.Errorf("expected %d InitContainers, got %d", tt.expectInitContainers, len(tt.pod.Spec.InitContainers))
				}
			}
			if tt.expectContainers > 0 {
				if len(tt.pod.Spec.Containers) != tt.expectContainers {
					t.Errorf("expected %d Containers, got %d", tt.expectContainers, len(tt.pod.Spec.Containers))
				}
			}
		})
	}
}
