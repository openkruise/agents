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

package filter

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agents "github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/identity/oidc"
	"github.com/openkruise/agents/pkg/sandbox-gateway/serving"
	"github.com/openkruise/agents/pkg/sandboxroute"
	sessionbinding "github.com/openkruise/agents/pkg/servingsession/binding"
	"github.com/openkruise/agents/pkg/servingsession/kubernetes"
)

func TestServingCredentialBoundary(t *testing.T) {
	for _, tt := range []struct {
		name, token   string
		ready, custom bool
		want          int
	}{
		{name: "missing credential always rejected", ready: true, want: 401},
		{name: "no Manager rejects credential", ready: true, token: "secret-jwt", want: 503},
		{name: "not ready", token: "secret-jwt", want: 503},
		{name: "custom credential stripped", ready: true, custom: true, token: "secret-jwt", want: 503},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reg := useTestRegistry(t)
			reg.SetReady(tt.ready)
			cfg := DefaultConfig()
			if tt.custom {
				cfg.TrafficAccessTokenHeader = "X-Serving-Token"
			}
			cfg.EnableAuth = false
			cfg.EnableJWTAuth = false
			f, cb := newTestFilter(cfg)
			h := newHostHeader("8080-serving-example.svc.example.com")
			h.Set(serving.SessionHeader, "Ab_9")
			h.Set(cfg.GetTrafficAccessTokenHeader(), tt.token)
			require.Equal(t, api.LocalReply, f.DecodeHeaders(h, true))
			assert.Equal(t, tt.want, cb.decoderCallbacks.replyStatusCode)
			_, ok := h.Get(cfg.GetTrafficAccessTokenHeader())
			assert.False(t, ok)
			assert.Empty(t, cb.streamInfo.dynamicMetadata.data)
			assert.NotContains(t, cb.decoderCallbacks.replyBody, "secret-jwt")
		})
	}
}

type servingResponseHeader struct{ mockRequestHeaderMap }

func (h *servingResponseHeader) Status() (int, bool) { return 500, true }

type servingVerifier struct{}

func (servingVerifier) Verify(string) (*oidc.TrafficAccessTokenClaims, error) {
	return &oidc.TrafficAccessTokenClaims{
		Claims:   jwt.Claims{Subject: "serving:controlplane:serving-demo"},
		Metadata: map[string]string{"servingId": "serving-demo", "servingUid": "serving-uid"},
	}, nil
}

func localServingFixture(t *testing.T, memberJWT bool, hooks interceptor.Funcs) *serving.Service {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agents.AddToScheme(scheme))
	parent := &agents.SandboxServing{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "demo", UID: "serving-uid", Labels: map[string]string{sessionbinding.ServingIDLabel: "serving-demo"}}, Spec: agents.SandboxServingSpec{Ports: []int32{8080, 49983}}}
	box := &agents.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "sandbox-a", UID: "sandbox-uid", Labels: map[string]string{sessionbinding.ServingUIDLabel: "serving-uid"}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(parent, agents.GroupVersion.WithKind("SandboxServing"))}}, Status: agents.SandboxStatus{Phase: agents.SandboxRunning, PodInfo: agents.PodInfo{PodIP: "::1"}, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}}}
	if memberJWT {
		box.Annotations = map[string]string{identity.AnnotationEnableJwtAuth: agents.True}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(parent, box).WithIndex(parent, infracache.IndexServingID, func(obj client.Object) []string { return []string{obj.GetLabels()[sessionbinding.ServingIDLabel]} }).WithInterceptorFuncs(hooks).Build()
	service := serving.NewService(func() oidc.Verifier { return servingVerifier{} })
	require.NoError(t, service.Configure(kubernetes.NewStore(c, func() bool { return true }, kubernetes.Options{})))
	return service
}

func TestServingAsyncRouteAndResponse(t *testing.T) {
	for _, memberJWT := range []bool{false, true} {
		t.Run(fmt.Sprint(memberJWT), func(t *testing.T) {
			reg := useTestRegistry(t)
			putTestRoute(t, reg, "test--sandbox-a", sandboxroute.Route{UID: "sandbox-uid", Namespace: "test", Name: "sandbox-a", IP: "::1", State: "running", ResourceVersion: "1", AccessToken: "runtime-secret", RequireTrafficAuth: memberJWT})
			cfg := DefaultConfig()
			cfg.EnableRuntimeMTLS = true
			cfg.EnableAuth = true
			cfg.EnableJWTAuth = memberJWT
			f, cb := newTestFilter(cfg)
			f.servingService = localServingFixture(t, memberJWT, interceptor.Funcs{})
			cb.decoderCallbacks.done = make(chan struct{}, 1)
			f.servingSession = "Ab_9"
			header := newMockRequestHeaderMap()
			header.Set(accessTokenHeader, "wrong-caller-token")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.cancel = cancel
			go f.servingAndContinue(ctx, header, "serving-demo", "Ab_9", 49983, "secret-jwt")
			select {
			case <-cb.decoderCallbacks.done:
			case <-time.After(3 * time.Second):
				t.Fatal("no async completion")
			}
			require.True(t, cb.decoderCallbacks.continueCalled)
			assert.False(t, cb.decoderCallbacks.sendLocalReplyCalled)
			assert.Equal(t, "[::1]:49983", cb.streamInfo.dynamicMetadata.data["envoy.lb.original_dst"]["host"])
			assert.Equal(t, true, cb.streamInfo.dynamicMetadata.data[runtimeMTLSMetadataNamespace][runtimeMTLSMetadataKey])
			assert.Equal(t, 1, cb.clearRouteCalls)
			token, _ := header.Get(accessTokenHeader)
			assert.Equal(t, "runtime-secret", token)
			response := &servingResponseHeader{*newMockRequestHeaderMap()}
			response.Set(serving.SessionHeader, "workload-value")
			require.Equal(t, api.Continue, f.EncodeHeaders(response, true))
			value, _ := response.Get(serving.SessionHeader)
			assert.Equal(t, "Ab_9", value)
			assert.NotContains(t, response.GetAllHeaders(), DefaultTrafficAccessTokenHeader)
		})
	}
}

func TestServingAsyncDestroyCancels(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	service := localServingFixture(t, false, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*agents.SandboxServingList); ok {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return ctx.Err()
		}
		return c.List(ctx, list, opts...)
	}})
	f, cb := newTestFilter(DefaultConfig())
	f.servingService = service
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.cancel = cancel
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.servingAndContinue(ctx, newMockRequestHeaderMap(), "serving-demo", "Ab_9", 8080, "secret-jwt")
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("allocation did not start")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { f.OnDestroy(api.Normal) })
	}
	wg.Wait()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("allocation did not stop")
	}
	assert.Empty(t, cb.streamInfo.dynamicMetadata.data)
	assert.False(t, cb.decoderCallbacks.continueCalled)
	assert.False(t, cb.decoderCallbacks.sendLocalReplyCalled)
}

func TestServingDestroyedBeforeMetadata(t *testing.T) {
	for _, token := range []string{"runtime-secret", ""} {
		t.Run(fmt.Sprintf("empty credential %t", token == ""), func(t *testing.T) {
			reg := useTestRegistry(t)
			route := sandboxroute.Route{ID: "sandbox-a", UID: "sandbox-uid", Namespace: "test", Name: "sandbox-a", IP: "127.0.0.1", State: "running", ResourceVersion: "1", AccessToken: token}
			putTestRoute(t, reg, route.ID, route)
			f, cb := newTestFilter(DefaultConfig())
			header := newMockRequestHeaderMap()
			header.Set(accessTokenHeader, "caller-token")
			prepare, ok := f.prepareServingRuntime(header, route, 49983)
			require.True(t, ok)
			require.True(t, f.beginCompletion())
			f.OnDestroy(api.Normal)
			assert.False(t, f.setUpstreamMetadata(route, 49983, prepare))
			assert.Empty(t, cb.streamInfo.dynamicMetadata.data)
			value, present := header.Get(accessTokenHeader)
			assert.True(t, present, "no Del after OnDestroy")
			assert.Equal(t, "caller-token", value, "no Set after OnDestroy")
			assert.False(t, f.claimPreparedCompletion())
		})
	}
}

func TestServingCompletionRace(t *testing.T) {
	for range 30 {
		f, cb := newTestFilter(DefaultConfig())
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() { f.completeWithContinue(sandboxroute.Route{IP: "127.0.0.1"}, 8080, nil) })
		}
		wg.Go(func() { f.OnDestroy(api.Normal) })
		wg.Wait()
		assert.False(t, cb.decoderCallbacks.sendLocalReplyCalled)
		f.mu.Lock()
		assert.True(t, f.destroyed)
		f.mu.Unlock()
	}
}

// Trusted Serving admission is an alternative to the member's Gateway
// credentials, but agent-runtime still needs that exact member's static token.
func TestServingRuntimeCredentials(t *testing.T) {
	for _, tt := range []struct {
		name               string
		port               int
		routeChange        string
		token, wantToken   string
		ready, found, want bool
	}{
		{name: "UUID member", port: 49983, token: "runtime-secret", wantToken: "runtime-secret", ready: true, found: true, want: true},
		{name: "JWT member", port: 49983, routeChange: "jwt", token: "runtime-secret", wantToken: "runtime-secret", ready: true, found: true, want: true},
		{name: "empty token preserves no static credential policy", port: 49983, ready: true, found: true, want: true},
		{name: "missing member", port: 49983, ready: true, want: false},
		{name: "stale UID", port: 49983, routeChange: "uid", token: "other-secret", ready: true, found: true, want: false},
		{name: "wrong namespace", port: 49983, routeChange: "namespace", token: "other-secret", ready: true, found: true, want: false},
		{name: "wrong name", port: 49983, routeChange: "name", token: "other-secret", ready: true, found: true, want: false},
		{name: "registry not ready", port: 49983, token: "runtime-secret", found: true, want: false},
		{name: "business port never injects runtime secret", port: 8080, token: "runtime-secret", wantToken: "caller-business-token", ready: true, found: true, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reg := useTestRegistry(t)
			route := sandboxroute.Route{ID: "sandbox-a", UID: "sandbox-uid", Namespace: "test", Name: "sandbox-a", IP: "127.0.0.1", State: "running", ResourceVersion: "1"}
			cached := route
			cached.AccessToken = tt.token
			switch tt.routeChange {
			case "jwt":
				cached.RequireTrafficAuth = true
			case "uid":
				cached.UID = "other-uid"
			case "namespace":
				cached.Namespace = "other"
			case "name":
				cached.Name = "other"
			}
			if tt.found {
				putTestRoute(t, reg, route.ID, cached)
			}
			reg.SetReady(tt.ready)
			cfg := DefaultConfig()
			cfg.EnableAuth = true
			cfg.EnableJWTAuth = true
			f, _ := newTestFilter(cfg)
			header := newMockRequestHeaderMap()
			header.Set(accessTokenHeader, "caller-business-token")
			prepare, ok := f.prepareServingRuntime(header, route, tt.port)
			assert.Equal(t, tt.want, ok)
			if ok {
				f.completeWithContinue(route, tt.port, prepare)
			}
			value, _ := header.Get(accessTokenHeader)
			if tt.want {
				assert.Equal(t, tt.wantToken, value)
			} else {
				assert.NotEqual(t, "other-secret", value)
			}
		})
	}
}
