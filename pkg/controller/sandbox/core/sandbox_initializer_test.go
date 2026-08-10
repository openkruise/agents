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
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/utils"
	utilruntime "github.com/openkruise/agents/pkg/utils/runtime"
	utestutils "github.com/openkruise/agents/pkg/utils/testutils"
	testutils "github.com/openkruise/agents/test/utils"
)

var scheme *runtime.Scheme

func init() {
	scheme = runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
}

func TestInitialize(t *testing.T) {
	utestutils.InitLogOutput()
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
			expectError:     "returned status 500",
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

// TestInitialize_InitTransportDispatch verifies the init-handshake side of the
// two coexisting runtime transports: non-empty rtOpts route the /init call to
// the endpoint they select instead of the legacy annotation-resolved URL, so a
// TLS-capable sandbox reaches the agent-runtime sidecar over HTTPS. The options
// are the real ones TransportOptionsFor produces (WithTLS + WithTLSPort), with
// the authority pointed at the httptest certificate hostname so verification
// succeeds while the dial is pinned to the sandbox Pod IP. The legacy path
// (empty rtOpts) is covered by TestInitialize above.
func TestInitialize_InitTransportDispatch(t *testing.T) {
	utestutils.InitLogOutput()

	newBox := func(name string) *agentsv1alpha1.Sandbox {
		initOpts := config.InitRuntimeOptions{
			AccessToken: "test-token",
			EnvVars:     map[string]string{"VAR1": "val1"},
		}
		initJSON, err := json.Marshal(initOpts)
		require.NoError(t, err)
		return &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels: map[string]string{
					agentsv1alpha1.LabelSandboxClaimName: "my-claim",
				},
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationInitRuntimeRequest: string(initJSON),
				},
			},
		}
	}

	tests := []struct {
		name        string
		initStatus  int
		expectError string
	}{
		{
			name:       "init routed to rtOpts endpoint",
			initStatus: http.StatusNoContent,
		},
		{
			// GetInitRuntimeRequest always sets ReInit=true, so a 401 from the
			// dispatched endpoint is classified as "already initialized".
			name:       "init 401 treated as success on re-init",
			initStatus: http.StatusUnauthorized,
		},
		{
			name:        "init server error propagates",
			initStatus:  http.StatusInternalServerError,
			expectError: "returned status 500",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The dispatch server stands in for the agent-runtime HTTPS endpoint
			// selected by rtOpts; it must receive the /init handshake.
			var gotInit config.InitRuntimeOptions
			var dispatchHits atomic.Int32
			dispatchSvr := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "/init", r.URL.Path)
				dispatchHits.Add(1)
				require.NoError(t, json.NewDecoder(r.Body).Decode(&gotInit))
				w.WriteHeader(tt.initStatus)
			}))
			defer dispatchSvr.Close()

			// The httptest certificate is issued for "example.com" and the
			// loopback IPs; trust it and address it by that authority so the
			// handshake validates while the dial is pinned to the Pod IP.
			caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: dispatchSvr.Certificate().Raw})
			podIP, portStr, err := net.SplitHostPort(dispatchSvr.Listener.Addr().String())
			require.NoError(t, err)
			tlsPort, err := strconv.Atoi(portStr)
			require.NoError(t, err)

			// The legacy server backs the annotation-resolved runtime URL; with
			// non-empty rtOpts it must never see the /init handshake.
			var legacyInitHits atomic.Int32
			legacySvr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/init" {
					legacyInitHits.Add(1)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer legacySvr.Close()

			box := newBox("test-sandbox-init-dispatch")
			box.Annotations[agentsv1alpha1.AnnotationRuntimeURL] = legacySvr.URL
			box.Status.PodInfo.PodIP = podIP

			fc := fake.NewClientBuilder().WithScheme(scheme).Build()
			err = Initialize(t.Context(), box, &box.Status, fc, fc, storages.NewStorageProvider(),
				utilruntime.WithTLS(utilruntime.TLSBundle{CABundle: caPEM}),
				utilruntime.WithAuthority("example.com"),
				utilruntime.WithTLSPort(tlsPort),
				utilruntime.WithRetry(wait.Backoff{Duration: time.Millisecond, Steps: 1}))

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
			}
			assert.GreaterOrEqual(t, dispatchHits.Load(), int32(1), "init must reach the rtOpts-selected endpoint")
			assert.Equal(t, int32(0), legacyInitHits.Load(), "init must not fall back to the legacy annotation URL")
			assert.Equal(t, "test-token", gotInit.AccessToken)
			assert.Equal(t, map[string]string{"VAR1": "val1"}, gotInit.EnvVars)
		})
	}
}

func TestDefaultSandboxInitializer(t *testing.T) {
	utestutils.InitLogOutput()

	tests := []struct {
		name                string
		box                 *agentsv1alpha1.Sandbox
		expectError         string
		expectInitCondition metav1.ConditionStatus
		expectInitReason    string
	}{
		{
			name: "unclaimed sandbox - skip initialization, set RuntimeInitialized=True",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox-default-init",
					Namespace: "default",
					Labels:    map[string]string{},
				},
			},
			expectInitCondition: metav1.ConditionTrue,
			expectInitReason:    agentsv1alpha1.SandboxConditionRuntimeInitReasonSucceeded,
		},
		{
			name: "claimed sandbox with invalid init runtime annotation - error path sets RuntimeInitialized=False",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox-init-fail",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxClaimName: "my-claim",
					},
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationInitRuntimeRequest: "not-valid-json",
					},
				},
			},
			expectError:         "failed to unmarshal init runtime request",
			expectInitCondition: metav1.ConditionFalse,
			expectInitReason:    agentsv1alpha1.SandboxConditionRuntimeInitReasonFailed,
		},
		{
			// The initializer holds no TLS bundle here, mirroring a controller
			// started without --runtime-client-cert-dir. The transport is now
			// resolved for every initialization, so a sandbox stamped by an
			// earlier configuration fails loudly instead of falling back to the
			// plaintext /init and mount paths.
			name: "tls-capable sandbox without tls bundle - RuntimeInitialized=False",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox-tls-no-bundle",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxClaimName: "my-claim",
					},
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationRuntimeTLSPort: "49984",
					},
				},
			},
			expectError:         "advertises runtime TLS port",
			expectInitCondition: metav1.ConditionFalse,
			expectInitReason:    agentsv1alpha1.SandboxConditionRuntimeInitReasonFailed,
		},
		{
			// A broken capability annotation points at a broken injection
			// template, so it must surface rather than silently select the
			// plaintext transport.
			name: "invalid tls port annotation - RuntimeInitialized=False",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox-bad-tls-port",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxClaimName: "my-claim",
					},
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationRuntimeTLSPort: "not-a-port",
					},
				},
			},
			expectError:         "invalid runtime TLS port annotation",
			expectInitCondition: metav1.ConditionFalse,
			expectInitReason:    agentsv1alpha1.SandboxConditionRuntimeInitReasonFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := fake.NewClientBuilder().WithScheme(scheme).Build()
			recorder := record.NewFakeRecorder(10)

			initializer := &defaultSandboxInitializer{
				client:          fc,
				apiReader:       fc,
				storageRegistry: storages.NewStorageProvider(),
				recorder:        recorder,
			}

			newStatus := &agentsv1alpha1.SandboxStatus{}
			err := initializer.Initialize(t.Context(), tt.box, newStatus)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}

			initCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.RuntimeInitialized))
			require.NotNil(t, initCond, "expected RuntimeInitialized condition to be set")
			assert.Equal(t, tt.expectInitCondition, initCond.Status)
			assert.Equal(t, tt.expectInitReason, initCond.Reason)
		})
	}
}
