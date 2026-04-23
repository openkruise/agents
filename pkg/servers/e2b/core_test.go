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
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
)

var TestServerPort = 9999
var Namespace = "default"
var InitKey = "admin-987654321"

func CreateSandboxWithStatus(t *testing.T, c ctrlclient.Client, sbx *agentsv1alpha1.Sandbox) {
	t.Helper()
	ctx := context.Background()
	err := c.Create(ctx, sbx)
	assert.NoError(t, err)
	err = c.Status().Update(ctx, sbx)
	assert.NoError(t, err)
}

func Setup(t *testing.T) (*Controller, ctrlclient.Client, func()) {
	utils.InitLogOutput()
	namespace := "sandbox-system"

	// Build infra using the builder pattern (avoids connecting to a real API server).
	// InitOptions populates defaults (e.g. MaxCreateQPS) that the infra rate limiter
	// relies on — omitting this previously produced "limiter's burst 0" errors.
	opts := config.InitOptions(config.SandboxManagerOptions{
		SystemNamespace:    namespace,
		MaxClaimWorkers:    10,
		MemberlistBindPort: config.DefaultMemberlistBindPort,
	})
	cache, fc, cacheErr := cachetest.NewTestCache(t)
	require.NoError(t, cacheErr)
	controller := NewController("example.com", namespace, "component=sandbox-manager", "", "", models.DefaultMaxTimeout, 10,
		0, 0, TestServerPort, config.DefaultMemberlistBindPort, &keys.Config{
			Mode:      keys.StorageModeSecret,
			Namespace: namespace,
			AdminKey:  InitKey,
			Client:    fc,
			APIReader: fc,
		}, nil)

	// Create test resources using the controller-runtime fake client
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-manager",
			Namespace: namespace,
			Labels: map[string]string{
				"component": "sandbox-manager",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
			PodIP: "127.0.0.1",
		},
	}
	require.NoError(t, fc.Create(ctx, pod))
	require.NoError(t, fc.Status().Update(ctx, pod))

	// create key store secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keys.KeySecretName,
			Namespace: namespace,
		},
		Data: map[string][]byte{},
	}
	require.NoError(t, fc.Create(ctx, secret))

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

	require.NoError(t, controller.initKeyStorage(ctx))

	// Start HTTP server and stop channel directly (skip controller.Run which
	// would call manager.Run and try to start memberlist/peersManager).
	controller.stop = make(chan os.Signal, 1)
	signal.Notify(controller.stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		if err := controller.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("HTTP server error: %v", err)
		}
	}()

	return controller, fc, func() {
		controller.stop <- syscall.SIGTERM
		_ = controller.server.Close()
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
	return req.WithContext(context.WithValue(req.Context(), "user", user))
}

func GetSbsOwnerReference(sbs *agentsv1alpha1.SandboxSet) []metav1.OwnerReference {
	return []metav1.OwnerReference{*metav1.NewControllerRef(sbs, agentsv1alpha1.SandboxSetControllerKind)}
}

// CreateSandboxPoolOptions contains options for creating a sandbox pool
type CreateSandboxPoolOptions struct {
	Namespace   string
	RuntimeURL  string
	AccessToken string
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
	tmpl := agentsv1alpha1.EmbeddedSandboxTemplate{
		Template: &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "old-image",
					},
				},
			},
		},
	}
	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       types.UID(uuid.NewString()),
		},
		Spec: agentsv1alpha1.SandboxSetSpec{
			EmbeddedSandboxTemplate: tmpl,
		},
	}
	// Use the controller-runtime client (CacheV2's fake client) for all CRD operations
	fc := getTestCRClient(controller)
	err := fc.Create(t.Context(), sbs)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
	// MockManager doesn't run reconcilers, so register template directly
	infraImpl, _ := controller.manager.GetInfra().(*sandboxcr.Infra)
	infraImpl.RegisterTemplate(name)
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
				Name:      fmt.Sprintf("%s-%d", name, i),
				Namespace: ns,
				Labels: map[string]string{
					agentsv1alpha1.LabelSandboxTemplate:  name,
					agentsv1alpha1.LabelSandboxIsClaimed: "false",
				},
				Annotations:       annotations,
				OwnerReferences:   GetSbsOwnerReference(sbs),
				UID:               types.UID(uuid.NewString()),
				CreationTimestamp: now,
			},
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: tmpl,
			},
			Status: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
				Conditions: []metav1.Condition{
					{
						Type:   string(agentsv1alpha1.SandboxConditionReady),
						Status: metav1.ConditionTrue,
					},
				},
				PodInfo: agentsv1alpha1.PodInfo{
					PodIP: "1.2.3.4",
				},
			},
		}
		CreateSandboxWithStatus(t, fc, sbx)
	}
	require.Eventually(t, func() bool {
		pool, _ := controller.cache.ListSandboxesInPool(name)
		return len(pool) == available
	}, time.Minute, 100*time.Millisecond)
	return func() {
		for i := 0; i < available; i++ {
			sbx := &agentsv1alpha1.Sandbox{}
			sbx.Name = fmt.Sprintf("%s-%d", name, i)
			sbx.Namespace = ns
			assert.NoError(t, fc.Delete(context.Background(), sbx))
		}
		sbs.Namespace = ns
		assert.NoError(t, fc.Delete(context.Background(), sbs))
	}
}

// getTestCRClient retrieves the controller-runtime client from the infra.
// This is the CacheV2's fake client used in tests.
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

func Immediately(sbx *agentsv1alpha1.Sandbox) bool {
	return sbx != nil
}

func UpdateSandboxWhen(t *testing.T, c ctrlclient.Client, sandboxID string, when WhenFunc, do DoFunc) {
	require.NotNil(t, do)
	var sbx *agentsv1alpha1.Sandbox
	if !assert.Eventually(t, func() bool {
		sbx = GetSandbox(t, sandboxID, c)
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
		sbx.Status.Conditions = []metav1.Condition{
			{
				Type:   string(agentsv1alpha1.SandboxConditionPaused),
				Status: pausedStatus,
			},
			{
				Type:   string(agentsv1alpha1.SandboxConditionReady),
				Status: readyStatus,
			},
		}
		err := c.Status().Update(context.Background(), sbx)
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
