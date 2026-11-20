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
	"crypto/x509"
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
	session "github.com/openkruise/agents/pkg/sessionservice"
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
		{name: "invalid zero port", port: 0, status: 400, wantReason: "InvalidRequest"},
		{name: "invalid high port", port: 65536, status: 400, wantReason: "InvalidRequest"},
		{name: "policy retry", changeLimit: true, port: 8080, wantBinding: true},
		{name: "unknown write committed", unknownWrite: true, commitWrite: true, port: 8080, wantBinding: true},
		{name: "unknown write uncommitted", unknownWrite: true, port: 8080, status: 503},
		{name: "success", port: 8080, wantBinding: true},
		{name: "cache unavailable", port: 8080, status: 503, unavailable: true},
		{name: "identity missing", port: 8080, status: 404, missing: true},
		{name: "no visible members", port: 8080, status: 503, noMembers: true},
		{name: "parent deleting", port: 8080, status: 503, deleting: true},
		{name: "parent deleting with existing session", sessionID: "Ab_9012345678901234567", seed: true, port: 8080, status: 503, deleting: true, wantBinding: true},
		{name: "port removed after admission", port: 8080, status: 403, changePort: true},
		{name: "port removed after binding write", port: 8080, status: 503, changePortAfterWrite: true, wantBinding: true},
		{name: "port denied", port: 9090, status: 403},
		{name: "invalid member IP", port: 8080, status: 503, invalidRoute: true, wantBinding: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, agents.AddToScheme(scheme))
			parent := &agents.SessionService{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "demo", UID: "550e8400-e29b-41d4-a716-446655440000"}, Spec: agents.SessionServiceSpec{Ports: []int32{8080}}}
			box := &agents.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "member", UID: "member-uid", Labels: map[string]string{sessionbinding.SessionServiceUIDLabel: "550e8400-e29b-41d4-a716-446655440000"}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(parent, agents.GroupVersion.WithKind("SessionService"))}}, Status: agents.SandboxStatus{Phase: agents.SandboxRunning, PodInfo: agents.PodInfo{PodIP: "10.0.0.1"}, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}}}
			if tt.seed {
				var err error
				box.Annotations, err = sessionbinding.Encode(nil, sessionbinding.Bindings{tt.sessionID: {CreatedAt: "2026-11-10T00:00:00Z"}})
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
			c := fake.NewClientBuilder().WithScheme(scheme).WithIndex(&agents.SessionService{}, kubernetes.SessionServiceUIDIndex, func(obj client.Object) []string { return []string{string(obj.GetUID())} }).WithObjects(objects...).WithInterceptorFuncs(hooks).Build()
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
			route, err := service.ResolveRoute(ctx, "550e8400-e29b-41d4-a716-446655440000", tt.sessionID, tt.port)
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
			if tt.deleting {
				assert.Zero(t, memberReads, "deleting parent must reject before reading members")
				assert.Zero(t, bindingWrites)
				assert.Zero(t, generated)
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
	const uid = "550e8400-e29b-41d4-a716-446655440000"
	for _, tt := range []struct {
		host, uid        string
		port             int
		matched, invalid bool
	}{
		{"svc-8080-" + uid + ".example.com", uid, 8080, true, false},
		{"SVC-65535-" + strings.ToUpper(uid) + ".Example.COM.:443", uid, 65535, true, false},
		{"8080-" + uid + ".svc.example.com", "", 0, false, false},
		{"sandbox-gateway.sandbox-system.svc.cluster.local", "", 0, false, false},
		{"svc-no-port.example.com", "", 0, true, true},
		{"8080-sandbox.example.com", "", 0, false, false},
		{"8080-demo.team.svc.example.com", "", 0, false, false},
		{"svc-8080-demo.example.com", "", 0, true, true},
		{"svc-8080-550e8400e29b41d4a716446655440000.example.com", "", 0, true, true},
		{"svc-8080-550e8400-e29b-41d4-a716-44665544000g.example.com", "", 0, true, true},
		{"svc-0-" + uid + ".example.com", "", 0, true, true},
		{"svc-65536-" + uid + ".example.com", "", 0, true, true},
		{"svc-8080-" + uid + ".example.com:bad", "", 0, true, true},
		{"svc-8080-" + uid + ".", "", 0, true, true},
		{"svc-8080-" + uid + "..com", "", 0, true, true},
		{"svc-8080-" + uid + ".bad_domain", "", 0, true, true},
		{"svc-8080-" + uid + "." + strings.Repeat("a", 64), "", 0, true, true},
		{"svc-8080-" + uid + "." + strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 63), "", 0, true, true},
	} {
		t.Run(tt.host, func(t *testing.T) {
			got, port, matched, err := ParseAuthority(tt.host)
			assert.Equal(t, tt.matched, matched)
			assert.Equal(t, tt.invalid, err != nil)
			assert.Equal(t, tt.uid, got)
			assert.Equal(t, tt.port, port)
		})
	}
}

// Every service/port endpoint occupies one label below the shared TLS wildcard.
func TestSessionServiceWildcardCertificate(t *testing.T) {
	certificate := &x509.Certificate{DNSNames: []string{"*.example.com"}}
	for _, host := range []string{
		"svc-8080-550e8400-e29b-41d4-a716-446655440000.example.com",
		"svc-65535-550e8400-e29b-41d4-a716-446655440001.example.com",
	} {
		t.Run(host, func(t *testing.T) {
			_, _, matched, err := ParseAuthority(host)
			require.NoError(t, err)
			require.True(t, matched)
			require.NoError(t, certificate.VerifyHostname(host))
		})
	}
	require.Error(t, certificate.VerifyHostname("8080-demo.team.svc.example.com"))
}

func TestAllocationErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		reason string
	}{
		{name: "missing service", err: session.ErrSessionServiceNotFound, status: 404, reason: "SessionServiceNotFound"},
		{name: "missing session", err: session.ErrSessionNotFound, status: 404, reason: "SessionNotFound"},
		{name: "invalid session", err: session.ErrInvalidSession, status: 400, reason: "InvalidSessionID"},
		{name: "capacity", err: &session.Error{Reason: "SessionCapacityExhausted"}, status: 503, reason: "SessionCapacityExhausted"},
		{name: "binding conflict", err: &session.Error{Reason: "SessionBindingConflict", Cause: session.ErrSessionServiceDataAmbiguous}, status: 503, reason: "SessionBindingConflict"},
		{name: "bound unavailable", err: &session.Error{Reason: "BoundSandboxUnavailable"}, status: 503, reason: "BoundSandboxUnavailable"},
		{name: "migration timeout", err: &session.Error{Reason: "SessionRebinding", Cause: context.DeadlineExceeded}, status: 503, reason: "SessionRebinding"},
		{name: "allocation timeout", err: &session.Error{Reason: "SessionAllocationUnavailable", Cause: context.DeadlineExceeded}, status: 503, reason: "SessionManagerUnavailable"},
		{name: "canceled", err: &session.Error{Reason: "SessionAllocationUnavailable", Cause: context.Canceled}, status: 503, reason: "SessionManagerUnavailable"},
		{name: "policy changed after write", err: &session.Error{Reason: "SessionAllocationUnavailable", Cause: session.ErrPolicyChanged}, status: 503, reason: "SessionManagerUnavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rejection := mapAllocationError(tc.err)
			require.Equal(t, tc.status, rejection.Status)
			require.Equal(t, tc.reason, rejection.Reason)
		})
	}
}
