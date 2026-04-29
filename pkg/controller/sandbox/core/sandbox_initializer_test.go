/*
Copyright 2025.

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

package core

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	utilruntime "github.com/openkruise/agents/pkg/utils/runtime"
	sandboxManagerUtils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	testutils "github.com/openkruise/agents/test/utils"
)

var scheme *runtime.Scheme

func init() {
	scheme = runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
}

func TestInitialize(t *testing.T) {
	sandboxManagerUtils.InitLogOutput()
	newFakeClient := func(initObj ...client.Object) client.Client {
		return fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObj...).Build()
	}
	tests := []struct {
		name            string
		box             *agentsv1alpha1.Sandbox
		newStatus       *agentsv1alpha1.SandboxStatus
		setupClients    func() (client.Client, client.Reader)
		storageRegistry storages.VolumeMountProviderRegistry
		expectError     string
		useRuntimeSvr   bool
		serverOpts      testutils.TestRuntimeServerOptions
	}{
		{
			name: "nil client returns nil",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{},
			setupClients: func() (c client.Client, reader client.Reader) {
				return nil, fake.NewFakeClient()
			},
		},
		{
			name: "nil apiReader returns nil",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{},
			setupClients: func() (c client.Client, reader client.Reader) {
				return fake.NewFakeClient(), nil
			},
		},
		{
			name: "sandbox not claimed by SandboxClaim - skip initialization",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels:    map[string]string{},
				},
			},
			newStatus:       &agentsv1alpha1.SandboxStatus{},
			useRuntimeSvr:   false,
			storageRegistry: storages.NewStorageProvider(),
		},
		{
			name: "claimed sandbox with no init runtime annotation and no csi mount annotation - success",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxClaimName: "my-claim",
					},
					Annotations: map[string]string{},
				},
			},
			newStatus:       &agentsv1alpha1.SandboxStatus{},
			useRuntimeSvr:   false,
			storageRegistry: storages.NewStorageProvider(),
		},
		{
			name: "claimed sandbox with init runtime annotation - re-init runtime success",
			box: func() *agentsv1alpha1.Sandbox {
				initOpts := config.InitRuntimeOptions{
					AccessToken: "test-token",
					EnvVars:     map[string]string{"VAR1": "val1"},
				}
				initJSON, _ := json.Marshal(initOpts)
				return &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-reinit",
						Namespace: "default",
						Labels: map[string]string{
							agentsv1alpha1.LabelSandboxClaimName: "my-claim",
						},
						Annotations: map[string]string{
							agentsv1alpha1.AnnotationInitRuntimeRequest: string(initJSON),
						},
					},
				}
			}(),
			newStatus: &agentsv1alpha1.SandboxStatus{},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: utilruntime.RunCommandResult{
					PID:    1,
					Exited: true,
				},
				RunCommandImmediately: true,
			},
			useRuntimeSvr:   true,
			storageRegistry: storages.NewStorageProvider(),
		},
		{
			name: "claimed sandbox with init runtime annotation - re-init runtime failure",
			box: func() *agentsv1alpha1.Sandbox {
				initOpts := config.InitRuntimeOptions{
					AccessToken: "test-token",
				}
				initJSON, _ := json.Marshal(initOpts)
				return &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-reinit-fail",
						Namespace: "default",
						Labels: map[string]string{
							agentsv1alpha1.LabelSandboxClaimName: "my-claim",
						},
						Annotations: map[string]string{
							agentsv1alpha1.AnnotationInitRuntimeRequest: string(initJSON),
						},
					},
				}
			}(),
			newStatus: &agentsv1alpha1.SandboxStatus{},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: utilruntime.RunCommandResult{
					PID:    1,
					Exited: true,
				},
				RunCommandImmediately: true,
				InitErrCode:           500,
			},
			useRuntimeSvr:   true,
			storageRegistry: storages.NewStorageProvider(),
			expectError:     "not 2xx",
		},
		{
			name: "claimed sandbox with invalid init runtime annotation JSON",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox-bad-init-json",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxClaimName: "my-claim",
					},
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationInitRuntimeRequest: "not-valid-json",
					},
				},
			},
			newStatus:       &agentsv1alpha1.SandboxStatus{},
			useRuntimeSvr:   false,
			storageRegistry: storages.NewStorageProvider(),
			expectError:     "failed to unmarshal init runtime request",
		},
		{
			name: "claimed sandbox with invalid csi mount annotation JSON",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox-bad-csi-json",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxClaimName: "my-claim",
					},
					Annotations: map[string]string{
						models.ExtensionKeyClaimWithCSIMount_MountConfig: "not-valid-json",
					},
				},
			},
			newStatus:       &agentsv1alpha1.SandboxStatus{},
			useRuntimeSvr:   false,
			storageRegistry: storages.NewStorageProvider(),
			expectError:     "failed to get csi mount request",
		},
		{
			name: "claimed sandbox with csi mount annotation - pv not found",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox-csi-pv-missing",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxClaimName: "my-claim",
					},
					Annotations: map[string]string{
						models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"non-existent-pv","mountPath":"/data"}]`,
					},
				},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: utilruntime.RunCommandResult{
					PID:    1,
					Exited: true,
				},
				RunCommandImmediately: true,
			},
			useRuntimeSvr:   true,
			storageRegistry: storages.NewStorageProvider(),
			expectError:     "failed to generate csi mount options config",
		},
		{
			name: "claimed sandbox with csi mount annotation - driver supported, mount success",
			box: func() *agentsv1alpha1.Sandbox {
				return &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-csi-ok",
						Namespace: "default",
						Labels: map[string]string{
							agentsv1alpha1.LabelSandboxClaimName: "my-claim",
						},
						Annotations: map[string]string{
							models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"test-pv-ok","mountPath":"/data"}]`,
						},
					},
				}
			}(),
			newStatus: &agentsv1alpha1.SandboxStatus{},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: utilruntime.RunCommandResult{
					PID:      1,
					ExitCode: 0,
					Exited:   true,
				},
				RunCommandImmediately: true,
			},
			useRuntimeSvr: true,
			storageRegistry: func() storages.VolumeMountProviderRegistry {
				reg := storages.NewStorageProvider()
				reg.RegisterProvider("test-csi-driver", &storages.MountProvider{})
				return reg
			}(),
			setupClients: func() (client.Client, client.Reader) {
				c := newFakeClient(&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-pv-ok",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver:       "test-csi-driver",
								VolumeHandle: "handle-ok",
							},
						},
					},
				})
				return c, c
			},
		},
		{
			name: "claimed sandbox with csi mount annotation - mount command failure",
			box: func() *agentsv1alpha1.Sandbox {
				return &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-csi-mount-fail",
						Namespace: "default",
						Labels: map[string]string{
							agentsv1alpha1.LabelSandboxClaimName: "my-claim",
						},
						Annotations: map[string]string{
							models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"test-pv-fail","mountPath":"/data"}]`,
						},
					},
				}
			}(),
			newStatus: &agentsv1alpha1.SandboxStatus{},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: utilruntime.RunCommandResult{
					PID:      1,
					ExitCode: 1,
					Stderr:   []string{"mount error"},
					Exited:   true,
				},
				RunCommandImmediately: true,
			},
			useRuntimeSvr: true,
			storageRegistry: func() storages.VolumeMountProviderRegistry {
				reg := storages.NewStorageProvider()
				reg.RegisterProvider("test-csi-driver-fail", &storages.MountProvider{})
				return reg
			}(),
			setupClients: func() (client.Client, client.Reader) {
				c := newFakeClient(&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-pv-fail",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver:       "test-csi-driver-fail",
								VolumeHandle: "handle-fail",
							},
						},
					},
				})
				return c, c
			},
			expectError: "failed to perform ReCSIMount after resume",
		},
		{
			name: "claimed sandbox with multiple csi mount annotations - partial failure returns joined errors",
			box: func() *agentsv1alpha1.Sandbox {
				return &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-csi-multi-fail",
						Namespace: "default",
						Labels: map[string]string{
							agentsv1alpha1.LabelSandboxClaimName: "my-claim",
						},
						Annotations: map[string]string{
							models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"test-pv-multi-1","mountPath":"/data1"},{"pvName":"test-pv-multi-2","mountPath":"/data2"}]`,
						},
					},
				}
			}(),
			newStatus: &agentsv1alpha1.SandboxStatus{},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: utilruntime.RunCommandResult{
					PID:      1,
					ExitCode: 1,
					Stderr:   []string{"mount failed"},
					Exited:   true,
				},
				RunCommandImmediately: true,
			},
			useRuntimeSvr: true,
			storageRegistry: func() storages.VolumeMountProviderRegistry {
				reg := storages.NewStorageProvider()
				reg.RegisterProvider("test-multi-driver", &storages.MountProvider{})
				return reg
			}(),
			setupClients: func() (client.Client, client.Reader) {
				var pvs []client.Object
				for _, pvName := range []string{"test-pv-multi-1", "test-pv-multi-2"} {
					pv := &corev1.PersistentVolume{
						ObjectMeta: metav1.ObjectMeta{
							Name: pvName,
						},
						Spec: corev1.PersistentVolumeSpec{
							PersistentVolumeSource: corev1.PersistentVolumeSource{
								CSI: &corev1.CSIPersistentVolumeSource{
									Driver:       "test-multi-driver",
									VolumeHandle: pvName + "-handle",
								},
							},
						},
					}
					pvs = append(pvs, pv)
				}
				c := newFakeClient(pvs...)
				return c, c
			},
			expectError: "failed to perform ReCSIMount after resume",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c client.Client
			var r client.Reader
			if tt.setupClients != nil {
				c, r = tt.setupClients()
			} else {
				f := newFakeClient()
				c, r = f, f
			}
			if tt.useRuntimeSvr {
				server := testutils.NewTestRuntimeServer(tt.serverOpts)
				defer server.Close()

				if tt.box.Annotations == nil {
					tt.box.Annotations = map[string]string{}
				}
				tt.box.Annotations[agentsv1alpha1.AnnotationRuntimeURL] = server.URL
				tt.box.Annotations[agentsv1alpha1.AnnotationRuntimeAccessToken] = utilruntime.AccessToken
			}

			err := Initialize(t.Context(), tt.box, tt.newStatus, c, r, tt.storageRegistry)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestDefaultSandboxInitializer(t *testing.T) {
	sandboxManagerUtils.InitLogOutput()

	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	initializer := &defaultSandboxInitializer{
		client:          fc,
		apiReader:       fc,
		storageRegistry: storages.NewStorageProvider(),
	}

	// Test with unclaimed sandbox - should skip initialization
	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox-default-init",
			Namespace: "default",
			Labels:    map[string]string{},
		},
	}
	newStatus := &agentsv1alpha1.SandboxStatus{}

	err := initializer.Initialize(t.Context(), box, newStatus)
	assert.NoError(t, err)
}
