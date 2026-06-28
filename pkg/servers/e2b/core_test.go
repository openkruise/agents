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
	"net/url"
	"os"
	"os/signal"
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
	toolscache "k8s.io/client-go/tools/cache"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
	"github.com/openkruise/agents/pkg/proxy"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/sandbox-manager/quota"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/utils/testutils"
)

var TestServerPort = 9999
var Namespace = models.AdminTeamName
var InitKey = "admin-987654321"

func CreateSandboxWithStatus(t *testing.T, c ctrlclient.Client, sbx *agentsv1alpha1.Sandbox) {
	t.Helper()
	ctx := t.Context()
	err := c.Create(ctx, sbx)
	assert.NoError(t, err)
	err = c.Status().Update(ctx, sbx)
	assert.NoError(t, err)
}

func Setup(t *testing.T) (*Controller, ctrlclient.Client, func()) {
	return SetupWithMinResumeTimeout(t, models.DefaultMinResumeTimeoutSeconds)
}

func refreshKeyStorageForTest(t *testing.T, controller *Controller) {
	t.Helper()
	require.NoError(t, controller.keys.Init(t.Context()))
}

func SetupWithMinResumeTimeout(t *testing.T, minResumeTimeout int) (*Controller, ctrlclient.Client, func()) {
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
	controller := NewController("example.com", namespace, "component=sandbox-manager", "", "", models.DefaultMaxTimeout, minResumeTimeout, 10,
		0, 0, TestServerPort, config.DefaultMemberlistBindPort, &keys.Config{
			Mode:      keys.StorageModeSecret,
			Namespace: namespace,
			AdminKey:  InitKey,
			Client:    fc,
			APIReader: fc,
		}, nil, config.QuotaOptions{})

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
		WithProxy(proxyServer).
		Build()
	require.NoError(t, infraInstance.Run(t.Context()))

	sandboxManager, err := sandboxmanager.NewSandboxManagerBuilder(opts).
		WithRequestAdapter(adapters.DefaultAdapterFactory(controller.port)).
		WithCustomInfra(func() (infra.Builder, error) {
			return sandboxcr.NewInfraBuilder(opts).
				WithCache(cache).
				WithAPIReader(fc).
				WithProxy(proxyServer), nil
		}).
		Build()
	require.NoError(t, err)

	controller.cache = cache
	controller.manager = sandboxManager
	controller.storageRegistry = storages.NewStorageProvider()
	controller.registerRoutes()

	require.NoError(t, controller.initKeyStorage(t.Context()))

	// Initialize quota through the manager (mirrors Init() logic)
	if controller.keys != nil {
		require.NoError(t, controller.manager.InitQuota(t.Context(), config.QuotaOptions{}, keys.NewQuotaSubjectLister(controller.keys)))
	} else {
		require.NoError(t, controller.manager.InitQuota(t.Context(), config.QuotaOptions{}, nil))
	}

	controller.stop = make(chan os.Signal, 1)
	signal.Notify(controller.stop, syscall.SIGINT, syscall.SIGTERM)
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
		signal.Stop(controller.stop)
		_ = controller.server.Close()
		require.NoError(t, <-serverErr)
	}
}

type quotaInitRegistration struct{}

func (quotaInitRegistration) HasSynced() bool { return true }
func (quotaInitRegistration) Remove() error   { return nil }

type quotaInitCache struct {
	addCalls    atomic.Int64
	lastHandler toolscache.ResourceEventHandler
	addErr      error
}

func (c *quotaInitCache) GetClaimedSandbox(context.Context, infracache.GetClaimedSandboxOptions) (*agentsv1alpha1.Sandbox, error) {
	return nil, nil
}
func (c *quotaInitCache) GetCheckpoint(context.Context, infracache.GetCheckpointOptions) (*agentsv1alpha1.Checkpoint, error) {
	return nil, nil
}
func (c *quotaInitCache) PickSandboxSet(context.Context, infracache.PickSandboxSetOptions) (*agentsv1alpha1.SandboxSet, error) {
	return nil, nil
}
func (c *quotaInitCache) ListSandboxSets(context.Context, infracache.ListSandboxSetsOptions) ([]*agentsv1alpha1.SandboxSet, error) {
	return nil, nil
}
func (c *quotaInitCache) ListSandboxes(context.Context, infracache.ListSandboxesOptions) ([]*agentsv1alpha1.Sandbox, error) {
	return nil, nil
}
func (c *quotaInitCache) CountActiveSandboxes(context.Context, infracache.ListSandboxesOptions) (int32, error) {
	return 0, nil
}
func (c *quotaInitCache) ListLiveSandboxesByOwner(context.Context, string) ([]*agentsv1alpha1.Sandbox, error) {
	return nil, nil
}
func (c *quotaInitCache) ListCheckpoints(context.Context, infracache.ListCheckpointsOptions) ([]*agentsv1alpha1.Checkpoint, error) {
	return nil, nil
}
func (c *quotaInitCache) ListSandboxesInPool(context.Context, infracache.ListSandboxesInPoolOptions) ([]*agentsv1alpha1.Sandbox, error) {
	return nil, nil
}
func (c *quotaInitCache) NewSandboxPauseTask(context.Context, *agentsv1alpha1.Sandbox) (*cacheutils.WaitTask[*agentsv1alpha1.Sandbox], error) {
	return nil, nil
}
func (c *quotaInitCache) NewSandboxResumeTask(context.Context, *agentsv1alpha1.Sandbox) (*cacheutils.WaitTask[*agentsv1alpha1.Sandbox], error) {
	return nil, nil
}
func (c *quotaInitCache) NewSandboxWaitReadyTask(context.Context, *agentsv1alpha1.Sandbox) *cacheutils.WaitTask[*agentsv1alpha1.Sandbox] {
	return nil
}
func (c *quotaInitCache) NewCheckpointTask(context.Context, *agentsv1alpha1.Checkpoint) *cacheutils.WaitTask[*agentsv1alpha1.Checkpoint] {
	return nil
}
func (c *quotaInitCache) AddSandboxEventHandler(_ context.Context, handler toolscache.ResourceEventHandler) (infracache.SandboxEventHandlerRegistration, error) {
	c.addCalls.Add(1)
	if c.addErr != nil {
		return nil, c.addErr
	}
	c.lastHandler = handler
	return quotaInitRegistration{}, nil
}
func (c *quotaInitCache) SandboxInformerHealthy() bool    { return true }
func (c *quotaInitCache) Run(context.Context) error       { return nil }
func (c *quotaInitCache) Stop(context.Context)            {}
func (c *quotaInitCache) GetClient() ctrlclient.Client    { return nil }
func (c *quotaInitCache) GetAPIReader() ctrlclient.Reader { return nil }
func (c *quotaInitCache) GetCache() ctrlcache.Cache       { return nil }

// quotaInitSubjectLister is a minimal SubjectLister for InitQuota tests.
type quotaInitSubjectLister struct{}

func (*quotaInitSubjectLister) ListLimited(context.Context) ([]quota.Subject, error) { return nil, nil }
func (*quotaInitSubjectLister) Load(context.Context, string) (quota.Subject, bool) {
	return quota.Subject{}, false
}

// buildQuotaTestManager creates a SandboxManager backed by the given cache for InitQuota tests.
func buildQuotaTestManager(t *testing.T, spyCache *quotaInitCache) *sandboxmanager.SandboxManager {
	t.Helper()
	opts := config.InitOptions(config.SandboxManagerOptions{
		SystemNamespace:            "sandbox-system",
		MemberlistBindPort:         config.DefaultMemberlistBindPort,
		DisableRouteReconciliation: true,
	})
	proxyServer := proxy.NewServer(opts)
	mgr, err := sandboxmanager.NewSandboxManagerBuilder(opts).
		WithCustomInfra(func() (infra.Builder, error) {
			return sandboxcr.NewInfraBuilder(opts).
				WithCache(spyCache).
				WithAPIReader(nil).
				WithProxy(proxyServer), nil
		}).
		Build()
	require.NoError(t, err)
	return mgr
}

func TestManagerInitQuotaNoKeysDoesNotRegisterAntiDrift(t *testing.T) {
	spyCache := &quotaInitCache{}
	mgr := buildQuotaTestManager(t, spyCache)

	require.NoError(t, mgr.InitQuota(context.Background(), config.QuotaOptions{}, nil))
	assert.Nil(t, mgr.GetQuotaAntiDrift())
	assert.Equal(t, int64(0), spyCache.addCalls.Load())
	assert.Nil(t, spyCache.lastHandler)
}

func TestManagerInitQuotaRedisAbsentDoesNotRegisterAntiDrift(t *testing.T) {
	spyCache := &quotaInitCache{}
	mgr := buildQuotaTestManager(t, spyCache)

	require.NoError(t, mgr.InitQuota(context.Background(), config.QuotaOptions{}, &quotaInitSubjectLister{}))
	assert.Nil(t, mgr.GetQuotaAntiDrift())
	assert.Equal(t, int64(0), spyCache.addCalls.Load())
	assert.Nil(t, spyCache.lastHandler)
}

func TestManagerInitQuotaRedisConfiguredRegistersHandler(t *testing.T) {
	spyCache := &quotaInitCache{}
	mgr := buildQuotaTestManager(t, spyCache)

	require.NoError(t, mgr.InitQuota(context.Background(), config.QuotaOptions{
		RedisAddr:         "127.0.0.1:1",
		OperationTimeout:  time.Millisecond,
		AntiDriftInterval: time.Minute,
		AntiDriftGrace:    time.Minute,
	}, &quotaInitSubjectLister{}))
	require.NotNil(t, mgr.GetQuotaAntiDrift())
	assert.Equal(t, int64(1), spyCache.addCalls.Load(), "Redis configured must register the anti-drift event handler")
	assert.NotNil(t, spyCache.lastHandler)
}

func TestManagerInitQuotaRedisConfiguredTransportUnavailableStillFailOpen(t *testing.T) {
	spyCache := &quotaInitCache{}
	mgr := buildQuotaTestManager(t, spyCache)

	require.NoError(t, mgr.InitQuota(context.Background(), config.QuotaOptions{
		RedisAddr:         "127.0.0.1:1",
		OperationTimeout:  time.Millisecond,
		AntiDriftInterval: time.Minute,
		AntiDriftGrace:    time.Minute,
	}, &quotaInitSubjectLister{}))
	require.NotNil(t, mgr.GetQuotaAntiDrift(), "transport unavailability must not disable driver construction when Redis is configured")

	// Verify the hot path fails open: Acquire must succeed even with an unreachable Redis.
	err := mgr.GetQuotaEnforcer().Acquire(context.Background(), quota.AcquireRequest{
		User:       "test-user",
		LockString: "test-lock",
		Quota:      &quota.QuotaSpec{Limits: []quota.QuotaLimit{{Dimension: quota.DimSandboxCount, Scope: quota.ScopeAll, Limit: 1}}},
		Footprint:  map[quota.QuotaDimension]int64{quota.DimSandboxCount: 1},
		Scopes:     []quota.QuotaScope{quota.ScopeAll},
	})
	require.NoError(t, err, "configured Redis transport errors must fail open on the hot path")
}

func TestManagerInitQuotaRedisConfiguredRequiresCache(t *testing.T) {
	mgr := &sandboxmanager.SandboxManager{}
	err := mgr.InitQuota(context.Background(), config.QuotaOptions{
		RedisAddr:         "127.0.0.1:1",
		OperationTimeout:  time.Millisecond,
		AntiDriftInterval: time.Minute,
		AntiDriftGrace:    time.Minute,
	}, &quotaInitSubjectLister{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cache is not available")
}

func TestManagerInitQuotaRedisConfiguredRegistrationErrorDoesNotLeavePartialState(t *testing.T) {
	spyCache := &quotaInitCache{addErr: errors.New("informer unavailable")}
	mgr := buildQuotaTestManager(t, spyCache)

	err := mgr.InitQuota(context.Background(), config.QuotaOptions{
		RedisAddr:         "127.0.0.1:1",
		OperationTimeout:  time.Millisecond,
		AntiDriftInterval: time.Minute,
		AntiDriftGrace:    time.Minute,
	}, &quotaInitSubjectLister{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "informer unavailable")
	assert.Equal(t, int64(1), spyCache.addCalls.Load())
	assert.Nil(t, mgr.GetQuotaAntiDrift())
	assert.Nil(t, mgr.GetQuotaRedisClient())
}

type recordingRedisCloser struct {
	closed atomic.Bool
}

func (c *recordingRedisCloser) Close() error {
	c.closed.Store(true)
	return nil
}

func TestManagerStopClosesQuotaRedis(t *testing.T) {
	closer := &recordingRedisCloser{}
	opts := config.InitOptions(config.SandboxManagerOptions{
		SystemNamespace:    "sandbox-system",
		MemberlistBindPort: config.DefaultMemberlistBindPort,
	})
	cache, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)

	proxyServer := proxy.NewServer(opts)
	mgr, err := sandboxmanager.NewSandboxManagerBuilder(opts).
		WithCustomInfra(func() (infra.Builder, error) {
			return sandboxcr.NewInfraBuilder(opts).
				WithCache(cache).
				WithAPIReader(fc).
				WithProxy(proxyServer), nil
		}).
		Build()
	require.NoError(t, err)

	require.NoError(t, mgr.GetInfra().Run(t.Context()))
	mgr.SetQuotaRedisClient(closer)

	mgr.Stop(t.Context())
	assert.True(t, closer.closed.Load())
}

func TestControllerShutdownClosesQuotaRedisAfterHTTPShutdown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(requestStarted)
			<-releaseRequest
			_, _ = w.Write([]byte("ok"))
		}),
	}
	shutdownStarted := make(chan struct{})
	server.RegisterOnShutdown(func() {
		close(shutdownStarted)
	})
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Serve(listener)
	}()

	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	defer client.CloseIdleConnections()
	clientErr := make(chan error, 1)
	go func() {
		resp, err := client.Get("http://" + listener.Addr().String())
		if err != nil {
			clientErr <- err
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		clientErr <- resp.Body.Close()
	}()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for request to start")
	}

	closer := &recordingRedisCloser{}
	cancelCalled := atomic.Bool{}

	opts := config.InitOptions(config.SandboxManagerOptions{
		SystemNamespace:    "sandbox-system",
		MemberlistBindPort: config.DefaultMemberlistBindPort,
	})
	fakeCache, fc, cacheErr := cachetest.NewTestCache(t)
	require.NoError(t, cacheErr)
	proxyServer := proxy.NewServer(opts)
	mgr, err := sandboxmanager.NewSandboxManagerBuilder(opts).
		WithCustomInfra(func() (infra.Builder, error) {
			return sandboxcr.NewInfraBuilder(opts).
				WithCache(fakeCache).
				WithAPIReader(fc).
				WithProxy(proxyServer), nil
		}).
		Build()
	require.NoError(t, err)
	require.NoError(t, mgr.GetInfra().Run(t.Context()))
	mgr.SetQuotaRedisClient(closer)

	shutdownCtx, shutdownCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer shutdownCancel()
	shutdownDone := make(chan struct{})
	sc := &Controller{
		server:  server,
		manager: mgr,
	}
	go func() {
		sc.shutdown(shutdownCtx, func() {
			cancelCalled.Store(true)
		})
		close(shutdownDone)
	}()

	select {
	case <-shutdownStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for HTTP shutdown to start")
	}
	assert.False(t, closer.closed.Load(), "quota Redis client must stay open while HTTP requests are draining")
	select {
	case <-shutdownDone:
		t.Fatal("shutdown completed before the active request drained")
	default:
	}

	close(releaseRequest)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for shutdown to finish")
	}
	require.NoError(t, <-clientErr)
	require.ErrorIs(t, <-serverErr, http.ErrServerClosed)
	assert.True(t, closer.closed.Load())
	assert.True(t, cancelCalled.Load())
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
	return req.WithContext(context.WithValue(req.Context(), "user", user))
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
		if options.CPURequest != "" {
			container.Resources.Requests[corev1.ResourceCPU] = resource.MustParse(options.CPURequest)
		}
		if options.Memory != "" {
			container.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(options.Memory)
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
