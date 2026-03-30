package sidecarutils

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

func TestInjectPodTemplateCSIAndRuntimeSidecar(t *testing.T) {
	tests := []struct {
		name                   string
		sandbox                *agentsv1alpha1.Sandbox
		podSpecTemplate        *corev1.PodSpec
		injectionConfigData    map[string]string
		expectInjection        bool
		expectRuntimeContainer bool
		expectCSIContainer     bool
		expectRuntimeEnvCount  int
		expectCSIVolumeMounts  int
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
			podSpecTemplate: &corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "main", Image: "nginx"},
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
					Annotations: map[string]string{
						agentsv1alpha1.ShouldInjectAgentRuntime: "true",
					},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{},
					},
				},
			},
			podSpecTemplate: &corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "main", Image: "nginx"},
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
					Annotations: map[string]string{
						agentsv1alpha1.ShouldInjectCsiMount: "true",
					},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{},
					},
				},
			},
			podSpecTemplate: &corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "main", Image: "nginx"},
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
					Annotations: map[string]string{
						agentsv1alpha1.ShouldInjectAgentRuntime: "true",
						agentsv1alpha1.ShouldInjectCsiMount:     "true",
					},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{},
					},
				},
			},
			podSpecTemplate: &corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "main", Image: "nginx"},
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

			err := InjectPodTemplateCSIAndRuntimeSidecar(ctx, tt.sandbox, tt.podSpecTemplate, fakeClient)
			// Verify results
			if !tt.expectInjection {
				// Should return early without error
				if err != nil {
					t.Errorf("expected no error when no injection needed, got: %v", err)
				}
				// Pod template should not be modified
				if len(tt.podSpecTemplate.Containers) != 1 {
					t.Errorf("expected 1 container, got %d", len(tt.podSpecTemplate.Containers))
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
			for _, container := range tt.podSpecTemplate.InitContainers {
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
			for _, container := range tt.podSpecTemplate.Containers {
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
			expectedTotal := 1 // main container
			if tt.expectRuntimeContainer {
				expectedTotal++
			}
			if tt.expectCSIContainer {
				expectedTotal++
			}
			// Count total containers (InitContainers + Containers)
			totalContainers := len(tt.podSpecTemplate.InitContainers) + len(tt.podSpecTemplate.Containers)
			if totalContainers != expectedTotal {
				t.Errorf("expected %d total containers, got %d", expectedTotal, totalContainers)
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
		podSpecTemplate        *corev1.PodSpec
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
					Annotations: map[string]string{
						agentsv1alpha1.ShouldInjectAgentRuntime: "true",
					},
				},
			},
			podSpecTemplate: &corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "main", Image: "nginx"},
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
					Annotations: map[string]string{
						agentsv1alpha1.ShouldInjectCsiMount: "true",
					},
				},
			},
			podSpecTemplate: &corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "main", Image: "nginx"},
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
					Annotations: map[string]string{
						agentsv1alpha1.ShouldInjectAgentRuntime: "true",
						agentsv1alpha1.ShouldInjectCsiMount:     "true",
					},
				},
			},
			podSpecTemplate: &corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "main", Image: "nginx"},
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
			podSpecTemplate: &corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "main", Image: "nginx"},
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
					Annotations: map[string]string{
						agentsv1alpha1.ShouldInjectAgentRuntime: "true",
					},
				},
			},
			podSpecTemplate: &corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "main", Image: "nginx"},
					{Name: "runtime-sidecar", Image: "existing:v1"}, // conflict with runtime sidecar in Containers
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
					Annotations: map[string]string{
						agentsv1alpha1.ShouldInjectAgentRuntime: "true",
					},
				},
			},
			podSpecTemplate: &corev1.PodSpec{
				InitContainers: []corev1.Container{
					{Name: "runtime-sidecar", Image: "existing-init:v1"}, // conflict in initContainers
				},
				Containers: []corev1.Container{
					{Name: "main", Image: "nginx"},
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
					Annotations: map[string]string{
						agentsv1alpha1.ShouldInjectCsiMount: "true",
					},
				},
			},
			podSpecTemplate: &corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "main", Image: "nginx"},
					{Name: "csi-sidecar", Image: "existing-csi:v1"}, // conflict with csi sidecar in Containers
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
					Annotations: map[string]string{
						agentsv1alpha1.ShouldInjectCsiMount: "true",
					},
				},
			},
			podSpecTemplate: &corev1.PodSpec{
				InitContainers: []corev1.Container{
					{Name: "csi-sidecar", Image: "existing-init-csi:v1"}, // conflict in initContainers
				},
				Containers: []corev1.Container{
					{Name: "main", Image: "nginx"},
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
					Annotations: map[string]string{
						agentsv1alpha1.ShouldInjectAgentRuntime: "true",
						agentsv1alpha1.ShouldInjectCsiMount:     "true",
					},
				},
			},
			podSpecTemplate: &corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "main", Image: "nginx"},
					{Name: "runtime-sidecar", Image: "existing:v1"}, // conflict with runtime sidecar
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
					Annotations: map[string]string{
						agentsv1alpha1.ShouldInjectAgentRuntime: "true",
						agentsv1alpha1.ShouldInjectCsiMount:     "true",
					},
				},
			},
			podSpecTemplate: &corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "main", Image: "nginx"},
					{Name: "csi-sidecar", Image: "existing-csi:v1"}, // conflict with csi sidecar
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
					Annotations: map[string]string{
						agentsv1alpha1.ShouldInjectAgentRuntime: "true",
						agentsv1alpha1.ShouldInjectCsiMount:     "true",
					},
				},
			},
			podSpecTemplate: &corev1.PodSpec{
				InitContainers: []corev1.Container{
					{Name: "runtime-sidecar", Image: "existing-init:v1"}, // conflict in initContainers
					{Name: "csi-sidecar", Image: "existing-init-csi:v1"}, // conflict in initContainers
				},
				Containers: []corev1.Container{
					{Name: "main", Image: "nginx"},
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
			err := doSidecarInjection(ctx, tt.sandbox, tt.podSpecTemplate, tt.injectConfigMap)
			// Verify no error
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// Verify container injection - both runtime and csi sidecars are injected to InitContainers
			runtimeContainerInjected := false
			csiContainerInjected := false
			// Check InitContainers for both runtime sidecar and csi sidecar
			// Use image to distinguish between pre-existing containers (for conflict tests) and injected containers
			for _, container := range tt.podSpecTemplate.InitContainers {
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
			for _, container := range tt.podSpecTemplate.Containers {
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
				actualVolumeCount := len(tt.podSpecTemplate.Volumes)
				if actualVolumeCount < tt.expectCSIVolumeCount {
					t.Errorf("expected at least %d volumes, got %d", tt.expectCSIVolumeCount, actualVolumeCount)
				}
			}
			// Main container should always exist
			mainContainerFound := false
			for _, container := range tt.podSpecTemplate.Containers {
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
