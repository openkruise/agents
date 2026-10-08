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

package e2b

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	clientfeatures "k8s.io/client-go/features"
	clientfeaturestesting "k8s.io/client-go/features/testing"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/proxy"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/sandboxroute/refresh"
)

func TestControllerStartupWaitsForInitialSandboxRoutes(t *testing.T) {
	// The in-memory ListWatch serves a regular list, without watch-list bookmarks.
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.WatchListClient, false)
	for _, cancelStartup := range []bool{false, true} {
		name := "initial routes finish before serving"
		if cancelStartup {
			name = "cancel while initial route handler is blocked"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sandbox := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "team-a", Name: "sandbox-a", UID: "uid-a", ResourceVersion: "10",
					Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: "owner-a"},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase:   agentsv1alpha1.SandboxRunning,
					PodInfo: agentsv1alpha1.PodInfo{PodIP: "10.0.0.1"},
				},
			}
			watcher := watch.NewRaceFreeFake()
			informer := &startupRouteInformer{
				SharedIndexInformer: toolscache.NewSharedIndexInformer(&toolscache.ListWatch{
					ListFunc: func(metav1.ListOptions) (runtime.Object, error) {
						return &agentsv1alpha1.SandboxList{Items: []agentsv1alpha1.Sandbox{*sandbox}}, nil
					},
					WatchFunc: func(metav1.ListOptions) (watch.Interface, error) { return watcher, nil },
				}, &agentsv1alpha1.Sandbox{}, 0, toolscache.Indexers{}),
				entered: make(chan struct{}), release: make(chan struct{}),
			}
			fakeCache, client, err := cachetest.NewTestCache(t, sandbox)
			require.NoError(t, err)
			base := fakeCache.GetMockManager()
			ctrlCache := &startupRouteCache{Cache: base.GetCache(), informer: informer}
			mgr := &startupRouteManager{Manager: base, cache: ctrlCache, done: make(chan struct{})}
			health := cache.NewInformerHealth()
			// Watch recovery health is intentionally stricter than startup sync.
			health.RecordWatchError(nil, errors.New("previous watch failed"))
			managerCache, err := cache.NewCacheWithHealth(mgr, health, true)
			require.NoError(t, err)
			opts := config.InitOptions(config.SandboxManagerOptions{DisableEnvoyExtProc: true})
			manager, err := sandboxmanager.NewSandboxManagerBuilder(opts).
				WithCustomInfra(func() (infra.Builder, error) {
					return sandboxcr.NewInfraBuilder(opts).WithCache(managerCache).WithAPIReader(client), nil
				}).Build()
			require.NoError(t, err)
			controller := NewController(ControllerOptions{})
			controller.registerRoutes()
			controller.manager = manager
			controller.server.Addr = "127.0.0.1:0"
			apiAddr := make(chan string, 1)
			controller.server.BaseContext = func(listener net.Listener) context.Context {
				apiAddr <- listener.Addr().String()
				return ctx
			}
			t.Cleanup(func() {
				cancel()
				select {
				case <-informer.release:
				default:
					close(informer.release)
				}
				manager.Stop(context.Background())
				require.NoError(t, controller.server.Close())
				watcher.Stop()
			})
			startupDone := make(chan error, 1)
			go func() { startupDone <- controller.startComponents(ctx) }()
			select {
			case <-informer.entered:
			case err := <-startupDone:
				t.Fatalf("startup exited before the initial route handler: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("initial route handler was not called")
			}
			require.Eventually(t, informer.HasSynced, time.Second, time.Millisecond)
			select {
			case err := <-startupDone:
				t.Fatalf("startup returned before the initial route handler finished: %v", err)
			case <-time.After(150 * time.Millisecond):
			}
			select {
			case addr := <-apiAddr:
				t.Fatalf("API listener opened before initial routes synced: %s", addr)
			default:
			}
			_, present := manager.GetOwnerOfSandbox("team-a--sandbox-a")
			assert.False(t, present)
			conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(refresh.DefaultPort)), 100*time.Millisecond)
			if conn != nil {
				require.NoError(t, conn.Close())
			}
			assert.Error(t, err, "proxy must not listen before initial route sync")
			if cancelStartup {
				cancel()
				select {
				case err := <-startupDone:
					require.ErrorIs(t, err, context.Canceled)
					require.ErrorContains(t, err, "initial sandbox event handlers")
				case <-time.After(time.Second):
					t.Fatal("startup did not return after cancellation")
				}
			}
			close(informer.release)
			if !cancelStartup {
				select {
				case err := <-startupDone:
					require.NoError(t, err)
				case <-time.After(3 * time.Second):
					t.Fatal("startup did not finish after initial route sync")
				}
				owner, present := manager.GetOwnerOfSandbox("team-a--sandbox-a")
				require.True(t, present)
				assert.Equal(t, "owner-a", owner)
				assert.False(t, managerCache.SandboxInformerHealthy(), "watch settle still protects quota maintenance")
				select {
				case addr := <-apiAddr:
					response, err := http.Get("http://" + addr + "/health")
					require.NoError(t, err)
					require.NoError(t, response.Body.Close())
					assert.Equal(t, http.StatusOK, response.StatusCode)
				case <-time.After(time.Second):
					t.Fatal("API server was not started")
				}
			}
			cancel()
			select {
			case <-mgr.done:
			case <-time.After(time.Second):
				t.Fatal("informer did not stop after cancellation")
			}
		})
	}
}

type startupRouteInformer struct {
	toolscache.SharedIndexInformer
	entered chan struct{}
	release chan struct{}
}

func (i *startupRouteInformer) AddEventHandler(handler toolscache.ResourceEventHandler) (toolscache.ResourceEventHandlerRegistration, error) {
	return i.SharedIndexInformer.AddEventHandler(toolscache.ResourceEventHandlerDetailedFuncs{
		AddFunc: func(obj any, initial bool) {
			close(i.entered)
			<-i.release
			handler.OnAdd(obj, initial)
		},
		UpdateFunc: handler.OnUpdate, DeleteFunc: handler.OnDelete,
	})
}

type startupRouteCache struct {
	ctrlcache.Cache
	informer *startupRouteInformer
}

func (c *startupRouteCache) GetInformer(_ context.Context, obj ctrlclient.Object, _ ...ctrlcache.InformerGetOption) (ctrlcache.Informer, error) {
	if _, ok := obj.(*agentsv1alpha1.Sandbox); ok {
		return c.informer, nil
	}
	return nil, nil
}

func (c *startupRouteCache) WaitForCacheSync(ctx context.Context) bool {
	return toolscache.WaitForCacheSync(ctx.Done(), c.informer.HasSynced)
}

type startupRouteManager struct {
	ctrl.Manager
	cache *startupRouteCache
	done  chan struct{}
}

func (m *startupRouteManager) GetCache() ctrlcache.Cache { return m.cache }

func (m *startupRouteManager) Start(ctx context.Context) error {
	defer close(m.done)
	m.cache.informer.Run(ctx.Done())
	return nil
}

// newStartupController builds a controller whose infra Run is replaced by
// run, for exercising Controller.Run startup paths. The ext-proc listener is
// disabled so only the route-refresh port would be bound if startup ever got
// far enough to start the proxy.
func newStartupController(t *testing.T, run func(context.Context) error) *Controller {
	t.Helper()
	opts := config.InitOptions(config.SandboxManagerOptions{DisableEnvoyExtProc: true})
	fakeCache, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	proxyServer := proxy.NewServer(opts)
	manager, err := sandboxmanager.NewSandboxManagerBuilder(opts).
		WithCustomInfra(func() (infra.Builder, error) {
			return hookInfraBuilder{
				base: sandboxcr.NewInfraBuilder(opts).
					WithCache(fakeCache).
					WithAPIReader(fc).
					WithRouteReader(proxyServer),
				run: run,
			}, nil
		}).
		Build()
	require.NoError(t, err)
	return &Controller{
		server:  &http.Server{Addr: "127.0.0.1:0"},
		manager: manager,
	}
}

// TestControllerSignalDuringStartupExitsCleanly pins the startup crash
// semantics: a termination signal that lands while the manager is still
// starting makes Run return a canceled context with no error, so main exits
// zero without waiting for startup or running per-component cleanup.
func TestControllerSignalDuringStartupExitsCleanly(t *testing.T) {
	started := make(chan struct{})
	controller := newStartupController(t, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})

	type runResult struct {
		ctx context.Context
		err error
	}
	result := make(chan runResult, 1)
	stop := make(chan os.Signal, 1)
	go func() {
		ctx, err := controller.Run(stop)
		result <- runResult{ctx: ctx, err: err}
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for manager startup to block")
	}
	stop <- syscall.SIGTERM

	select {
	case got := <-result:
		require.NoError(t, got.err)
		require.ErrorIs(t, got.ctx.Err(), context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Run did not return after SIGTERM during startup")
	}
}

// TestControllerRunPropagatesStartupFailure pins that startup errors surface
// synchronously from Run instead of crashing from a background goroutine.
func TestControllerRunPropagatesStartupFailure(t *testing.T) {
	controller := newStartupController(t, func(context.Context) error {
		return errors.New("cache sync failed")
	})

	_, err := controller.Run(make(chan os.Signal, 1))
	require.Error(t, err)
	assert.ErrorContains(t, err, "sandbox manager failed to start")
}

func TestAwaitStartup(t *testing.T) {
	tests := []struct {
		name     string
		startup  func(startupDone chan error, stop chan os.Signal)
		expected []startupOutcome
		errMsg   string
	}{
		{
			name:     "startup succeeds",
			startup:  func(startupDone chan error, _ chan os.Signal) { startupDone <- nil },
			expected: []startupOutcome{startupCompleted},
		},
		{
			name:     "startup fails",
			startup:  func(startupDone chan error, _ chan os.Signal) { startupDone <- errors.New("boom") },
			expected: []startupOutcome{startupFailed},
			errMsg:   "boom",
		},
		{
			name:     "signal interrupts startup",
			startup:  func(_ chan error, stop chan os.Signal) { stop <- syscall.SIGTERM },
			expected: []startupOutcome{startupInterrupted},
		},
		{
			// The outer select picks either ready channel; both branches must
			// report the completed startup so the caller shuts down gracefully.
			name: "signal pending with completed startup is never an interruption",
			startup: func(startupDone chan error, stop chan os.Signal) {
				startupDone <- nil
				stop <- syscall.SIGTERM
			},
			expected: []startupOutcome{startupCompleted, startupSignaled},
		},
		{
			name: "signal pending with failed startup reports the failure",
			startup: func(startupDone chan error, stop chan os.Signal) {
				startupDone <- errors.New("boom")
				stop <- syscall.SIGTERM
			},
			expected: []startupOutcome{startupFailed},
			errMsg:   "boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Repeat so rows with two ready channels exercise both select
			// branches in practice.
			for range 32 {
				startupDone := make(chan error, 1)
				stop := make(chan os.Signal, 1)
				tt.startup(startupDone, stop)

				outcome, err := awaitStartup(startupDone, stop)
				assert.Contains(t, tt.expected, outcome)
				if tt.errMsg != "" {
					assert.ErrorContains(t, err, tt.errMsg)
				} else {
					assert.NoError(t, err)
				}
			}
		})
	}
}

func TestControllerStartHTTPServerReportsBindFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	controller := &Controller{server: &http.Server{Addr: listener.Addr().String()}}
	err = controller.startHTTPServer()
	require.Error(t, err)
	assert.ErrorContains(t, err, "listen for E2B API")
}
