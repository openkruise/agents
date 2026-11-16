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
	"testing"

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
	"github.com/openkruise/agents/pkg/identity/oidc"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
	"github.com/openkruise/agents/pkg/sessionservice/kubernetes"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

func TestParseAuthority(t *testing.T) {
	for _, tt := range []struct {
		host, id       string
		port           int
		match, invalid bool
	}{
		{"8080-abcde234abcde.svc.example.com", "abcde234abcde", 8080, true, false},
		{"8080-abcde234abcde.svc.Example.COM", "abcde234abcde", 8080, true, false},
		{"8080-abcde234abcde.svc.example.com.:443", "abcde234abcde", 8080, true, false},
		{"49983-abcde234abcde.svc.example.com:443", "abcde234abcde", 49983, true, false},
		{"0-abcde234abcde.svc.example.com", "", 0, true, true},
		{"65536-abcde234abcde.svc.example.com", "", 0, true, true},
		{"abc-abcde234abcde.svc.example.com", "", 0, true, true},
		{"8080-serving-demo.svc.example.com", "", 0, true, true},
		{"8080-sandbox.svc.example.com", "", 0, true, true},
		{"abcde234abcde.svc.example.com", "", 0, true, true},
		{"8080-ABCDE234ABCDE.svc.example.com", "", 0, true, true},
		{"8080-abcde234abcd1.svc.example.com", "", 0, true, true},
		{"8080-abcde234abcde.svc.", "", 0, true, true},
		{"8080-abcde234abcde.svc.example.com:bad", "", 0, true, true},
		{"8080-sandbox.example.com", "", 0, false, false},
	} {
		t.Run(tt.host, func(t *testing.T) {
			id, port, match, err := ParseAuthority(tt.host)
			assert.Equal(t, tt.match, match)
			assert.Equal(t, tt.invalid, err != nil)
			if !tt.invalid {
				assert.Equal(t, tt.id, id)
				assert.Equal(t, tt.port, port)
			}
		})
	}
}

type admissionVerifier struct {
	subject, uid   string
	mixed, invalid bool
	missingBinding bool
}

func (v admissionVerifier) Verify(raw string) (*oidc.TrafficAccessTokenClaims, error) {
	if v.invalid || raw != "secret-jwt" {
		return nil, errors.New("invalid signature")
	}
	uid := v.uid
	if uid == "" {
		uid = "serving-uid"
	}
	claims := &oidc.TrafficAccessTokenClaims{
		Claims: jwt.Claims{Subject: v.subject},
	}
	if !v.missingBinding {
		claims.SessionService = &oidc.SessionServiceClaims{SessionServiceID: "abcde234abcde", SessionServiceUID: uid}
	}
	if v.mixed {
		claims.Sandbox = &oidc.SandboxClaims{SandboxID: "sandbox"}
	}
	return claims, nil
}

func TestLocalSessionServiceAdmissionAndAllocation(t *testing.T) {
	for _, tt := range []struct {
		name, subject, token, uid                             string
		sessionID                                             string
		seed, changeLimit, unknownWrite, commitWrite          bool
		port, status                                          int
		mixed, invalid, missingVerifier                       bool
		invalidRoute, wantBinding                             bool
		unavailable, missing, duplicate, deleting, changePort bool
		missingBinding, changePortAfterWrite                  bool
	}{
		{name: "known", sessionID: "Ab_9012345678901234567", seed: true, subject: "sessionservice:controlplane:abcde234abcde", token: "secret-jwt", port: 8080, wantBinding: true},
		{name: "unknown", sessionID: "Ab_9012345678901234567", subject: "sessionservice:controlplane:abcde234abcde", token: "secret-jwt", port: 8080, status: 404},
		{name: "invalid ID", sessionID: "short", subject: "sessionservice:controlplane:abcde234abcde", token: "secret-jwt", port: 8080, status: 400},
		{name: "policy retry", changeLimit: true, subject: "sessionservice:controlplane:abcde234abcde", token: "secret-jwt", port: 8080, wantBinding: true},
		{name: "unknown write committed", unknownWrite: true, commitWrite: true, subject: "sessionservice:controlplane:abcde234abcde", token: "secret-jwt", port: 8080, wantBinding: true},
		{name: "unknown write uncommitted", unknownWrite: true, subject: "sessionservice:controlplane:abcde234abcde", token: "secret-jwt", port: 8080, status: 503},
		{name: "success", subject: "sessionservice:controlplane:abcde234abcde", token: "secret-jwt", port: 8080, wantBinding: true},
		{name: "cache unavailable", subject: "sessionservice:controlplane:abcde234abcde", token: "secret-jwt", port: 8080, status: 503, unavailable: true},
		{name: "identity missing", token: "secret-jwt", port: 8080, status: 404, missing: true},
		{name: "identity ambiguous", token: "secret-jwt", port: 8080, status: 503, duplicate: true},
		{name: "parent deleting", token: "secret-jwt", port: 8080, status: 503, deleting: true},
		{name: "port removed after admission", subject: "sessionservice:controlplane:abcde234abcde", token: "secret-jwt", port: 8080, status: 403, changePort: true},
		{name: "port removed after binding write", subject: "sessionservice:controlplane:abcde234abcde", token: "secret-jwt", port: 8080, status: 503, changePortAfterWrite: true, wantBinding: true},
		{name: "missing token", port: 8080, status: 401},
		{name: "verifier unavailable", token: "secret-jwt", port: 8080, status: 503, missingVerifier: true},
		{name: "invalid signature", token: "secret-jwt", port: 8080, status: 401, invalid: true},
		{name: "old UID", subject: "sessionservice:controlplane:abcde234abcde", uid: "old", token: "secret-jwt", port: 8080, status: 404},
		{name: "wrong SessionService", subject: "sessionservice:controlplane:other", token: "secret-jwt", port: 8080, status: 401},
		{name: "missing binding", subject: "sessionservice:controlplane:abcde234abcde", token: "secret-jwt", port: 8080, status: 401, missingBinding: true},
		{name: "old subject only", subject: "serving:abcde234abcde:serving-uid", token: "secret-jwt", port: 8080, status: 401, missingBinding: true},
		{name: "mixed token", subject: "sessionservice:controlplane:abcde234abcde", token: "secret-jwt", port: 8080, status: 401, mixed: true},
		{name: "port denied", subject: "sessionservice:controlplane:abcde234abcde", token: "secret-jwt", port: 9090, status: 403},
		{name: "invalid member IP", subject: "sessionservice:controlplane:abcde234abcde", token: "secret-jwt", port: 8080, status: 503, invalidRoute: true, wantBinding: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, agents.AddToScheme(scheme))
			parent := &agents.SessionService{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "demo", UID: "serving-uid", Labels: map[string]string{sessionbinding.SessionServiceIDLabel: "abcde234abcde"}}, Spec: agents.SessionServiceSpec{Ports: []int32{8080}}}
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
			objects := []client.Object{box}
			if !tt.missing {
				objects = append(objects, parent)
			}
			if tt.deleting {
				now := metav1.Now()
				parent.DeletionTimestamp = &now
				parent.Finalizers = []string{"test.example/hold"}
			}
			if tt.duplicate {
				other := parent.DeepCopy()
				other.Name, other.UID = "other", "other-uid"
				objects = append(objects, other)
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
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(hooks).WithIndex(parent, infracache.IndexSessionServiceID, func(o client.Object) []string { return []string{o.GetLabels()[sessionbinding.SessionServiceIDLabel]} }).Build()
			service := NewService(func() oidc.Verifier {
				if tt.missingVerifier {
					return nil
				}
				return admissionVerifier{subject: tt.subject, uid: tt.uid, mixed: tt.mixed, invalid: tt.invalid, missingBinding: tt.missingBinding}
			})
			generated := 0
			service.newSessionID = func() (string, error) { generated++; return randomSessionID() }
			require.NoError(t, service.Configure(kubernetes.NewStore(c, func() bool { return !tt.unavailable }, kubernetes.Options{})))
			route, err := service.ResolveRoute(context.Background(), "abcde234abcde", tt.sessionID, tt.port, tt.token)
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
				assert.Empty(t, route.SessionID)
			}
			if !tt.wantBinding && !tt.unknownWrite {
				assert.Zero(t, bindingWrites, "rejected requests must not write bindings")
				if !tt.changePort && tt.name != "unknown" && !tt.unknownWrite {
					assert.Zero(t, memberReads, "admission must reject before reading member bindings")
				}
			}
			if tt.changePortAfterWrite {
				assert.Equal(t, 1, bindingWrites, "a policy change after mutation must not restart allocation")
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
				assert.Zero(t, generated, "rejected authorization must not generate an identity")
			}
			for _, value := range current.Annotations {
				assert.NotContains(t, value, "secret-jwt")
			}
		})
	}
}
