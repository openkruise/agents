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
	"sync"
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
	"github.com/openkruise/agents/pkg/configreload"
	"github.com/openkruise/agents/pkg/proxy"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/sandboxroute/refresh"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/web"
)

func TestControllerStartupWaitsForInitialSandboxRoutes(t *testing.T) {
	// The in-memory ListWatch serves a regular list, without watch-list bookmarks.
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.WatchListClient, false)
	for _, scenario := range []struct{ cancel, early bool }{{}, {cancel: true}, {early: true}, {cancel: true, early: true}} {
		cancelStartup := scenario.cancel
		name := "initial routes finish before serving"
		if cancelStartup {
			name = "cancel while initial route handler is blocked"
		}
		if scenario.early {
			name = "early listeners/" + name
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
			health.RecordWatchError(t.Context(), nil, errors.New("previous watch failed"))
			managerCache, err := cache.NewCacheWithHealth(mgr, health, true)
			require.NoError(t, err)
			opts := config.InitOptions(config.SandboxManagerOptions{DisableEnvoyExtProc: true})
			manager, err := sandboxmanager.NewSandboxManagerBuilder(opts).
				WithCustomInfra(func() (infra.Builder, error) {
					return sandboxcr.NewInfraBuilder(opts).WithCache(managerCache).WithAPIReader(client), nil
				}).Build()
			require.NoError(t, err)
			controller := NewController(ControllerOptions{ReadyBeforeConfig: scenario.early})
			if scenario.early {
				ready := make(chan struct{})
				close(ready)
				controller.configReady = ready
			}
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
			go func() {
				listener, err := controller.startComponents(ctx)
				if listener != nil {
					controller.serveHTTPServers(listener)
				}
				startupDone <- err
			}()
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
			if scenario.early {
				select {
				case addr := <-apiAddr:
					response, err := http.Get("http://" + addr + "/v2/sandboxes")
					require.NoError(t, err)
					require.NoError(t, response.Body.Close())
					require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
					apiAddr <- addr
				default:
					t.Fatal("early API listener was not started")
				}
			} else {
				select {
				case addr := <-apiAddr:
					t.Fatalf("API listener opened before initial routes synced: %s", addr)
				default:
				}
			}
			_, present := manager.GetOwnerOfSandbox("team-a--sandbox-a")
			assert.False(t, present)
			conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(refresh.DefaultPort)), 100*time.Millisecond)
			if conn != nil {
				require.NoError(t, conn.Close())
			}
			if scenario.early {
				require.NoError(t, err)
			} else {
				assert.Error(t, err, "proxy must not listen before initial route sync")
			}
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

func TestControllerStartupFailureWithPendingSignal(t *testing.T) {
	startupErr := errors.New("startup failed")
	// With both channels ready, either select branch must report the failure.
	for range 32 {
		controller := NewController(ControllerOptions{})
		ctx, cancel := context.WithCancel(t.Context())
		startupDone := make(chan startupResult, 1)
		startupDone <- startupResult{err: startupErr}
		stop := make(chan os.Signal, 1)
		stop <- syscall.SIGTERM
		err := controller.run(ctx, cancel, stop, startupDone)
		assert.ErrorIs(t, ctx.Err(), context.Canceled)
		cancel()
		require.ErrorIs(t, err, startupErr)
	}
}

type startupKeyStorage struct {
	lookupKeyStorage
	run     func()
	stopped chan struct{}
}

func (s *startupKeyStorage) Run()  { s.run() }
func (s *startupKeyStorage) Stop() { close(s.stopped) }

func TestControllerDrainsAfterSignalAtStartupCompletion(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		early bool
	}{{name: "default"}, {name: "early listeners", early: true}} {
		t.Run(scenario.name, func(t *testing.T) {
			controller := NewController(ControllerOptions{ReadyBeforeConfig: scenario.early})
			registerE2BRoute(controller, http.MethodGet, "/work", func(_ *http.Request) (web.ApiResponse[any], *web.ApiError) {
				return web.ApiResponse[any]{Code: http.StatusNoContent}, nil
			}, controller.CheckBusinessReady)
			store := &startupKeyStorage{stopped: make(chan struct{})}
			controller.keys = store
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer func() {
				if err := listener.Close(); !errors.Is(err, net.ErrClosed) {
					assert.NoError(t, err)
				}
			}()
			addr := listener.Addr().String()
			client := &http.Client{Timeout: time.Second, Transport: &http.Transport{}}
			defer client.CloseIdleConnections()
			fetch := func(want int) *http.Response {
				t.Helper()
				response, err := client.Get("http://" + addr + "/work")
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				require.Equal(t, want, response.StatusCode)
				return response
			}
			startup := startupResult{listener: listener}
			if scenario.early {
				controller.serveHTTPServers(listener)
				fetch(http.StatusServiceUnavailable)
				startup.listener = nil
			}

			// Keep the drain open until a business request has gone through it.
			finishTask := sync.OnceFunc(controller.activity.beginTask())
			defer finishTask()
			stop := make(chan os.Signal, 1)
			defer close(stop)
			startupDone := make(chan startupResult, 1)
			startupDone <- startup
			stop <- syscall.SIGTERM
			require.NoError(t, controller.run(ctx, cancel, stop, startupDone))
			require.Eventually(t, func() bool {
				controller.activity.mu.Lock()
				defer controller.activity.mu.Unlock()
				return controller.activity.draining
			}, time.Second, time.Millisecond, "the pending signal must start the drain")
			require.True(t, fetch(http.StatusNoContent).Close, "a drain response must retire the connection")
			finishTask()
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("shutdown did not finish after one signal and an idle window")
			}
			select {
			case <-store.stopped:
			default:
				t.Fatal("startup shutdown did not stop key storage")
			}
			rebound, err := net.Listen("tcp", addr)
			require.NoError(t, err, "shutdown must release the port")
			require.NoError(t, rebound.Close())
		})
	}
}

func TestControllerStartHTTPServerReportsBindFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { assert.NoError(t, listener.Close()) }()

	controller := newStartupController(t, func(context.Context) error { return nil })
	defer controller.manager.Stop(context.Background())
	controller.server.Addr = listener.Addr().String()
	keysStarted := false
	controller.keys = &startupKeyStorage{run: func() { keysStarted = true }, stopped: make(chan struct{})}
	_, err = controller.Run(nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "listen for E2B API")
	assert.False(t, keysStarted, "key storage must not start when the API port cannot bind")
}

func TestControllerSignalAfterAPIBindInterruptsStartup(t *testing.T) {
	controller := newStartupController(t, func(context.Context) error { return nil })
	defer controller.manager.Stop(context.Background())
	defer func() { assert.NoError(t, controller.server.Close()) }()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())
	controller.server.Addr = addr
	controller.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	keysStarted := make(chan struct{})
	releaseKeys := make(chan struct{})
	stopped := make(chan struct{})
	controller.keys = &startupKeyStorage{stopped: stopped, run: func() {
		close(keysStarted)
		<-releaseKeys
	}}
	stop := make(chan os.Signal, 1)
	defer func() {
		select {
		case stop <- syscall.SIGTERM:
		default:
		}
		select {
		case <-releaseKeys:
		default:
			close(releaseKeys)
		}
	}()
	type runResult struct {
		ctx context.Context
		err error
	}
	result := make(chan runResult, 1)
	go func() {
		ctx, err := controller.Run(stop)
		result <- runResult{ctx: ctx, err: err}
	}()
	select {
	case <-keysStarted:
	case got := <-result:
		t.Fatalf("Run returned before key storage started: %v", got.err)
	case <-time.After(time.Second):
		t.Fatal("key storage did not start")
	}

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	require.NoError(t, err, "the API port must already be bound")
	defer func() { assert.NoError(t, conn.Close()) }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(150*time.Millisecond)))
	_, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"))
	require.NoError(t, err)
	var response [1]byte
	_, err = conn.Read(response[:])
	require.ErrorIs(t, err, os.ErrDeadlineExceeded, "the API must wait for startup before serving")

	stop <- syscall.SIGTERM
	select {
	case got := <-result:
		require.NoError(t, got.err)
		require.ErrorIs(t, got.ctx.Err(), context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Run did not return after SIGTERM with key storage still starting")
	}

	close(releaseKeys)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("interrupted startup did not stop key storage")
	}
	require.Eventually(t, func() bool {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return false
		}
		return listener.Close() == nil
	}, time.Second, time.Millisecond, "interrupted startup must release the API port")
}

func TestReadyBeforeConfig(t *testing.T) {
	for _, stage := range []string{"activate", "cancel-config", "cancel-runtime", "runtime-error", "source-exit"} {
		t.Run(stage, func(t *testing.T) {
			runtimeEntered, releaseRuntime := make(chan struct{}), make(chan struct{})
			sc := newStartupController(t, func(ctx context.Context) error {
				close(runtimeEntered)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-releaseRuntime:
				}
				if stage == "runtime-error" {
					return errors.New("runtime initialization failed")
				}
				return nil
			})
			controller := NewController(ControllerOptions{ReadyBeforeConfig: true})
			controller.manager = sc.manager
			if stage == "cancel-config" {
				// Shutdown must also drain storage that never reached Run.
				controller.keys = keys.NewSecretKeyStorage(nil, nil, nil, "ns", "")
			}
			controller.registerRoutes()
			controller.server.Addr = "127.0.0.1:0"
			addr := make(chan string, 1)
			controller.server.BaseContext = func(l net.Listener) context.Context { addr <- l.Addr().String(); return context.Background() }
			provide, sourceExit := make(chan struct{}), make(chan struct{})
			controller.configSource = func(ctx context.Context, d *configreload.Dispatcher) error {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-sourceExit:
					return nil
				case <-provide:
				}
				if err := d.Apply(ctx, map[string]string{configreload.RedisUsername: "", configreload.RedisPassword: ""}); err != nil {
					return err
				}
				<-ctx.Done()
				return ctx.Err()
			}
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			require.NoError(t, controller.manager.ConfigureQuota(ctx, config.QuotaOptions{}, nil))
			require.NoError(t, controller.initializeConfiguration(ctx))
			stop := make(chan os.Signal, 1)
			result := make(chan error, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				runCtx, err := controller.Run(stop)
				result <- err
				if err == nil {
					<-runCtx.Done()
				}
			}()
			var address string
			t.Cleanup(func() {
				close(stop)
				select {
				case <-finished:
				case <-time.After(3 * time.Second):
					t.Fatal("controller cleanup timed out")
				}
				require.False(t, controller.businessReady())
				if address != "" {
					listener, err := net.Listen("tcp", address)
					require.NoError(t, err)
					require.NoError(t, listener.Close())
				}
			})
			select {
			case address = <-addr:
			case <-time.After(3 * time.Second):
				t.Fatal("API listener did not start before configuration")
			}
			client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
			defer client.CloseIdleConnections()
			fetch := func(path string, want int) {
				t.Helper()
				r, err := client.Get("http://" + address + path)
				require.NoError(t, err)
				require.NoError(t, r.Body.Close())
				require.Equal(t, want, r.StatusCode)
			}
			const listPath = "/v2/sandboxes?limit=invalid"
			if stage == "activate" {
				fetch("/health", http.StatusOK)
				fetch("/metrics", http.StatusOK)
				fetch("/kruise/api/health", http.StatusOK)
				fetch(listPath, http.StatusServiceUnavailable)
				fetch("/kruise/api/v2/sandboxes", http.StatusServiceUnavailable)
				conn, err := net.DialTimeout("tcp", "127.0.0.1:7789", time.Second)
				require.NoError(t, err)
				require.NoError(t, conn.Close())
			}
			select {
			case <-runtimeEntered:
				t.Fatal("runtime started without configuration")
			default:
			}
			switch stage {
			case "cancel-config":
				stop <- syscall.SIGTERM
			case "source-exit":
				close(sourceExit)
			default:
				close(provide)
				select {
				case <-runtimeEntered:
				case <-time.After(time.Second):
					t.Fatal("runtime did not start")
				}
				if stage == "activate" {
					fetch(listPath, http.StatusServiceUnavailable)
				}
				if stage == "cancel-runtime" {
					stop <- syscall.SIGTERM
				} else {
					close(releaseRuntime)
				}
			}
			select {
			case err := <-result:
				if stage == "runtime-error" || stage == "source-exit" {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Controller.Run did not return")
			}
			if stage == "activate" {
				fetch(listPath, http.StatusBadRequest)
			}
		})
	}
}
