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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agents "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	serving "github.com/openkruise/agents/pkg/sandbox-gateway/sessionservice"
	"github.com/openkruise/agents/pkg/sandboxroute"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
	"github.com/openkruise/agents/pkg/sessionservice/kubernetes"
)

func newTestSessionServiceFilter(cfg *Config) (*sessionServiceFilter, *mockFilterCallbackHandler) {
	callbacks := newMockFilterCallbackHandler()
	return &sessionServiceFilter{callbacks: callbacks, config: cfg, authority: "49983-demo.test.svc.example.com"}, callbacks
}

type servingResponseHeader struct{ mockRequestHeaderMap }

func (h *servingResponseHeader) Status() (int, bool) { return 500, true }

func localSessionServiceFixture(t *testing.T, memberJWT bool, hooks interceptor.Funcs, sessionIDs ...string) *serving.Service {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agents.AddToScheme(scheme))
	parent := &agents.SessionService{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "demo", UID: "serving-uid"}, Spec: agents.SessionServiceSpec{Ports: []int32{8080, 49983}}}
	box := &agents.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "sandbox-a", UID: "sandbox-uid", Labels: map[string]string{sessionbinding.SessionServiceUIDLabel: "serving-uid"}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(parent, agents.GroupVersion.WithKind("SessionService"))}}, Status: agents.SandboxStatus{Phase: agents.SandboxRunning, PodInfo: agents.PodInfo{PodIP: "::1"}, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}}}
	if memberJWT {
		box.Annotations = map[string]string{identity.AnnotationEnableJwtAuth: agents.True}
	}
	records := sessionbinding.Bindings{}
	for _, id := range sessionIDs {
		records[id] = sessionbinding.Record{CreatedAt: "2026-11-10T00:00:00Z"}
	}
	var err error
	box.Annotations, err = sessionbinding.Encode(box.Annotations, records)
	require.NoError(t, err)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(parent, box).WithInterceptorFuncs(hooks).Build()
	service := serving.NewService()
	require.NoError(t, service.Configure(kubernetes.NewStore(c, func() bool { return true })))
	return service
}

// repeatedSessionHeader models separate header lines, which a map cannot retain.
type repeatedSessionHeader struct{ api.RequestHeaderMap }

func (h repeatedSessionHeader) Range(fn func(string, string) bool) {
	h.RequestHeaderMap.Range(fn)
	fn(serving.SessionHeader, "Ab_9012345678901234567")
}

func TestSessionServiceAsyncRouteAndResponse(t *testing.T) {
	const known = "Ab_9012345678901234567"
	for _, memberJWT := range []bool{false, true} {
		for _, tt := range []struct {
			name, supplied           string
			present, seed, duplicate bool
			status                   int
		}{
			{name: "absent creates"},
			{name: "empty creates", present: true},
			{name: "known", supplied: known, present: true, seed: true},
			{name: "unknown", supplied: known, present: true, status: 404},
			{name: "short", supplied: "Ab_9", present: true, status: 400},
			{name: "whitespace", supplied: " " + known, present: true, status: 400},
			{name: "duplicate", supplied: known, present: true, duplicate: true, status: 400},
		} {
			t.Run(fmt.Sprintf("%t/%s", memberJWT, tt.name), func(t *testing.T) {
				reg := useTestRegistry(t)
				putTestRoute(t, reg, "test--sandbox-a", sandboxroute.Route{UID: "sandbox-uid", Namespace: "test", Name: "sandbox-a", IP: "::1", State: "running", ResourceVersion: "1", AccessToken: "runtime-secret", RequireTrafficAuth: memberJWT})
				cfg := DefaultConfig()
				cfg.EnableRuntimeMTLS = true
				cfg.EnableAuth = true
				cfg.EnableJWTAuth = memberJWT
				cb := newMockFilterCallbackHandler()
				filterConfig := NewFilterConfig(cfg)
				patches := 0
				var persisted sessionbinding.Bindings
				hooks := interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					if err := c.Patch(ctx, obj, patch, opts...); err != nil {
						return err
					}
					var current agents.Sandbox
					if err := c.Get(ctx, client.ObjectKeyFromObject(obj), &current); err != nil {
						return err
					}
					var err error
					persisted, err = sessionbinding.Decode(current.Annotations)
					return err
				}}
				var seed []string
				if tt.seed {
					seed = []string{known}
				}
				filterConfig.servingService = localSessionServiceFixture(t, memberJWT, hooks, seed...)
				filter := FilterFactory(filterConfig, cb)
				cb.decoderCallbacks.done = make(chan struct{}, 1)
				header := newHostHeader("49983-demo.test.svc.example.com")
				header.Set(accessTokenHeader, "wrong-caller-token")
				// No SessionService traffic token is required.
				if tt.present {
					header.Set(serving.SessionHeader, tt.supplied)
				}
				var request api.RequestHeaderMap = header
				if tt.duplicate {
					request = repeatedSessionHeader{header}
				}
				outcome := filter.DecodeHeaders(request, true)
				f := filter.(*routingFilter).StreamFilter.(*sessionServiceFilter)
				if tt.status == 400 {
					require.Equal(t, api.LocalReply, outcome)
				} else {
					require.Equal(t, api.Running, outcome)
				}
				select {
				case <-cb.decoderCallbacks.done:
				case <-time.After(3 * time.Second):
					t.Fatal("no async completion")
				}
				if tt.status != 0 {
					require.Equal(t, tt.status, cb.decoderCallbacks.replyStatusCode)
					require.False(t, cb.decoderCallbacks.continueCalled)
					require.Zero(t, patches)
					require.Empty(t, f.servingSession)
					require.Equal(t, []string{"no-store"}, cb.decoderCallbacks.replyHeaders["cache-control"])
					require.NotContains(t, cb.decoderCallbacks.replyHeaders, serving.SessionHeader)
					return
				}
				require.True(t, cb.decoderCallbacks.continueCalled)
				require.False(t, cb.decoderCallbacks.sendLocalReplyCalled)
				assert.Equal(t, "[::1]:49983", cb.streamInfo.dynamicMetadata.data["envoy.lb.original_dst"]["host"])
				assert.Equal(t, true, cb.streamInfo.dynamicMetadata.data[runtimeMTLSMetadataNamespace][runtimeMTLSMetadataKey])
				assert.Equal(t, 1, cb.clearRouteCalls)
				token, _ := header.Get(accessTokenHeader)
				assert.Equal(t, "runtime-secret", token)
				confirmed, _ := header.Get(serving.SessionHeader)
				require.True(t, sessionbinding.ValidSessionID(confirmed))
				if tt.seed {
					assert.Equal(t, known, confirmed)
					assert.Zero(t, patches)
				} else {
					assert.Equal(t, 1, patches)
					assert.Contains(t, persisted, confirmed)
				}
				assert.Equal(t, confirmed, f.servingSession)
				response := &servingResponseHeader{*newMockRequestHeaderMap()}
				response.Set(serving.SessionHeader, "workload-value")
				response.Set("cache-control", "public, max-age=600")
				require.Equal(t, api.Continue, filter.EncodeHeaders(response, true))
				value, _ := response.Get(serving.SessionHeader)
				assert.Equal(t, confirmed, value)
				cacheControl, _ := response.Get("cache-control")
				assert.Equal(t, "no-store", cacheControl)
				assert.NotContains(t, response.GetAllHeaders(), DefaultTrafficAccessTokenHeader)
			})
		}
	}
}

func TestSessionServiceAsyncDestroyCancels(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	service := localSessionServiceFixture(t, false, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*agents.SandboxList); ok {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return ctx.Err()
		}
		return c.List(ctx, list, opts...)
	}})
	f, cb := newTestSessionServiceFilter(DefaultConfig())
	f.servingService = service
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.cancel = cancel
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.servingAndContinue(ctx, newMockRequestHeaderMap(), "test", "demo", "", 8080)
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

func TestSessionServiceDestroyedBeforeMetadata(t *testing.T) {
	for _, token := range []string{"runtime-secret", ""} {
		t.Run(fmt.Sprintf("empty credential %t", token == ""), func(t *testing.T) {
			reg := useTestRegistry(t)
			route := sandboxroute.Route{ID: "sandbox-a", UID: "sandbox-uid", Namespace: "test", Name: "sandbox-a", IP: "127.0.0.1", State: "running", ResourceVersion: "1", AccessToken: token}
			putTestRoute(t, reg, route.ID, route)
			f, cb := newTestSessionServiceFilter(DefaultConfig())
			header := newMockRequestHeaderMap()
			header.Set(accessTokenHeader, "caller-token")
			prepare, ok := f.prepareSessionServiceRuntime(header, route, 49983)
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

func TestSessionServiceCompletionRace(t *testing.T) {
	for range 30 {
		f, cb := newTestSessionServiceFilter(DefaultConfig())
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

// Unauthenticated SessionService routing still supplies the member runtime credential.
func TestSessionServiceRuntimeCredentials(t *testing.T) {
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
			f, _ := newTestSessionServiceFilter(cfg)
			header := newMockRequestHeaderMap()
			header.Set(accessTokenHeader, "caller-business-token")
			prepare, ok := f.prepareSessionServiceRuntime(header, route, tt.port)
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

func TestMalformedSessionServiceAuthorityCannotFallThrough(t *testing.T) {
	for _, host := range []string{"8080-demo..svc.example.com", "8080-demo.test.svc..com", "no-port.test.svc.example.com", "8080-demo_bad.test.svc.example.com"} {
		t.Run(host, func(t *testing.T) {
			activateTestRegistry(useTestRegistry(t))
			cfg := DefaultConfig()
			cfg.EnableAuth = false
			cfg.EnableJWTAuth = false
			f, cb := newTestSessionServiceFilter(cfg)
			f.authority = host
			h := newHostHeader(host)
			h.Set(serving.SessionHeader, "session")
			require.Equal(t, api.LocalReply, f.DecodeHeaders(h, true))
			require.Equal(t, 400, cb.decoderCallbacks.replyStatusCode)
		})
	}
}
