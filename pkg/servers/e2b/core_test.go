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

package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/proxy"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/utils/network"
	utilruntime "github.com/openkruise/agents/pkg/utils/runtime"
	"github.com/openkruise/agents/pkg/utils/testutils"
)

var TestServerPort = 9999
var Namespace = models.AdminTeamName
var InitKey = "admin-987654321"

// adminTestUser returns the canonical admin API key used across handler tests.
// Tests needing Team or other fields keep constructing their own literal.
func adminTestUser() *models.CreatedTeamAPIKey {
	return &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}
}

func CreateSandboxWithStatus(t *testing.T, c ctrlclient.Client, sbx *agentsv1alpha1.Sandbox) {
	t.Helper()
	ctx := t.Context()
	err := c.Create(ctx, sbx)
	assert.NoError(t, err)
	err = c.Status().Update(ctx, sbx)
	assert.NoError(t, err)
}

func Setup(t *testing.T) (*Controller, ctrlclient.Client, func()) {
	return setupWithQuota(t, nil)
}

func SetupWithQuota(t *testing.T, enforcer sandboxmanager.QuotaEnforcer) (*Controller, ctrlclient.Client, func()) {
	return setupWithQuota(t, enforcer)
}

func refreshKeyStorageForTest(t *testing.T, controller *Controller) {
	t.Helper()
	require.NoError(t, controller.keys.Init(t.Context()))
}

// TestNewController pins the ControllerOptions -> Controller mapping so a field
// cannot be silently dropped by the constructor, and so the manager options
// reach the sandbox-manager builder verbatim.
func TestNewController(t *testing.T) {
	keyCfg := &keys.Config{Mode: keys.StorageModeSecret, Namespace: "sandbox-system"}
	bundle := &utilruntime.TLSBundle{CABundle: []byte("pem")}
	mgrOpts := config.SandboxManagerOptions{
		SystemNamespace:       "sandbox-system",
		PeerSelector:          "component=sandbox-manager",
		BindAddress:           "10.0.0.2",
		SandboxNamespace:      "sandboxes",
		SandboxLabelSelector:  "app=sandbox",
		MaxClaimWorkers:       7,
		MaxCreateQPS:          11,
		ExtProcMaxConcurrency: 13,
		MemberlistBindPort:    config.DefaultMemberlistBindPort,
		Quota:                 config.QuotaOptions{RedisAddr: "127.0.0.1:6379"},
	}

	tests := []struct {
		name string
		opts ControllerOptions
	}{
		{
			name: "fully populated options",
			opts: ControllerOptions{
				Domain:           "example.com",
				Port:             8080,
				MaxTimeout:       3600,
				KeyConfig:        keyCfg,
				Manager:          mgrOpts,
				RuntimeTLSBundle: bundle,
			},
		},
		{
			name: "zero options leave auth and runtime TLS disabled",
			opts: ControllerOptions{},
		},
		{
			name: "dedicated metrics port",
			opts: ControllerOptions{
				Port:        8080,
				MetricsPort: 9090,
			},
		},
		{
			name: "equal metrics port reuses API listener",
			opts: ControllerOptions{
				Port:        8080,
				MetricsPort: 8080,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := NewController(tt.opts)
			require.NotNil(t, sc)
			sc.registerRoutes()

			assert.Equal(t, tt.opts.Domain, sc.domain)
			assert.Equal(t, tt.opts.MaxTimeout, sc.maxTimeout)
			assert.Equal(t, tt.opts.KeyConfig, sc.keyCfg)
			assert.Equal(t, tt.opts.RuntimeTLSBundle, sc.runtimeTLSBundle)
			// The manager options are handed to the sandbox-manager builder
			// unchanged, so they must survive the constructor verbatim.
			assert.Equal(t, tt.opts.Manager, sc.mgrOpts)

			// Port is not retained as a field; it only shapes the server address.
			require.NotNil(t, sc.server)
			assert.Equal(t, network.ListenAddress(tt.opts.Manager.BindAddress, tt.opts.Port), sc.server.Addr)
			if tt.opts.Manager.BindAddress != "" {
				assert.Equal(t, fmt.Sprintf("%s:%d", tt.opts.Manager.BindAddress, tt.opts.Port), sc.adapter.Entry())
			}
			assert.NotNil(t, sc.mux)
			assert.NotNil(t, sc.adapter)

			status := func(h http.Handler, path string) int {
				t.Helper()
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				return rec.Code
			}
			// /health always stays on the control API listener.
			assert.Equal(t, http.StatusOK, status(sc.mux, "/health"))
			assert.Equal(t, http.StatusOK, status(sc.mux, "/kruise/api/health"))
			if tt.opts.MetricsPort > 0 && tt.opts.MetricsPort != tt.opts.Port {
				require.NotNil(t, sc.metricsServer)
				assert.Equal(t, fmt.Sprintf(":%d", tt.opts.MetricsPort), sc.metricsServer.Addr)
				assert.Equal(t, http.StatusNotFound, status(sc.mux, "/metrics"))
				assert.Equal(t, http.StatusOK, status(sc.metricsServer.Handler, "/metrics"))
				assert.Equal(t, http.StatusNotFound, status(sc.metricsServer.Handler, "/health"))
				assert.Equal(t, http.StatusNotFound, status(sc.metricsServer.Handler, "/kruise/api/health"))
			} else {
				assert.Nil(t, sc.metricsServer)
				assert.Equal(t, http.StatusOK, status(sc.mux, "/metrics"))
			}
		})
	}
}

func SetupWithMinResumeTimeout(t *testing.T, _ int) (*Controller, ctrlclient.Client, func()) {
	return setupWithQuota(t, nil)
}

func setupWithQuota(t *testing.T, quotaEnforcer sandboxmanager.QuotaEnforcer) (*Controller, ctrlclient.Client, func()) {
	testutils.InitLogOutput()
	namespace := "sandbox-system"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	opts := config.InitOptions(config.SandboxManagerOptions{
		SystemNamespace:    namespace,
		MaxClaimWorkers:    10,
		MemberlistBindPort: config.DefaultMemberlistBindPort,
	})
	cache, fc, cacheErr := cachetest.NewTestCache(t)
	require.NoError(t, cacheErr)
	// The controller gets its own copy so the peer selector it advertises does not
	// leak into the proxy/infra built from opts below.
	ctrlMgrOpts := opts
	ctrlMgrOpts.PeerSelector = "component=sandbox-manager"
	controller := NewController(ControllerOptions{
		Domain:     "example.com",
		Port:       TestServerPort,
		MaxTimeout: models.DefaultMaxTimeout,
		KeyConfig: &keys.Config{
			Mode:      keys.StorageModeSecret,
			Namespace: namespace,
			AdminKey:  InitKey,
			Client:    fc,
			APIReader: fc,
		},
		Manager: ctrlMgrOpts,
	})

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-manager",
			Namespace: namespace,
			Labels:    map[string]string{"component": "sandbox-manager"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			PodIP: "127.0.0.1",
		},
	}
	require.NoError(t, fc.Create(t.Context(), pod))
	require.NoError(t, fc.Status().Update(t.Context(), pod))

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: keys.KeySecretName, Namespace: namespace},
		Data:       map[string][]byte{},
	}
	require.NoError(t, fc.Create(t.Context(), secret))

	proxyServer := proxy.NewServer(opts)
	infraInstance := sandboxcr.NewInfraBuilder(opts).
		WithCache(cache).
		WithAPIReader(fc).
		WithRouteReader(proxyServer).
		Build()
	require.NoError(t, infraInstance.Run(t.Context()))

	sandboxManager, err := sandboxmanager.NewSandboxManagerBuilder(opts).
		WithRequestAdapter(controller.adapter).
		WithQuotaEnforcer(quotaEnforcer).
		WithCustomInfra(func() (infra.Builder, error) {
			return sandboxcr.NewInfraBuilder(opts).
				WithCache(cache).
				WithAPIReader(fc).
				WithRouteReader(proxyServer), nil
		}).
		Build()
	require.NoError(t, err)

	controller.cache = cache
	controller.manager = sandboxManager
	controller.storageRegistry = storages.NewStorageProvider()
	controller.registerRoutes()

	require.NoError(t, controller.initKeyStorage(t.Context()))

	if quotaEnforcer == nil {
		// Initialize quota through the manager (mirrors Init() logic).
		if controller.keys != nil {
			require.NoError(t, controller.manager.InitQuota(t.Context(), config.QuotaOptions{}, keys.NewQuotaSubjectLister(controller.keys)))
		} else {
			require.NoError(t, controller.manager.InitQuota(t.Context(), config.QuotaOptions{}, nil))
		}
	}

	serverErr := make(chan error, 1)
	go func() {
		if err := controller.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	return controller, fc, func() {
		t.Helper()
		_ = controller.server.Close()
		require.NoError(t, <-serverErr)
	}
}

func TestNewControllerPropagatesShortSandboxIDOption(t *testing.T) {
	tests := []struct {
		name                 string
		enableShortSandboxID bool
		shortSandboxIDPrefix string
	}{
		{name: "disabled", enableShortSandboxID: false},
		{name: "enabled with prefix", enableShortSandboxID: true, shortSandboxIDPrefix: "prod-"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := NewController(ControllerOptions{
				Manager: config.SandboxManagerOptions{
					EnableShortSandboxID: tt.enableShortSandboxID,
					ShortSandboxIDPrefix: tt.shortSandboxIDPrefix,
				},
			})

			assert.Equal(t, tt.enableShortSandboxID, controller.mgrOpts.EnableShortSandboxID)
			assert.Equal(t, tt.shortSandboxIDPrefix, controller.mgrOpts.ShortSandboxIDPrefix)
		})
	}
}

// hookInfraBuilder builds hookInfra around a real sandboxcr infra.
type hookInfraBuilder struct {
	base infra.Builder
	run  func(context.Context) error
	stop func()
}

func (b hookInfraBuilder) Build() infra.Infrastructure {
	return hookInfra{Infrastructure: b.base.Build(), run: b.run, stop: b.stop}
}

// hookInfra lets tests replace Run and observe Stop on an otherwise real
// Infrastructure; its route source never emits events.
type hookInfra struct {
	infra.Infrastructure
	run  func(context.Context) error
	stop func()
}

func (i hookInfra) Run(ctx context.Context) error {
	if i.run != nil {
		return i.run(ctx)
	}
	return i.Infrastructure.Run(ctx)
}

func (i hookInfra) Stop(ctx context.Context) {
	if i.stop != nil {
		i.stop()
	}
	i.Infrastructure.Stop(ctx)
}

func (hookInfra) GetSandboxRouteSource() infra.SandboxRouteSource {
	return noOpSandboxRouteSource{}
}

type noOpSandboxRouteSource struct{}

func (noOpSandboxRouteSource) Subscribe(context.Context, infra.SandboxRouteEventHandler) error {
	return nil
}

func newStopProbeManager(t *testing.T, stop func()) *sandboxmanager.SandboxManager {
	t.Helper()
	opts := config.InitOptions(config.SandboxManagerOptions{
		SystemNamespace:    "sandbox-system",
		MemberlistBindPort: config.DefaultMemberlistBindPort,
	})
	fakeCache, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	proxyServer := proxy.NewServer(opts)
	mgr, err := sandboxmanager.NewSandboxManagerBuilder(opts).
		WithCustomInfra(func() (infra.Builder, error) {
			return hookInfraBuilder{
				base: sandboxcr.NewInfraBuilder(opts).
					WithCache(fakeCache).
					WithAPIReader(fc).
					WithRouteReader(proxyServer),
				stop: stop,
			}, nil
		}).
		Build()
	require.NoError(t, err)
	return mgr
}

func TestControllerRunStartsDedicatedObservabilityListener(t *testing.T) {
	sc := NewController(ControllerOptions{Port: 8080, MetricsPort: 9090})
	sc.registerRoutes()
	sc.manager = newStopProbeManager(t, func() {})

	apiAddr := make(chan string, 1)
	metricsAddr := make(chan string, 1)
	sc.server.Addr = "127.0.0.1:0"
	sc.server.BaseContext = func(listener net.Listener) context.Context {
		apiAddr <- listener.Addr().String()
		return context.Background()
	}
	sc.metricsServer.Addr = "127.0.0.1:0"
	sc.metricsServer.BaseContext = func(listener net.Listener) context.Context {
		metricsAddr <- listener.Addr().String()
		return context.Background()
	}

	stop := make(chan os.Signal, 1)
	runCtx, err := sc.Run(stop)
	require.NoError(t, err)
	t.Cleanup(func() {
		select {
		case stop <- syscall.SIGTERM:
		case <-runCtx.Done():
		}
		select {
		case <-runCtx.Done():
		case <-time.After(5 * time.Second):
			t.Error("timed out waiting for controller shutdown")
		}
	})

	waitAddr := func(name string, ch <-chan string) string {
		t.Helper()
		select {
		case addr := <-ch:
			return addr
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s listener", name)
			return ""
		}
	}
	controlAddr := waitAddr("control API", apiAddr)
	observabilityAddr := waitAddr("observability", metricsAddr)

	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	t.Cleanup(client.CloseIdleConnections)
	status := func(addr, path string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		return resp.StatusCode
	}

	assert.Equal(t, http.StatusOK, status(controlAddr, "/health"))
	assert.Equal(t, http.StatusOK, status(controlAddr, "/kruise/api/health"))
	assert.Equal(t, http.StatusNotFound, status(controlAddr, "/metrics"))
	assert.Equal(t, http.StatusNotFound, status(observabilityAddr, "/health"))
	assert.Equal(t, http.StatusNotFound, status(observabilityAddr, "/kruise/api/health"))
	assert.Equal(t, http.StatusOK, status(observabilityAddr, "/metrics"))
	assert.Equal(t, http.StatusMethodNotAllowed, status(controlAddr, "/sandboxes"))
	assert.Equal(t, http.StatusNotFound, status(observabilityAddr, "/sandboxes"))
}

func TestControllerShutdownStopsManagerAfterHTTPShutdown(t *testing.T) {
	tests := []struct {
		name        string
		withMetrics bool
	}{
		{name: "api listener only"},
		{name: "api and metrics listeners", withMetrics: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{
				Timeout:   5 * time.Second,
				Transport: &http.Transport{Proxy: nil},
			}
			t.Cleanup(client.CloseIdleConnections)

			type liveServer struct {
				server    *http.Server
				release   chan struct{}
				onStop    chan struct{}
				clientErr chan error
				serveErr  chan error
			}
			start := func() *liveServer {
				t.Helper()
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, err)
				s := &liveServer{
					release:   make(chan struct{}),
					onStop:    make(chan struct{}),
					clientErr: make(chan error, 1),
					serveErr:  make(chan error, 1),
				}
				started := make(chan struct{})
				s.server = &http.Server{
					Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						close(started)
						<-s.release
						_, _ = w.Write([]byte("ok"))
					}),
				}
				s.server.RegisterOnShutdown(func() { close(s.onStop) })
				go func() { s.serveErr <- s.server.Serve(ln) }()
				go func() {
					resp, err := client.Get("http://" + ln.Addr().String())
					if err != nil {
						s.clientErr <- err
						return
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					s.clientErr <- resp.Body.Close()
				}()
				select {
				case <-started:
				case <-time.After(time.Second):
					t.Fatal("timed out waiting for request to start")
				}
				return s
			}

			api := start()
			sc := &Controller{server: api.server}
			servers := []*liveServer{api}
			if tt.withMetrics {
				metrics := start()
				sc.metricsServer = metrics.server
				servers = append(servers, metrics)
			}

			cancelCalled := atomic.Bool{}
			managerStopped := make(chan struct{})
			sc.manager = newStopProbeManager(t, func() { close(managerStopped) })

			shutdownCtx, shutdownCancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer shutdownCancel()
			shutdownDone := make(chan struct{})
			go func() {
				sc.shutdown(shutdownCtx, func() { cancelCalled.Store(true) })
				close(shutdownDone)
			}()

			for _, s := range servers {
				select {
				case <-s.onStop:
				case <-time.After(time.Second):
					t.Fatal("timed out waiting for HTTP shutdown to start")
				}
			}
			for _, s := range servers[:len(servers)-1] {
				close(s.release)
			}
			select {
			case <-managerStopped:
				t.Fatal("manager.Stop must not run while HTTP requests are draining")
			case <-shutdownDone:
				t.Fatal("shutdown completed before the active request drained")
			case <-time.After(50 * time.Millisecond):
			}
			assert.False(t, cancelCalled.Load(), "server context must remain active while HTTP requests drain")

			close(servers[len(servers)-1].release)
			select {
			case <-shutdownDone:
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for shutdown to finish")
			}
			select {
			case <-managerStopped:
			default:
				t.Fatal("manager.Stop must run after HTTP requests drain")
			}
			for _, s := range servers {
				require.NoError(t, <-s.clientErr)
				require.ErrorIs(t, <-s.serveErr, http.ErrServerClosed)
			}
			assert.True(t, cancelCalled.Load())
		})
	}
}

func NewRequest(t *testing.T, query map[string]string, body any, pathValues map[string]string, user *models.CreatedTeamAPIKey) *http.Request {
	var bodyBuf io.Reader
	if body != nil {
		marshal, err := json.Marshal(body)
		require.NoError(t, err)
		bodyBuf = bytes.NewBuffer(marshal)
	}
	urlStr := fmt.Sprintf("http://127.0.0.1:%d", TestServerPort)
	if query != nil {
		q := url.Values{}
		for k, v := range query {
			q.Set(k, v)
		}
		urlStr += "?" + q.Encode()
	}
	req, err := http.NewRequest("", urlStr, bodyBuf)
	require.NoError(t, err)
	if pathValues != nil {
		for k, v := range pathValues {
			req.SetPathValue(k, v)
		}
	}
	return req.WithContext(context.WithValue(req.Context(), userContextKey, user))
}

func GetSbsOwnerReference(sbs *agentsv1alpha1.SandboxSet) []metav1.OwnerReference {
	return []metav1.OwnerReference{*metav1.NewControllerRef(sbs, agentsv1alpha1.SandboxSetControllerKind)}
}

type CreateSandboxPoolOptions struct {
	Namespace   string
	RuntimeURL  string
	AccessToken string
	CPURequest  string
	Memory      string
}

func CreateSandboxPool(t *testing.T, controller *Controller, name string, available int, opts ...CreateSandboxPoolOptions) func() {
	var options CreateSandboxPoolOptions
	if len(opts) > 0 {
		options = opts[0]
	}
	ns := Namespace
	if options.Namespace != "" {
		ns = options.Namespace
	}
	container := corev1.Container{Name: "main", Image: "old-image"}
	if options.CPURequest != "" || options.Memory != "" {
		container.Resources.Requests = corev1.ResourceList{}
		container.Resources.Limits = corev1.ResourceList{}
		if options.CPURequest != "" {
			container.Resources.Requests[corev1.ResourceCPU] = resource.MustParse(options.CPURequest)
			container.Resources.Limits[corev1.ResourceCPU] = resource.MustParse(options.CPURequest)
		}
		if options.Memory != "" {
			container.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(options.Memory)
			container.Resources.Limits[corev1.ResourceMemory] = resource.MustParse(options.Memory)
		}
	}
	tmpl := agentsv1alpha1.EmbeddedSandboxTemplate{
		Template: &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{container}},
		},
	}
	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uuid.NewString())},
		Spec:       agentsv1alpha1.SandboxSetSpec{EmbeddedSandboxTemplate: tmpl},
	}
	fc := getTestCRClient(controller)
	err := fc.Create(t.Context(), sbs)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
	now := metav1.Now()
	for i := 0; i < available; i++ {
		annotations := map[string]string{}
		if options.RuntimeURL != "" {
			annotations[agentsv1alpha1.AnnotationRuntimeURL] = options.RuntimeURL
		}
		if options.AccessToken != "" {
			annotations[agentsv1alpha1.AnnotationRuntimeAccessToken] = options.AccessToken
		}
		sbx := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("%s-%d", name, i), Namespace: ns,
				Labels: map[string]string{
					agentsv1alpha1.LabelSandboxTemplate:  name,
					agentsv1alpha1.LabelSandboxIsClaimed: "false",
				},
				Annotations: annotations, OwnerReferences: GetSbsOwnerReference(sbs),
				UID: types.UID(uuid.NewString()), CreationTimestamp: now,
			},
			Spec: agentsv1alpha1.SandboxSpec{EmbeddedSandboxTemplate: tmpl},
			Status: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
				Conditions: []metav1.Condition{{
					Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue,
				}},
				PodInfo: agentsv1alpha1.PodInfo{PodIP: "1.2.3.4"},
			},
		}
		CreateSandboxWithStatus(t, fc, sbx)
	}
	require.Eventually(t, func() bool {
		pool, _ := controller.cache.ListSandboxesInPool(t.Context(), infracache.ListSandboxesInPoolOptions{Namespace: ns, Pool: name})
		return len(pool) == available
	}, time.Minute, 100*time.Millisecond)
	return func() {
		for i := 0; i < available; i++ {
			sbx := &agentsv1alpha1.Sandbox{}
			sbx.Name = fmt.Sprintf("%s-%d", name, i)
			sbx.Namespace = ns
			assert.NoError(t, fc.Delete(t.Context(), sbx))
		}
		sbs.Namespace = ns
		assert.NoError(t, fc.Delete(t.Context(), sbs))
	}
}

func CreateClaimedSandboxCR(t *testing.T, controller *Controller, namespace, name, template, owner string, annotations map[string]string) *agentsv1alpha1.Sandbox {
	t.Helper()
	fc := getTestCRClient(controller)
	now := metav1.Now()
	copiedAnnotations := map[string]string{
		agentsv1alpha1.AnnotationClaimTime: now.Format(time.RFC3339),
		agentsv1alpha1.AnnotationOwner:     owner,
	}
	for key, value := range annotations {
		copiedAnnotations[key] = value
	}
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxTemplate:  template,
				agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True,
			},
			Annotations: copiedAnnotations, UID: types.UID(uuid.NewString()), CreationTimestamp: now,
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "test-image"}}},
				},
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{{
				Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue,
			}},
			PodInfo: agentsv1alpha1.PodInfo{PodIP: "1.2.3.4"},
		},
	}
	CreateSandboxWithStatus(t, fc, sbx)
	sandboxID := fmt.Sprintf("%s--%s", namespace, name)
	require.Eventually(t, func() bool {
		_, err := controller.cache.GetClaimedSandbox(t.Context(), infracache.GetClaimedSandboxOptions{Namespace: namespace, SandboxID: sandboxID})
		return err == nil
	}, time.Second, 10*time.Millisecond)
	return sbx
}

func CreateCheckpointAndTemplateInNamespace(t *testing.T, controller *Controller, namespace, name, checkpointID, owner, sandboxID, creationTime string) func() {
	t.Helper()
	fc := getTestCRClient(controller)
	createdAt, err := time.Parse(time.RFC3339, creationTime)
	require.NoError(t, err)
	tmpl := agentsv1alpha1.EmbeddedSandboxTemplate{
		Template: &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "checkpoint-image"}}},
		},
	}
	sbt := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(uuid.NewString())},
		Spec:       agentsv1alpha1.SandboxTemplateSpec{Template: tmpl.Template},
	}
	require.NoError(t, fc.Create(t.Context(), sbt))
	cp := &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, UID: types.UID(uuid.NewString()), CreationTimestamp: metav1.NewTime(createdAt),
			Labels:      map[string]string{agentsv1alpha1.LabelSandboxTemplate: name},
			Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: owner, agentsv1alpha1.AnnotationSandboxID: sandboxID},
		},
		Status: agentsv1alpha1.CheckpointStatus{Phase: agentsv1alpha1.CheckpointSucceeded, CheckpointId: checkpointID},
	}
	require.NoError(t, fc.Create(t.Context(), cp))
	require.NoError(t, fc.Status().Update(t.Context(), cp))
	require.Eventually(t, func() bool {
		_, err := controller.cache.GetCheckpoint(t.Context(), infracache.GetCheckpointOptions{Namespace: namespace, CheckpointID: checkpointID})
		return err == nil
	}, time.Second, 10*time.Millisecond)
	return func() {
		_ = fc.Delete(t.Context(), cp)
		_ = fc.Delete(t.Context(), sbt)
	}
}

func getTestCRClient(controller *Controller) ctrlclient.Client {
	return controller.manager.GetInfra().GetCache().GetClient()
}

func GetSandbox(t *testing.T, sandboxID string, c ctrlclient.Client) *agentsv1alpha1.Sandbox {
	split := strings.Split(sandboxID, "--")
	namespace, name := split[0], split[1]
	sbx := &agentsv1alpha1.Sandbox{}
	err := c.Get(t.Context(), ctrlclient.ObjectKey{Namespace: namespace, Name: name}, sbx)
	require.NoError(t, err)
	return sbx
}

func EnableWaitSim(t *testing.T, controller *Controller, sandboxID string) {
	mgr := controller.manager.GetInfra().GetCache().(*infracache.Cache).GetMockManager()
	mgr.AddWaitReconcileKey(GetSandbox(t, sandboxID, mgr.GetClient()))
}

type DoFunc func(t *testing.T, c ctrlclient.Client, sbx *agentsv1alpha1.Sandbox)
type WhenFunc func(sbx *agentsv1alpha1.Sandbox) bool

func Immediately(sbx *agentsv1alpha1.Sandbox) bool { return sbx != nil }

func UpdateSandboxWhen(t *testing.T, c ctrlclient.Client, sandboxID string, when WhenFunc, do DoFunc) {
	require.NotNil(t, do)
	var sbx *agentsv1alpha1.Sandbox
	if !assert.Eventually(t, func() bool {
		// Do not use GetSandbox here because it calls require.NoError,
		// which panics when called from a goroutine after the parent
		// test has completed (assert.Eventually runs this callback in
		// a separate goroutine). Instead, handle the Get error
		// gracefully so Eventually can retry or time out.
		split := strings.Split(sandboxID, "--")
		namespace, name := split[0], split[1]
		sbx = &agentsv1alpha1.Sandbox{}
		if err := c.Get(t.Context(), ctrlclient.ObjectKey{Namespace: namespace, Name: name}, sbx); err != nil {
			sbx = nil
			return false
		}
		return when(sbx)
	}, 5*time.Second, 100*time.Millisecond) {
		return
	}
	if sbx != nil {
		do(t, c, sbx.DeepCopy())
	}
}

func DoSetSandboxStatus(phase agentsv1alpha1.SandboxPhase, pausedStatus, readyStatus metav1.ConditionStatus) DoFunc {
	return func(t *testing.T, c ctrlclient.Client, sbx *agentsv1alpha1.Sandbox) {
		sbx.Status.Phase = phase
		sbx.Status.Conditions = nil
		if pausedStatus != "" {
			sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
				Type: string(agentsv1alpha1.SandboxConditionPaused), Status: pausedStatus,
			})
		}
		if readyStatus != "" {
			sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
				Type: string(agentsv1alpha1.SandboxConditionReady), Status: readyStatus,
			})
		}
		err := c.Status().Update(t.Context(), sbx)
		if err != nil {
			log.Printf("failed to update sandbox status: %v", err)
		}
	}
}

func AssertEndAt(t *testing.T, expect time.Time, endAt string) {
	endAtTime, err := time.Parse(time.RFC3339, endAt)
	assert.NoError(t, err)
	assert.WithinDuration(t, expect, endAtTime, 5*time.Second)
}
