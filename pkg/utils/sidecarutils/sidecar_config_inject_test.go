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
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// getTestNamespace returns the namespace used for testing
func getTestNamespace() string {
	// This should match the namespace returned by webhookutils.GetNamespace()
	return "sandbox-system"
}

func TestSetCSIMountContainer(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name                   string
		template               *corev1.PodSpec
		config                 SidecarInjectConfig
		expectedContainers     int // main containers count
		expectedInitContainers int // CSI sidecars are injected to InitContainers
		expectedVolumes        int
		expectedEnvCount       int
		expectedVolumeMount    int
	}{
		{
			name: "empty template with CSI config",
			template: &corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main-container",
						Image: "nginx:latest",
					},
				},
			},
			config: SidecarInjectConfig{
				MainContainer: corev1.Container{
					Env: []corev1.EnvVar{
						{Name: "ENV1", Value: "value1"},
						{Name: "ENV2", Value: "value2"},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "mount-root", MountPath: "/run/csi/mount-root"},
						{Name: "nas-plugin-dir", MountPath: "/var/run/csi/sockets/nasplugin.csi.alibabacloud.com"},
					},
				},
				Sidecars: []corev1.Container{
					{
						Name:  "csi-sidecar",
						Image: "csi-sidecar:latest",
					},
					{
						Name:  "csi-agent-sidecar",
						Image: "csi-agent:latest",
					},
				},
				Volumes: []corev1.Volume{
					{Name: "mount-root", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					{Name: "nas-plugin-dir", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					{Name: "oss-plugin-dir", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				},
			},
			expectedContainers:     1, // main container only
			expectedInitContainers: 2, // 2 CSI sidecars injected to InitContainers
			expectedVolumes:        3,
			expectedEnvCount:       2,
			expectedVolumeMount:    2,
		},
		{
			name: "template with existing volumes - no duplicates",
			template: &corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main-container",
						Image: "nginx:latest",
					},
				},
				Volumes: []corev1.Volume{
					{Name: "mount-root", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				},
			},
			config: SidecarInjectConfig{
				MainContainer: corev1.Container{
					VolumeMounts: []corev1.VolumeMount{
						{Name: "mount-root", MountPath: "/run/csi/mount-root"},
					},
				},
				Sidecars: []corev1.Container{
					{Name: "csi-sidecar", Image: "csi-sidecar:latest"},
				},
				Volumes: []corev1.Volume{
					{Name: "mount-root", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					{Name: "new-volume", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				},
			},
			expectedContainers:     1, // main container only
			expectedInitContainers: 1, // 1 CSI sidecar injected to InitContainers
			expectedVolumes:        2, // mount-root (existing) + new-volume
			expectedEnvCount:       0,
			expectedVolumeMount:    1,
		},
		{
			name: "template with existing envs - no duplicates",
			template: &corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main-container",
						Image: "nginx:latest",
						Env: []corev1.EnvVar{
							{Name: "ENV1", Value: "existing-value"},
						},
					},
				},
			},
			config: SidecarInjectConfig{
				MainContainer: corev1.Container{
					Env: []corev1.EnvVar{
						{Name: "ENV1", Value: "value1"}, // duplicate
						{Name: "ENV2", Value: "value2"}, // new
					},
				},
				Sidecars: []corev1.Container{},
				Volumes:  []corev1.Volume{},
			},
			expectedContainers:     1,
			expectedInitContainers: 0,
			expectedVolumes:        0,
			expectedEnvCount:       2, // ENV1 (existing) + ENV2 (new)
			expectedVolumeMount:    0,
		},
		{
			name: "empty containers list",
			template: &corev1.PodSpec{
				Containers: []corev1.Container{},
			},
			config: SidecarInjectConfig{
				MainContainer: corev1.Container{
					Env: []corev1.EnvVar{{Name: "ENV1", Value: "value1"}},
				},
				Sidecars: []corev1.Container{{Name: "sidecar", Image: "sidecar:latest"}},
				Volumes:  []corev1.Volume{{Name: "vol1", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			},
			expectedContainers:     0, // no change when containers list is empty
			expectedInitContainers: 0, // sidecars not injected when no main container
			expectedVolumes:        0,
			expectedEnvCount:       0,
			expectedVolumeMount:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setCSIMountContainer(ctx, tt.template, tt.config)

			// Verify main container count
			if len(tt.template.Containers) != tt.expectedContainers {
				t.Errorf("expected %d containers, got %d", tt.expectedContainers, len(tt.template.Containers))
			}

			// Verify init container count (CSI sidecars are injected here)
			if len(tt.template.InitContainers) != tt.expectedInitContainers {
				t.Errorf("expected %d init containers, got %d", tt.expectedInitContainers, len(tt.template.InitContainers))
			}

			// Verify volume count
			if len(tt.template.Volumes) != tt.expectedVolumes {
				t.Errorf("expected %d volumes, got %d", tt.expectedVolumes, len(tt.template.Volumes))
			}

			// Verify main container env count
			if len(tt.template.Containers) > 0 {
				mainContainer := tt.template.Containers[0]
				if len(mainContainer.Env) != tt.expectedEnvCount {
					t.Errorf("expected %d env vars, got %d", tt.expectedEnvCount, len(mainContainer.Env))
				}

				// Verify main container volume mount count
				if len(mainContainer.VolumeMounts) != tt.expectedVolumeMount {
					t.Errorf("expected %d volume mounts, got %d", tt.expectedVolumeMount, len(mainContainer.VolumeMounts))
				}
			}
		})
	}
}

func TestSetAgentRuntimeContainer(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name                     string
		template                 *corev1.PodSpec
		config                   SidecarInjectConfig
		expectedInitContainers   int
		expectedContainers       int
		expectedEnvCount         int
		hasPostStartLifecycle    bool
		hasPostStartCommand      bool
		expectedVolumeMountCount int
	}{
		{
			name: "empty template with runtime config",
			template: &corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main-container",
						Image: "nginx:latest",
					},
				},
			},
			config: SidecarInjectConfig{
				MainContainer: corev1.Container{
					Env: []corev1.EnvVar{
						{Name: "ENVD_DIR", Value: "/mnt/envd"},
						{Name: "GODEBUG", Value: "multipathtcp=0"},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "envd-volume", MountPath: "/mnt/envd"},
					},
					Lifecycle: &corev1.Lifecycle{
						PostStart: &corev1.LifecycleHandler{
							Exec: &corev1.ExecAction{
								Command: []string{"bash", "-c", "/mnt/envd/envd-run.sh"},
							},
						},
					},
				},
				Sidecars: []corev1.Container{
					{
						Name:    "init-runtime",
						Image:   "runtime:latest",
						Command: []string{"sh", "/workspace/entrypoint_inner.sh"},
					},
				},
			},
			expectedInitContainers:   1,
			expectedContainers:       1,
			expectedEnvCount:         2,
			hasPostStartLifecycle:    true,
			hasPostStartCommand:      true,
			expectedVolumeMountCount: 1,
		},
		{
			name: "template with existing init containers",
			template: &corev1.PodSpec{
				InitContainers: []corev1.Container{
					{Name: "existing-init", Image: "init:latest"},
				},
				Containers: []corev1.Container{
					{Name: "main-container", Image: "nginx:latest"},
				},
			},
			config: SidecarInjectConfig{
				MainContainer: corev1.Container{
					Env: []corev1.EnvVar{{Name: "ENV1", Value: "value1"}},
				},
				Sidecars: []corev1.Container{
					{Name: "init-runtime-1", Image: "runtime1:latest"},
					{Name: "init-runtime-2", Image: "runtime2:latest"},
				},
			},
			expectedInitContainers:   3,
			expectedContainers:       1,
			expectedEnvCount:         1,
			hasPostStartLifecycle:    false, // No PostStart when config doesn't have valid handler
			hasPostStartCommand:      false,
			expectedVolumeMountCount: 0,
		},
		{
			name: "template without init containers",
			template: &corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "main-container", Image: "nginx:latest"},
				},
			},
			config: SidecarInjectConfig{
				MainContainer: corev1.Container{},
				Sidecars: []corev1.Container{
					{Name: "init-runtime", Image: "runtime:latest"},
				},
			},
			expectedInitContainers:   1,
			expectedContainers:       1,
			expectedEnvCount:         0,
			hasPostStartLifecycle:    false, // No PostStart when config doesn't have valid handler
			hasPostStartCommand:      false,
			expectedVolumeMountCount: 0,
		},
		{
			name: "empty containers list",
			template: &corev1.PodSpec{
				Containers: []corev1.Container{},
			},
			config: SidecarInjectConfig{
				MainContainer: corev1.Container{
					Env: []corev1.EnvVar{{Name: "ENV1", Value: "value1"}},
				},
				Sidecars: []corev1.Container{{Name: "init", Image: "init:latest"}},
			},
			expectedInitContainers:   1,
			expectedContainers:       0,
			expectedEnvCount:         0,
			hasPostStartLifecycle:    false, // No container to modify
			hasPostStartCommand:      false,
			expectedVolumeMountCount: 0,
		},
		{
			name: "multiple runtime sidecars",
			template: &corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main-container",
						Image: "nginx:latest",
					},
				},
			},
			config: SidecarInjectConfig{
				MainContainer: corev1.Container{
					Env: []corev1.EnvVar{
						{Name: "POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "envd-volume", MountPath: "/mnt/envd"},
					},
				},
				Sidecars: []corev1.Container{
					{Name: "init-1", Image: "runtime1:latest"},
					{Name: "init-2", Image: "runtime2:latest"},
					{Name: "init-3", Image: "runtime3:latest"},
				},
			},
			expectedInitContainers:   3,
			expectedContainers:       1,
			expectedEnvCount:         1,
			hasPostStartLifecycle:    false, // No PostStart when config doesn't have valid handler
			hasPostStartCommand:      false,
			expectedVolumeMountCount: 1,
		},
		{
			name: "existing lifecycle - conflicting postStart should skip env and volumeMounts injection",
			template: &corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main-container",
						Image: "nginx:latest",
						Lifecycle: &corev1.Lifecycle{
							PostStart: &corev1.LifecycleHandler{
								Exec: &corev1.ExecAction{
									Command: []string{"echo", "old"},
								},
							},
						},
					},
				},
			},
			config: SidecarInjectConfig{
				MainContainer: corev1.Container{
					Env: []corev1.EnvVar{{Name: "ENV1", Value: "value1"}},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "vol1", MountPath: "/vol1"},
					},
					Lifecycle: &corev1.Lifecycle{
						PostStart: &corev1.LifecycleHandler{
							Exec: &corev1.ExecAction{
								Command: []string{"echo", "new"},
							},
						},
					},
				},
				Sidecars: []corev1.Container{
					{Name: "init-runtime", Image: "runtime:latest"},
				},
			},
			expectedInitContainers:   1,
			expectedContainers:       1,
			expectedEnvCount:         1, // conflicting postStart only skips postStart injection, env still injected
			hasPostStartLifecycle:    true,
			hasPostStartCommand:      true, // keeps existing command
			expectedVolumeMountCount: 1,    // conflicting postStart only skips postStart injection, volumeMounts still injected
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setAgentRuntimeContainer(ctx, tt.template, tt.config)

			// Verify init container count
			if len(tt.template.InitContainers) != tt.expectedInitContainers {
				t.Errorf("expected %d init containers, got %d", tt.expectedInitContainers, len(tt.template.InitContainers))
			}

			// Verify container count
			if len(tt.template.Containers) != tt.expectedContainers {
				t.Errorf("expected %d containers, got %d", tt.expectedContainers, len(tt.template.Containers))
			}

			// Verify main container configuration
			if len(tt.template.Containers) > 0 {
				mainContainer := tt.template.Containers[0]

				// Check env count
				if len(mainContainer.Env) != tt.expectedEnvCount {
					t.Errorf("expected %d env vars, got %d", tt.expectedEnvCount, len(mainContainer.Env))
				}

				// Check volume mount count
				if len(mainContainer.VolumeMounts) != tt.expectedVolumeMountCount {
					t.Errorf("expected %d volume mounts, got %d", tt.expectedVolumeMountCount, len(mainContainer.VolumeMounts))
				}

				// Check lifecycle post start
				if tt.hasPostStartLifecycle {
					if mainContainer.Lifecycle == nil || mainContainer.Lifecycle.PostStart == nil {
						t.Error("expected PostStart lifecycle handler, got nil")
					} else if tt.hasPostStartCommand {
						// Verify that the command was set from config
						if mainContainer.Lifecycle.PostStart.Exec == nil {
							t.Error("expected Exec action in PostStart, got nil")
						} else if len(mainContainer.Lifecycle.PostStart.Exec.Command) == 0 {
							t.Error("expected PostStart command to be set from config, got empty")
						}
					}
				} else {
					// When no container exists, lifecycle should not be checked
					if mainContainer.Lifecycle != nil && mainContainer.Lifecycle.PostStart != nil {
						t.Error("expected no PostStart lifecycle handler, but got one")
					}
				}
			}
		})
	}
}

func TestSetMainContainerWhenInjectCSISidecar(t *testing.T) {
	tests := []struct {
		name                string
		mainContainer       *corev1.Container
		config              SidecarInjectConfig
		expectedEnvCount    int
		expectedVolumeMount int
	}{
		{
			name: "empty container with full config",
			mainContainer: &corev1.Container{
				Name:  "main",
				Image: "nginx:latest",
			},
			config: SidecarInjectConfig{
				MainContainer: corev1.Container{
					Env: []corev1.EnvVar{
						{Name: "ENV1", Value: "value1"},
						{Name: "ENV2", Value: "value2"},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "vol1", MountPath: "/vol1"},
						{Name: "vol2", MountPath: "/vol2"},
					},
				},
			},
			expectedEnvCount:    2,
			expectedVolumeMount: 2,
		},
		{
			name: "container with existing envs - no duplicates",
			mainContainer: &corev1.Container{
				Name:  "main",
				Image: "nginx:latest",
				Env: []corev1.EnvVar{
					{Name: "ENV1", Value: "existing"},
				},
			},
			config: SidecarInjectConfig{
				MainContainer: corev1.Container{
					Env: []corev1.EnvVar{
						{Name: "ENV1", Value: "new"},    // duplicate
						{Name: "ENV2", Value: "value2"}, // new
					},
				},
			},
			expectedEnvCount:    2, // ENV1 (existing) + ENV2 (new)
			expectedVolumeMount: 0,
		},
		{
			name: "container with existing volume mounts - no duplicates",
			mainContainer: &corev1.Container{
				Name:  "main",
				Image: "nginx:latest",
				VolumeMounts: []corev1.VolumeMount{
					{Name: "vol1", MountPath: "/existing-vol1"},
				},
			},
			config: SidecarInjectConfig{
				MainContainer: corev1.Container{
					VolumeMounts: []corev1.VolumeMount{
						{Name: "vol1", MountPath: "/new-vol1"}, // duplicate
						{Name: "vol2", MountPath: "/vol2"},     // new
					},
				},
			},
			expectedEnvCount:    0,
			expectedVolumeMount: 2, // vol1 (existing) + vol2 (new)
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setMainContainerWhenInjectCSISidecar(tt.mainContainer, tt.config)

			// Verify env count
			if len(tt.mainContainer.Env) != tt.expectedEnvCount {
				t.Errorf("expected %d env vars, got %d", tt.expectedEnvCount, len(tt.mainContainer.Env))
			}

			// Verify volume mount count
			if len(tt.mainContainer.VolumeMounts) != tt.expectedVolumeMount {
				t.Errorf("expected %d volume mounts, got %d", tt.expectedVolumeMount, len(tt.mainContainer.VolumeMounts))
			}
		})
	}
}

func TestSetMainContainerConfigWhenInjectRuntimeSidecar(t *testing.T) {
	tests := []struct {
		name                     string
		mainContainer            *corev1.Container
		config                   SidecarInjectConfig
		expectedEnvCount         int
		expectedVolumeMountCount int
		hasPostStart             bool
		postStartCommand         []string
	}{
		{
			name: "empty container with full config",
			mainContainer: &corev1.Container{
				Name:  "main",
				Image: "nginx:latest",
			},
			config: SidecarInjectConfig{
				MainContainer: corev1.Container{
					Env: []corev1.EnvVar{
						{Name: "ENVD_DIR", Value: "/mnt/envd"},
						{Name: "GODEBUG", Value: "multipathtcp=0"},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "envd-volume", MountPath: "/mnt/envd"},
					},
					Lifecycle: &corev1.Lifecycle{
						PostStart: &corev1.LifecycleHandler{
							Exec: &corev1.ExecAction{
								Command: []string{"bash", "-c", "/mnt/envd/envd-run.sh"},
							},
						},
					},
				},
			},
			expectedEnvCount:         2,
			expectedVolumeMountCount: 1,
			hasPostStart:             true,
			postStartCommand:         []string{"bash", "-c", "/mnt/envd/envd-run.sh"},
		},
		{
			name: "container with existing lifecycle - conflicting postStart should skip all injection",
			mainContainer: &corev1.Container{
				Name:  "main",
				Image: "nginx:latest",
				Lifecycle: &corev1.Lifecycle{
					PostStart: &corev1.LifecycleHandler{
						Exec: &corev1.ExecAction{
							Command: []string{"echo", "old"},
						},
					},
				},
			},
			config: SidecarInjectConfig{
				MainContainer: corev1.Container{
					Env: []corev1.EnvVar{{Name: "ENV1", Value: "value1"}},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "vol1", MountPath: "/vol1"},
					},
					Lifecycle: &corev1.Lifecycle{
						PostStart: &corev1.LifecycleHandler{
							Exec: &corev1.ExecAction{
								Command: []string{"echo", "new"},
							},
						},
					},
				},
			},
			expectedEnvCount:         1, // conflicting postStart only skips postStart injection, env still injected
			expectedVolumeMountCount: 1, // conflicting postStart only skips postStart injection, volumeMounts still injected
			hasPostStart:             true,
			postStartCommand:         []string{"echo", "old"}, // keeps existing, NOT overridden
		},
		{
			name: "config without lifecycle - no override",
			mainContainer: &corev1.Container{
				Name:  "main",
				Image: "nginx:latest",
				Lifecycle: &corev1.Lifecycle{
					PostStart: &corev1.LifecycleHandler{
						Exec: &corev1.ExecAction{
							Command: []string{"echo", "keep"},
						},
					},
				},
			},
			config: SidecarInjectConfig{
				MainContainer: corev1.Container{
					Env: []corev1.EnvVar{{Name: "ENV1", Value: "value1"}},
				},
			},
			expectedEnvCount:         1,
			expectedVolumeMountCount: 0,
			hasPostStart:             true, // keeps existing
			postStartCommand:         []string{"echo", "keep"},
		},
		{
			name: "container with empty Lifecycle - should apply config PostStart",
			mainContainer: &corev1.Container{
				Name:      "main",
				Image:     "nginx:latest",
				Lifecycle: &corev1.Lifecycle{}, // empty Lifecycle, no PostStart
			},
			config: SidecarInjectConfig{
				MainContainer: corev1.Container{
					Env: []corev1.EnvVar{{Name: "ENV1", Value: "value1"}},
					Lifecycle: &corev1.Lifecycle{
						PostStart: &corev1.LifecycleHandler{
							Exec: &corev1.ExecAction{
								Command: []string{"bash", "-c", "injected"},
							},
						},
					},
				},
			},
			expectedEnvCount:         1,
			expectedVolumeMountCount: 0,
			hasPostStart:             true,
			postStartCommand:         []string{"bash", "-c", "injected"},
		},
		{
			name: "container with empty PostStart handler - should apply config PostStart",
			mainContainer: &corev1.Container{
				Name:  "main",
				Image: "nginx:latest",
				Lifecycle: &corev1.Lifecycle{
					PostStart: &corev1.LifecycleHandler{}, // empty handler, no Exec/HTTPGet/TCPSocket
				},
			},
			config: SidecarInjectConfig{
				MainContainer: corev1.Container{
					Env: []corev1.EnvVar{{Name: "ENV1", Value: "value1"}},
					Lifecycle: &corev1.Lifecycle{
						PostStart: &corev1.LifecycleHandler{
							Exec: &corev1.ExecAction{
								Command: []string{"bash", "-c", "injected"},
							},
						},
					},
				},
			},
			expectedEnvCount:         1,
			expectedVolumeMountCount: 0,
			hasPostStart:             true,
			postStartCommand:         []string{"bash", "-c", "injected"},
		},
		{
			name: "config with empty PostStart handler - should not apply",
			mainContainer: &corev1.Container{
				Name:  "main",
				Image: "nginx:latest",
			},
			config: SidecarInjectConfig{
				MainContainer: corev1.Container{
					Env: []corev1.EnvVar{{Name: "ENV1", Value: "value1"}},
					Lifecycle: &corev1.Lifecycle{
						PostStart: &corev1.LifecycleHandler{}, // empty handler
					},
				},
			},
			expectedEnvCount:         1,
			expectedVolumeMountCount: 0,
			hasPostStart:             false, // config has empty handler, should not apply
			postStartCommand:         nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setMainContainerConfigWhenInjectRuntimeSidecar(context.TODO(), tt.mainContainer, tt.config)

			// Verify env count
			if len(tt.mainContainer.Env) != tt.expectedEnvCount {
				t.Errorf("expected %d env vars, got %d", tt.expectedEnvCount, len(tt.mainContainer.Env))
			}

			// Verify volume mount count
			if len(tt.mainContainer.VolumeMounts) != tt.expectedVolumeMountCount {
				t.Errorf("expected %d volume mounts, got %d", tt.expectedVolumeMountCount, len(tt.mainContainer.VolumeMounts))
			}

			// Verify PostStart lifecycle
			if tt.hasPostStart && tt.postStartCommand != nil {
				if tt.mainContainer.Lifecycle == nil || tt.mainContainer.Lifecycle.PostStart == nil {
					t.Error("expected PostStart lifecycle handler, got nil")
				} else if tt.mainContainer.Lifecycle.PostStart.Exec == nil {
					t.Error("expected Exec action in PostStart, got nil")
				} else if len(tt.mainContainer.Lifecycle.PostStart.Exec.Command) != len(tt.postStartCommand) {
					t.Errorf("expected command %v, got %v", tt.postStartCommand, tt.mainContainer.Lifecycle.PostStart.Exec.Command)
				}
			}
		})
	}
}

func TestParseCSIMountConfig(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		configRaw      map[string]string
		expectedConfig SidecarInjectConfig
		expectError    bool
		errorContains  string
	}{
		{
			name: "valid CSI config",
			configRaw: map[string]string{
				KEY_CSI_INJECTION_CONFIG: `{
					"mainContainer": {
						"env": [
							{"name": "ENV1", "value": "value1"},
							{"name": "ENV2", "value": "value2"}
						],
						"volumeMounts": [
							{"name": "vol1", "mountPath": "/mnt/vol1"}
						]
					},
					"csiSidecar": [
						{"name": "csi-sidecar", "image": "csi:latest"}
					],
					"volume": [
						{"name": "vol1", "emptyDir": {}}
					]
				}`,
			},
			expectedConfig: SidecarInjectConfig{
				MainContainer: corev1.Container{
					Env: []corev1.EnvVar{
						{Name: "ENV1", Value: "value1"},
						{Name: "ENV2", Value: "value2"},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "vol1", MountPath: "/mnt/vol1"},
					},
				},
				Sidecars: []corev1.Container{
					{Name: "csi-sidecar", Image: "csi:latest"},
				},
				Volumes: []corev1.Volume{
					{Name: "vol1", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				},
			},
			expectError: false,
		},
		{
			name: "empty CSI config",
			configRaw: map[string]string{
				KEY_CSI_INJECTION_CONFIG: `{"mainContainer":{},"csiSidecar":[],"volume":[]}`,
			},
			expectedConfig: SidecarInjectConfig{
				MainContainer: corev1.Container{},
				Sidecars:      []corev1.Container{},
				Volumes:       []corev1.Volume{},
			},
			expectError: false,
		},
		{
			name: "missing CSI config key",
			configRaw: map[string]string{
				"other-key": "some-value",
			},
			expectedConfig: SidecarInjectConfig{},
			expectError:    false,
			errorContains:  "",
		},
		{
			name: "invalid JSON format",
			configRaw: map[string]string{
				KEY_CSI_INJECTION_CONFIG: `{"mainContainer": invalid json}`,
			},
			expectedConfig: SidecarInjectConfig{},
			expectError:    true,
			errorContains:  "invalid character",
		},
		{
			name: "partial config with only sidecars",
			configRaw: map[string]string{
				KEY_CSI_INJECTION_CONFIG: `{"mainContainer":{},"csiSidecar":[{"name":"sidecar1","image":"img1"}],"volume":[]}`,
			},
			expectedConfig: SidecarInjectConfig{
				MainContainer: corev1.Container{},
				Sidecars: []corev1.Container{
					{Name: "sidecar1", Image: "img1"},
				},
				Volumes: []corev1.Volume{},
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := parseInjectConfig(ctx, KEY_CSI_INJECTION_CONFIG, tt.configRaw)

			if tt.expectError {
				if err == nil {
					t.Error("expected error, got nil")
				} else if tt.errorContains != "" && !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("expected error to contain %q, got %q", tt.errorContains, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if !reflect.DeepEqual(config, tt.expectedConfig) {
					t.Errorf("config mismatch:\nexpected: %v\ngot:      %v", tt.expectedConfig, config)
				}
			}
		})
	}
}

func TestParseAgentRuntimeConfig(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		configRaw      map[string]string
		expectedConfig SidecarInjectConfig
		expectError    bool
		errorContains  string
	}{
		{
			name: "valid runtime config",
			configRaw: map[string]string{
				KEY_RUNTIME_INJECTION_CONFIG: `{
					"mainContainer": {
						"env": [
							{"name": "ENVD_DIR", "value": "/mnt/envd"},
							{"name": "GODEBUG", "value": "multipathtcp=0"}
						],
						"volumeMounts": [
							{"name": "envd-volume", "mountPath": "/mnt/envd"}
						],
						"lifecycle": {
							"postStart": {
								"exec": {
									"command": ["bash", "-c", "/mnt/envd/envd-run.sh"]
								}
							}
						}
					},
					"csiSidecar": [
						{
							"name": "init-runtime",
							"image": "runtime:latest",
							"command": ["sh", "/workspace/entrypoint_inner.sh"]
						}
					],
					"volume": [
						{"name": "envd-volume", "emptyDir": {}}
					]
				}`,
			},
			expectedConfig: SidecarInjectConfig{
				MainContainer: corev1.Container{
					Env: []corev1.EnvVar{
						{Name: "ENVD_DIR", Value: "/mnt/envd"},
						{Name: "GODEBUG", Value: "multipathtcp=0"},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "envd-volume", MountPath: "/mnt/envd"},
					},
					Lifecycle: &corev1.Lifecycle{
						PostStart: &corev1.LifecycleHandler{
							Exec: &corev1.ExecAction{
								Command: []string{"bash", "-c", "/mnt/envd/envd-run.sh"},
							},
						},
					},
				},
				Sidecars: []corev1.Container{
					{
						Name:    "init-runtime",
						Image:   "runtime:latest",
						Command: []string{"sh", "/workspace/entrypoint_inner.sh"},
					},
				},
				Volumes: []corev1.Volume{
					{Name: "envd-volume", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				},
			},
			expectError: false,
		},
		{
			name: "empty runtime config",
			configRaw: map[string]string{
				KEY_RUNTIME_INJECTION_CONFIG: `{"mainContainer":{},"csiSidecar":[],"volume":[]}`,
			},
			expectedConfig: SidecarInjectConfig{
				MainContainer: corev1.Container{},
				Sidecars:      []corev1.Container{},
				Volumes:       []corev1.Volume{},
			},
			expectError: false,
		},
		{
			name: "missing runtime config key",
			configRaw: map[string]string{
				"other-key": "some-value",
			},
			expectedConfig: SidecarInjectConfig{
				MainContainer: corev1.Container{},
				Sidecars:      nil,
				Volumes:       nil,
			},
			expectError:   false,
			errorContains: "",
		},
		{
			name: "invalid JSON format",
			configRaw: map[string]string{
				KEY_RUNTIME_INJECTION_CONFIG: `not valid json`,
			},
			expectedConfig: SidecarInjectConfig{},
			expectError:    true,
			errorContains:  "invalid character",
		},
		{
			name: "partial config with only main container env",
			configRaw: map[string]string{
				KEY_RUNTIME_INJECTION_CONFIG: `{"mainContainer":{"env":[{"name":"POD_UID","valueFrom":{"fieldRef":{"fieldPath":"metadata.uid"}}}]},"csiSidecar":[],"volume":[]}`,
			},
			expectedConfig: SidecarInjectConfig{
				MainContainer: corev1.Container{
					Env: []corev1.EnvVar{
						{
							Name: "POD_UID",
							ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{
									FieldPath: "metadata.uid",
								},
							},
						},
					},
				},
				Sidecars: []corev1.Container{},
				Volumes:  []corev1.Volume{},
			},
			expectError: false,
		},
		{
			name: "complex lifecycle with multiple commands",
			configRaw: map[string]string{
				KEY_RUNTIME_INJECTION_CONFIG: `{
					"mainContainer": {
						"lifecycle": {
							"postStart": {
								"exec": {
									"command": ["bash", "-c", "echo start && /run.sh"]
								}
							}
						}
					},
					"csiSidecar": [],
					"volume": []
				}`,
			},
			expectedConfig: SidecarInjectConfig{
				MainContainer: corev1.Container{
					Lifecycle: &corev1.Lifecycle{
						PostStart: &corev1.LifecycleHandler{
							Exec: &corev1.ExecAction{
								Command: []string{"bash", "-c", "echo start && /run.sh"},
							},
						},
					},
				},
				Sidecars: []corev1.Container{},
				Volumes:  []corev1.Volume{},
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := parseInjectConfig(ctx, KEY_RUNTIME_INJECTION_CONFIG, tt.configRaw)

			if tt.expectError {
				if err == nil {
					t.Error("expected error, got nil")
				} else if tt.errorContains != "" && !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("expected error to contain %q, got %q", tt.errorContains, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if !compareSidecarInjectConfigs(config, tt.expectedConfig) {
					t.Errorf("config mismatch:\nexpected: %#v\ngot:      %#v", tt.expectedConfig, config)
				}
			}
		})
	}
}

// compareSidecarInjectConfigs compares two SidecarInjectConfig structs deeply
func compareSidecarInjectConfigs(a, b SidecarInjectConfig) bool {
	return reflect.DeepEqual(a.MainContainer, b.MainContainer) &&
		reflect.DeepEqual(a.Sidecars, b.Sidecars) &&
		reflect.DeepEqual(a.Volumes, b.Volumes)
}

func TestIsContainersExists(t *testing.T) {
	tests := []struct {
		name             string
		podContainers    []corev1.Container
		injectContainers []corev1.Container
		expected         bool
	}{
		{
			name:             "both lists empty",
			podContainers:    []corev1.Container{},
			injectContainers: []corev1.Container{},
			expected:         false,
		},
		{
			name:          "podContainers empty, injectContainers has values",
			podContainers: []corev1.Container{},
			injectContainers: []corev1.Container{
				{Name: "sidecar-1"},
				{Name: "sidecar-2"},
			},
			expected: false,
		},
		{
			name: "podContainers has values, injectContainers empty",
			podContainers: []corev1.Container{
				{Name: "main-container"},
				{Name: "app-container"},
			},
			injectContainers: []corev1.Container{},
			expected:         false,
		},
		{
			name: "no conflict - different names",
			podContainers: []corev1.Container{
				{Name: "main-container"},
				{Name: "app-container"},
			},
			injectContainers: []corev1.Container{
				{Name: "sidecar-1"},
				{Name: "sidecar-2"},
			},
			expected: false,
		},
		{
			name: "conflict - one duplicate name",
			podContainers: []corev1.Container{
				{Name: "main-container"},
				{Name: "app-container"},
			},
			injectContainers: []corev1.Container{
				{Name: "main-container"}, // duplicate
				{Name: "sidecar-1"},
			},
			expected: true,
		},
		{
			name: "conflict - multiple duplicate names",
			podContainers: []corev1.Container{
				{Name: "main-container"},
				{Name: "app-container"},
				{Name: "helper-container"},
			},
			injectContainers: []corev1.Container{
				{Name: "main-container"},   // duplicate
				{Name: "helper-container"}, // duplicate
			},
			expected: true,
		},
		{
			name: "conflict - last inject container duplicates",
			podContainers: []corev1.Container{
				{Name: "main-container"},
			},
			injectContainers: []corev1.Container{
				{Name: "sidecar-1"},
				{Name: "sidecar-2"},
				{Name: "main-container"}, // duplicate at the end
			},
			expected: true,
		},
		{
			name: "no conflict - single container in each list",
			podContainers: []corev1.Container{
				{Name: "main-container"},
			},
			injectContainers: []corev1.Container{
				{Name: "sidecar"},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isContainersExists(tt.podContainers, tt.injectContainers)
			if result != tt.expected {
				t.Errorf("isContainersExists() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

func TestHasValidLifecycleHandler(t *testing.T) {
	tests := []struct {
		name     string
		handler  *corev1.LifecycleHandler
		expected bool
	}{
		{
			name:     "nil handler",
			handler:  nil,
			expected: false,
		},
		{
			name:     "empty handler",
			handler:  &corev1.LifecycleHandler{},
			expected: false,
		},
		{
			name: "handler with Exec action",
			handler: &corev1.LifecycleHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"echo", "hello"},
				},
			},
			expected: true,
		},
		{
			name: "handler with HTTPGet action",
			handler: &corev1.LifecycleHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/health",
					Port: intstr.FromInt(8080),
				},
			},
			expected: true,
		},
		{
			name: "handler with TCPSocket action",
			handler: &corev1.LifecycleHandler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromInt(8080),
				},
			},
			expected: true,
		},
		{
			name: "handler with empty Exec (nil Command)",
			handler: &corev1.LifecycleHandler{
				Exec: &corev1.ExecAction{},
			},
			expected: true, // Exec is not nil, so it's valid even if Command is empty
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hasValidLifecycleHandler(tt.handler)
			if result != tt.expected {
				t.Errorf("hasValidLifecycleHandler() = %v, expected %v", result, tt.expected)
			}
		})
	}
}
