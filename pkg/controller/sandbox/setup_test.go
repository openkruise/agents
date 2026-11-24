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

package sandbox

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/sandbox/core"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/sandboxroute"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

// newSandboxTestManager creates a real controller-runtime Manager backed by a
// stub REST config. The manager is never started; we only exercise controller
// registration, so no running apiserver is required.
func newSandboxTestManager(t *testing.T) ctrl.Manager {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add clientgo scheme: %v", err)
	}
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add agents scheme: %v", err)
	}
	mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:0"}, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr
}

// noopEnqueuer satisfies Enqueuer without side effects.
type noopEnqueuer struct{}

func (n *noopEnqueuer) Enqueue(_, _ string) {}

// TestSandboxReconciler_SetupWithManager verifies that SetupWithManager
// registers the sandbox controller with the manager without error. This
// exercises the wiring path (For/Watches/Named/Complete chain) against a real
// controller-runtime manager backed by a stub REST config; the manager is
// never started so no apiserver is needed.
func TestSandboxReconciler_SetupWithManager(t *testing.T) {
	mgr := newSandboxTestManager(t)

	r := &SandboxReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		controls: core.NewSandboxControl(core.SandboxControlArgs{
			Client:    mgr.GetClient(),
			APIReader: mgr.GetAPIReader(),
			Recorder:  mgr.GetEventRecorderFor("sandbox"),
		}),
		metricsCleanup: &noopEnqueuer{},
	}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("SandboxReconciler.SetupWithManager: unexpected error: %v", err)
	}
}

func TestSetupRouteSyncValidation(t *testing.T) {
	assert.False(t, utilfeature.DefaultFeatureGate.Enabled(features.SandboxRouteSyncGate))
	tests := []struct {
		name        string
		gateEnabled bool
		selector    string
		bindAddress string
		bindPort    int
		wantErr     string
	}{
		{name: "默认关闭且默认参数不启用同步", selector: "agents.kruise.io/sandbox-manager-peer-finder=ack-sandbox-manager", bindPort: 7946},
		{name: "gate 关闭不校验 selector、绑定地址或端口", selector: "component in (", bindAddress: "invalid-address", bindPort: 65536},
		{name: "自定义端口不能单独启用同步", selector: "agents.kruise.io/sandbox-manager-peer-finder=ack-sandbox-manager", bindPort: 9000},
		{name: "gate 开启但 selector 为空仍关闭", gateEnabled: true, bindPort: 65536},
		{name: "gate 开启后 selector 必须合法", gateEnabled: true, selector: "component in (", bindPort: 7946, wantErr: "parse peer selector"},
		{name: "gate 开启后端口不能越界", gateEnabled: true, selector: "agents.kruise.io/sandbox-manager-peer-finder=ack-sandbox-manager", bindPort: 65536, wantErr: "--memberlist-bind-port must be in the range 1-65535"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SandboxRouteSyncGate, tt.gateEnabled)
			// All cases must return before accessing the manager or starting listeners.
			sender, err := SetupRouteSync(nil, tt.selector, tt.bindAddress, tt.bindPort)
			assert.Nil(t, sender)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestRouteSyncLifecycle(t *testing.T) {
	tests := []struct {
		name         string
		hostname     string
		podName      string
		wantNodeName string
		bindAddress  string
		podIP        string
		peerSelector string
		cancelSync   bool
		setupOnly    bool
		bindPort     int
		wantBindPort int
	}{
		{name: "默认端口", setupOnly: true, bindPort: 7946, wantBindPort: 7946},
		{name: "零端口回退默认值", setupOnly: true, wantBindPort: 7946},
		{name: "负端口回退默认值", setupOnly: true, bindPort: -1, wantBindPort: 7946},
		{name: "端口上限合法", setupOnly: true, bindPort: 65535, wantBindPort: 65535},
		{name: "覆盖默认 selector", peerSelector: "component=sandbox-manager"},
		{name: "HOSTNAME 优先", hostname: "controller-hostname", podName: "controller-pod", wantNodeName: "controller-hostname"},
		{name: "POD_NAME 回退", podName: "controller-route-sync-test", wantNodeName: "controller-route-sync-test"},
		{name: "环境变量均为空时使用 UUID"},
		{name: "指定绑定地址覆盖 POD_IP", bindAddress: "127.0.0.1", podIP: "192.0.2.1"},
		{name: "缓存未同步时可以取消", cancelSync: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SandboxRouteSyncGate, true)
			peerSelector := tt.peerSelector
			if peerSelector == "" {
				peerSelector = "agents.kruise.io/sandbox-manager-peer-finder=ack-sandbox-manager"
			}
			t.Setenv("HOSTNAME", tt.hostname)
			t.Setenv("POD_NAME", tt.podName)
			podIP := tt.podIP
			if podIP == "" {
				podIP = "127.0.0.1"
			}
			t.Setenv("POD_IP", podIP)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			listStarted := make(chan struct{}, 1)
			// Use real informer/cache/memberlist implementations; only simulate API HTTP responses.
			apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/namespaces/sandbox-system/pods" {
					t.Errorf("unexpected API request: %s", r.URL.String())
					http.NotFound(w, r)
					return
				}
				assert.Equal(t, peerSelector, r.URL.Query().Get("labelSelector"))
				list := &corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: []corev1.Pod{}}
				select {
				case listStarted <- struct{}{}:
				default:
				}
				if tt.cancelSync {
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				encoder := json.NewEncoder(w)
				if r.URL.Query().Get("watch") != "true" {
					if err := encoder.Encode(list); err != nil && r.Context().Err() == nil {
						t.Errorf("failed to encode List response: %v", err)
					}
					return
				}
				if r.URL.Query().Get("sendInitialEvents") == "true" {
					bookmark := map[string]interface{}{
						"apiVersion": "v1", "kind": "Pod",
						"metadata": map[string]interface{}{"resourceVersion": "1", "annotations": map[string]string{"k8s.io/initial-events-end": "true"}},
					}
					if err := encoder.Encode(map[string]interface{}{"type": "BOOKMARK", "object": bookmark}); err != nil {
						return
					}
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer apiServer.Close()
			scheme := runtime.NewScheme()
			require.NoError(t, clientgoscheme.AddToScheme(scheme))
			mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion})
			mapper.Add(corev1.SchemeGroupVersion.WithKind("Pod"), meta.RESTScopeNamespace)
			mgr, err := ctrl.NewManager(&rest.Config{Host: apiServer.URL}, ctrl.Options{
				Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
				MapperProvider: func(*rest.Config, *http.Client) (meta.RESTMapper, error) { return mapper, nil },
			})
			require.NoError(t, err)
			port := tt.bindPort
			wantPort := tt.wantBindPort
			if !tt.setupOnly {
				// Pass an available port through setup without occupying the production default.
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, err)
				port = listener.Addr().(*net.TCPAddr).Port
				require.NoError(t, listener.Close())
				wantPort = port
			}
			sender, err := SetupRouteSync(mgr, peerSelector, tt.bindAddress, port)
			require.NoError(t, err)
			require.NotNil(t, sender)
			assert.Equal(t, wantPort, sender.bindPort)
			assert.Equal(t, tt.bindAddress, sender.bindAddress)
			assert.NotSame(t, mgr.GetCache(), sender.GetCache())
			if tt.setupOnly {
				return
			}
			done := make(chan error, 1)
			go func() { done <- sender.Start(ctx) }()
			// Ensure assertion failures also cancel the informer and allow the component to exit.
			defer cancel()
			if tt.cancelSync {
				select {
				case <-listStarted:
				case <-ctx.Done():
					t.Fatal("timed out waiting for informer request")
				}
				cancel()
			} else {
				select {
				case <-sender.ready:
				case err := <-done:
					t.Fatalf("component exited before becoming ready: %v", err)
				case <-ctx.Done():
					t.Fatal("timed out waiting for peer startup")
				}
				assert.Equal(t, "http", sender.outbound.Scheme())
				members := sender.peers.GetAllMembers()
				require.Len(t, members, 1)
				assert.Equal(t, "127.0.0.1", members[0].IP)
				assert.Equal(t, "127.0.0.1", sender.peers.LocalAddr().String())
				assert.Equal(t, port, sender.peers.LocalPort())
				if tt.wantNodeName != "" {
					assert.Equal(t, peers.NodePrefixSandboxController+tt.wantNodeName, members[0].Name)
				} else {
					assert.Regexp(t, "^"+peers.NodePrefixSandboxController+"[0-9a-f]{8}$", members[0].Name)
				}
				assert.Empty(t, sender.peers.GetPeers())
				require.NoError(t, sender.SyncRouteWithPeers(ctx, sandboxroute.Route{}))
				cancel()
			}
			select {
			case err := <-done:
				if tt.cancelSync {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.NoError(t, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("component did not exit in time")
			}
			if tt.cancelSync {
				select {
				case <-sender.ready:
					t.Fatal("canceled startup must not publish ready")
				default:
				}
				canceled, stop := context.WithCancel(t.Context())
				stop()
				assert.ErrorIs(t, sender.SyncRouteWithPeers(canceled, sandboxroute.Route{}), context.Canceled)
			}
		})
	}
}
