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

package opensandbox

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/proxy"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
)

const (
	testNamespace       = "sandbox-system"
	testDefaultTemplate = "opensandbox-default"
)

// Setup builds a Controller wired to a real SandboxManager backed by a fake
// Kubernetes client (pkg/cache/cachetest), the same fake-client test
// infrastructure pkg/servers/e2b's own test suite uses for its Infra layer.
// Authentication is left disabled (Keys: nil), matching how E2B's
// --e2b-enable-auth=false path resolves every caller to the built-in admin
// identity, since these tests exercise orchestration and protocol behavior,
// not the (already separately tested) API-key store.
func Setup(t *testing.T) (*Controller, ctrlclient.Client) {
	t.Helper()
	opts := config.InitOptions(config.SandboxManagerOptions{
		SystemNamespace:    testNamespace,
		MaxClaimWorkers:    10,
		MemberlistBindPort: config.DefaultMemberlistBindPort,
	})
	cache, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)

	proxyServer := proxy.NewServer(opts)
	infraInstance := sandboxcr.NewInfraBuilder(opts).
		WithCache(cache).
		WithAPIReader(fc).
		WithRouteReader(proxyServer).
		Build()
	require.NoError(t, infraInstance.Run(t.Context()))

	manager, err := sandboxmanager.NewSandboxManagerBuilder(opts).
		WithCustomInfra(func() (infra.Builder, error) {
			return sandboxcr.NewInfraBuilder(opts).
				WithCache(cache).
				WithAPIReader(fc).
				WithRouteReader(proxyServer), nil
		}).
		Build()
	require.NoError(t, err)
	require.NoError(t, manager.InitQuota(t.Context(), config.QuotaOptions{}, nil))

	controller := NewController(Dependencies{
		Manager:         manager,
		Cache:           cache,
		SystemNamespace: testNamespace,
		DefaultTemplate: testDefaultTemplate,
	})
	controller.RegisterRoutes(http.NewServeMux())
	return controller, fc
}

// createDefaultTemplateFixture provisions the zero-replica SandboxSet that
// image-based CreateSandbox claims from, using a directly embedded pod
// template (rather than a SandboxTemplate + TemplateRef indirection) so a
// claim-on-no-stock materializes Spec.Template directly onto the new
// Sandbox — the same construction pkg/servers/e2b's own exercised claim-pool
// test fixture (CreateSandboxPool in core_test.go) uses.
func createDefaultTemplateFixture(t *testing.T, fc ctrlclient.Client, namespace string) {
	t.Helper()
	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testDefaultTemplate,
			Namespace: namespace,
			UID:       types.UID(uuid.NewString()),
		},
		Spec: agentsv1alpha1.SandboxSetSpec{
			Replicas: 0,
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "main",
							Image: "placeholder:base",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
						}},
					},
				},
			},
		},
	}
	require.NoError(t, fc.Create(t.Context(), sbs))
}

// simulateInstantReadySandbox overrides sandboxcr.DefaultCreateSandbox for the
// duration of the calling test so a claimed-on-no-stock Sandbox reaches
// Running/Ready synchronously, standing in for the agent-sandbox-controller
// reconcile loop that would normally drive that transition against a real
// kubelet. This is the same substitution pkg/servers/e2b's create tests use,
// since these unit tests run against a fake client with no controller
// actually watching Sandbox objects.
func simulateInstantReadySandbox(t *testing.T) {
	t.Helper()
	orig := sandboxcr.DefaultCreateSandbox
	sandboxcr.DefaultCreateSandbox = func(ctx context.Context, sbx *agentsv1alpha1.Sandbox, c ctrlclient.Client) (*agentsv1alpha1.Sandbox, error) {
		// The fake client does not auto-assign a UID the way a real API
		// server does, but sandboxroute.RouteFromSandbox requires one; set it
		// explicitly before creation, the same as every pre-populated pooled
		// Sandbox fixture in pkg/servers/e2b's test suite does.
		if sbx.UID == "" {
			sbx.UID = types.UID(uuid.NewString())
		}
		created, err := orig(ctx, sbx, c)
		if err != nil {
			return nil, err
		}
		created.Status = agentsv1alpha1.SandboxStatus{
			Phase:              agentsv1alpha1.SandboxRunning,
			ObservedGeneration: created.Generation,
			Conditions: []metav1.Condition{{
				Type:   string(agentsv1alpha1.SandboxConditionReady),
				Status: metav1.ConditionTrue,
				Reason: agentsv1alpha1.SandboxReadyReasonPodReady,
			}},
			PodInfo: agentsv1alpha1.PodInfo{PodIP: "10.0.0.5"},
		}
		if err := c.Status().Update(ctx, created); err != nil {
			return nil, err
		}
		return created, nil
	}
	t.Cleanup(func() { sandboxcr.DefaultCreateSandbox = orig })
}

func doRequest(t *testing.T, mux *http.ServeMux, method, path string, header http.Header, body []byte) *http.Response {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		reader = bytes.NewReader(body)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, reader)
	for k, values := range header {
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}
	mux.ServeHTTP(rec, req)
	return rec.Result()
}
