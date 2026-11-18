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

package sessionservice

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agents "github.com/openkruise/agents/api/v1alpha1"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
	"github.com/openkruise/agents/pkg/sessionservice/kubernetes"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

func TestLocalSessionServiceAdmissionAndAllocation(t *testing.T) {
	for _, tt := range []struct {
		name                                         string
		sessionID                                    string
		seed, changeLimit, unknownWrite, commitWrite bool
		port, status                                 int
		invalidRoute, wantBinding                    bool
		unavailable, missing, deleting, changePort   bool
		changePortAfterWrite                         bool
		canceled, entropyFailure                     bool
		noMembers, duplicateBindings                 bool
		wantReason                                   string
	}{
		{name: "canceled", canceled: true, port: 8080, status: 503},
		{name: "random identity unavailable", entropyFailure: true, port: 8080, status: 503},
		{name: "duplicate bindings", sessionID: "Ab_9012345678901234567", seed: true, duplicateBindings: true, port: 8080, status: 503, wantBinding: true, wantReason: "SessionBindingConflict"},
		{name: "known", sessionID: "Ab_9012345678901234567", seed: true, port: 8080, wantBinding: true},
		{name: "unknown", sessionID: "Ab_9012345678901234567", port: 8080, status: 404},
		{name: "invalid ID", sessionID: "short", port: 8080, status: 400},
		{name: "policy retry", changeLimit: true, port: 8080, wantBinding: true},
		{name: "unknown write committed", unknownWrite: true, commitWrite: true, port: 8080, wantBinding: true},
		{name: "unknown write uncommitted", unknownWrite: true, port: 8080, status: 503},
		{name: "success", port: 8080, wantBinding: true},
		{name: "cache unavailable", port: 8080, status: 503, unavailable: true},
		{name: "identity missing", port: 8080, status: 404, missing: true},
		{name: "no visible members", port: 8080, status: 503, noMembers: true},
		{name: "parent deleting", port: 8080, status: 503, deleting: true},
		{name: "port removed after admission", port: 8080, status: 403, changePort: true},
		{name: "port removed after binding write", port: 8080, status: 503, changePortAfterWrite: true, wantBinding: true},
		{name: "port denied", port: 9090, status: 403},
		{name: "invalid member IP", port: 8080, status: 503, invalidRoute: true, wantBinding: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, agents.AddToScheme(scheme))
			parent := &agents.SessionService{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "demo", UID: "serving-uid"}, Spec: agents.SessionServiceSpec{Ports: []int32{8080}}}
			box := &agents.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "member", UID: "member-uid", Labels: map[string]string{sessionbinding.SessionServiceUIDLabel: "serving-uid"}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(parent, agents.GroupVersion.WithKind("SessionService"))}}, Status: agents.SandboxStatus{Phase: agents.SandboxRunning, PodInfo: agents.PodInfo{PodIP: "10.0.0.1"}, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}}}
			if tt.seed {
				var err error
				box.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{tt.sessionID: {LastRequestAt: "2026-11-10T00:00:00Z"}})
				require.NoError(t, err)
			}
			if tt.invalidRoute {
				box.Status.PodInfo.PodIP = "not-an-IP"
			}
			t.Cleanup(func() { expectations.ResourceVersionExpectationDelete(box) })
			objects := []client.Object{}
			if !tt.noMembers {
				objects = append(objects, box)
			}
			if !tt.missing {
				objects = append(objects, parent)
			}
			if tt.duplicateBindings {
				other := box.DeepCopy()
				other.Name = "other"
				other.UID = "other-uid"
				objects = append(objects, other)
			}
			if tt.deleting {
				now := metav1.Now()
				parent.DeletionTimestamp = &now
				parent.Finalizers = []string{"test.example/hold"}
			}
			memberReads, bindingWrites := 0, 0
			hooks := interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*agents.SandboxList); ok {
						memberReads++
					}
					return c.List(ctx, list, opts...)
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, ok := obj.(*agents.Sandbox); ok {
						bindingWrites++
					}
					if tt.unknownWrite && !tt.commitWrite {
						return context.DeadlineExceeded
					}
					if err := c.Patch(ctx, obj, patch, opts...); err != nil {
						return err
					}
					if tt.unknownWrite {
						return context.DeadlineExceeded
					}
					if tt.changePortAfterWrite {
						var updated agents.SessionService
						if err := c.Get(ctx, client.ObjectKeyFromObject(parent), &updated); err != nil {
							return err
						}
						updated.Spec.Ports = []int32{9090}
						return c.Update(ctx, &updated)
					}
					return nil
				},
			}
			if tt.changePort || tt.changeLimit {
				changed := false
				hooks.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*agents.SessionService); ok && !changed {
						changed = true
						var updated agents.SessionService
						if err := c.Get(ctx, key, &updated); err != nil {
							return err
						}
						if tt.changeLimit {
							limit := int32(10)
							updated.Spec.SessionPolicy.MaxSessionsPerSandbox = &limit
						} else {
							updated.Spec.Ports = []int32{9090}
						}
						if err := c.Update(ctx, &updated); err != nil {
							return err
						}
					}
					return c.Get(ctx, key, obj, opts...)
				}
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(hooks).Build()
			service := NewService()
			generated := 0
			service.newSessionID = func() (string, error) {
				if tt.entropyFailure {
					return "", errors.New("entropy unavailable")
				}
				generated++
				return randomSessionID()
			}
			require.NoError(t, service.Configure(kubernetes.NewStore(c, func() bool { return !tt.unavailable })))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.canceled {
				cancel()
			}
			route, err := service.ResolveRoute(ctx, "team", "demo", tt.sessionID, tt.port)
			if tt.status == 0 {
				require.NoError(t, err)
				assert.Equal(t, "member", route.Name)
				assert.Empty(t, route.AccessToken)
				assert.True(t, sessionbinding.ValidSessionID(route.SessionID))
				if tt.seed {
					assert.Equal(t, tt.sessionID, route.SessionID)
				}
			} else {
				var rejection *Error
				require.ErrorAs(t, err, &rejection)
				assert.Equal(t, tt.status, rejection.Status)
				if tt.wantReason != "" {
					assert.Equal(t, tt.wantReason, rejection.Reason)
					assert.Zero(t, bindingWrites)
				}
				assert.Empty(t, route.SessionID)
			}
			if !tt.wantBinding && !tt.unknownWrite {
				assert.Zero(t, bindingWrites, "rejected requests must not write bindings")
				if !tt.changePort && tt.name != "unknown" && !tt.unknownWrite && !tt.noMembers {
					assert.Zero(t, memberReads, "admission must reject before reading member bindings")
				}
			}
			if tt.changePortAfterWrite {
				assert.Equal(t, 1, bindingWrites, "a policy change after mutation must not restart allocation")
			}
			if tt.noMembers {
				assert.Positive(t, memberReads, "allocation must inspect visible members")
				return
			}
			var current agents.Sandbox
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(box), &current))
			bindings, err := sessionbinding.Decode(current.Annotations)
			require.NoError(t, err)
			assert.Equal(t, tt.wantBinding, len(bindings) == 1)
			if tt.status == 0 {
				assert.Contains(t, bindings, route.SessionID)
			}
			if tt.wantBinding && !tt.seed {
				assert.Equal(t, 1, generated)
				assert.Equal(t, 1, bindingWrites)
			}
			if tt.seed || tt.name == "unknown" || tt.name == "invalid ID" {
				assert.Zero(t, generated)
				assert.Zero(t, bindingWrites)
			}
			if tt.unknownWrite {
				assert.Equal(t, 1, bindingWrites)
				assert.Equal(t, 1, generated)
			}
			if !tt.wantBinding && !tt.changePort && !tt.unknownWrite {
				assert.Zero(t, generated, "rejected admission must not generate an identity")
			}
		})
	}
}

func TestParseAuthority(t *testing.T) {
	for _, tt := range []struct {
		host, name, namespace string
		port                  int
		matched, invalid      bool
	}{
		{"8080-my-service.team.svc.example.com", "my-service", "team", 8080, true, false},
		{"65535-demo.svc.svc.Example.COM.:443", "demo", "svc", 65535, true, false},
		{"8080-DEMO.TEAM.SVC.example.com", "demo", "team", 8080, true, false},
		{"8080-sandbox.example.com", "", "", 0, false, false},
		{"8080-sandboxid.svc.example.com:443", "", "", 0, false, false},
		{"8080-sandboxid.example.svc.com", "sandboxid", "example", 8080, true, false},
		{"8080-sandboxid.example.com.svc.internal", "", "", 0, false, false},
		{"8080-demo.team.svc", "", "", 0, true, true},
		{"8080-demo.team.svc:bad", "", "", 0, true, true},
		{"8080-demo.svc.example.com", "", "", 0, false, false},
		{"0-demo.team.svc.example.com", "", "", 0, true, true},
		{"65536-demo.team.svc.example.com", "", "", 0, true, true},
		{"8080-demo.team.svc.example.com:bad", "", "", 0, true, true},
		{"8080-demo.team.svc.", "", "", 0, true, true},
		{"8080-demo.bad_ns.svc.example.com", "", "", 0, true, true},
		{"8080-" + strings.Repeat("a", 57) + ".team.svc.example.com", strings.Repeat("a", 57), "team", 8080, true, false},
		{"8080-" + strings.Repeat("a", 58) + ".team.svc.example.com", "", "", 0, true, true},
		{"65535-" + strings.Repeat("a", 57) + ".team.svc.example.com", strings.Repeat("a", 57), "team", 65535, true, false},
		{"065535-" + strings.Repeat("a", 57) + ".team.svc.example.com", "", "", 0, true, true},
		{"8080-demo." + strings.Repeat("a", 64) + ".svc.example.com", "", "", 0, true, true},
		{"8080-demo.team.svc." + strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 63), "", "", 0, true, true},
	} {
		t.Run(tt.host, func(t *testing.T) {
			name, namespace, port, matched, err := ParseAuthority(tt.host)
			assert.Equal(t, tt.matched, matched)
			assert.Equal(t, tt.invalid, err != nil)
			assert.Equal(t, tt.name, name)
			assert.Equal(t, tt.namespace, namespace)
			assert.Equal(t, tt.port, port)
		})
	}
}
