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
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/sandbox/core"
	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/sandboxroute"
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
	tests := []struct {
		name     string
		selector string
		wantErr  string
	}{
		{name: "默认关闭"},
		{name: "selector 必须合法", selector: "component in (", wantErr: "parse peer selector"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// All cases must return before accessing the manager or starting listeners.
			sender, err := SetupRouteSync(nil, tt.selector)
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
		name       string
		cancelSync bool
	}{
		{name: "明文启动并退出"},
		{name: "缓存未同步时可以取消", cancelSync: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("POD_NAME", "controller-route-sync-test")
			t.Setenv("POD_IP", "127.0.0.1")
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
				assert.Equal(t, "component=sandbox-manager", r.URL.Query().Get("labelSelector"))
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
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			port := listener.Addr().(*net.TCPAddr).Port
			require.NoError(t, listener.Close())
			sender, err := SetupRouteSync(mgr, "component=sandbox-manager")
			require.NoError(t, err)
			require.NotNil(t, sender)
			assert.Equal(t, 7946, sender.bindPort)
			// Override the instance port only in tests to avoid occupying the fixed production port.
			sender.bindPort = port
			assert.NotSame(t, mgr.GetCache(), sender.GetCache())
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
				assert.Equal(t, peers.NodePrefixSandboxController+"controller-route-sync-test", members[0].Name)
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
