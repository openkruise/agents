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

package serving

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
	sessionbinding "github.com/openkruise/agents/pkg/servingsession/binding"
	"github.com/openkruise/agents/pkg/servingsession/kubernetes"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

func TestParseAuthority(t *testing.T) {
	for _, tt := range []struct {
		host, id       string
		port           int
		match, invalid bool
	}{
		{"8080-serving-demo.svc.example.com", "serving-demo", 8080, true, false},
		{"49983-serving-a-b.svc.example.com:443", "serving-a-b", 49983, true, false},
		{"0-serving-demo.svc.example.com", "", 0, true, true},
		{"65536-serving-demo.svc.example.com", "", 0, true, true},
		{"abc-serving-demo.svc.example.com", "", 0, true, true},
		{"8080-serving-.svc.example.com", "", 0, true, true},
		{"8080-sandbox.svc.example.com", "", 0, false, false},
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
	subject, uid    string
	mixed, invalid  bool
	missingMetadata bool
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
	if !v.missingMetadata {
		claims.Metadata = map[string]string{"servingId": "serving-demo", "servingUid": uid}
	}
	if v.mixed {
		claims.Sandbox = &oidc.SandboxClaims{SandboxID: "sandbox"}
	}
	return claims, nil
}

func TestLocalServingAdmissionAndAllocation(t *testing.T) {
	for _, tt := range []struct {
		name, subject, token, uid                             string
		port, status                                          int
		mixed, invalid, missingVerifier                       bool
		invalidRoute, wantBinding                             bool
		unavailable, missing, duplicate, deleting, changePort bool
		missingMetadata, changePortAfterWrite                 bool
	}{
		{name: "success", subject: "serving:controlplane:serving-demo", token: "secret-jwt", port: 8080, wantBinding: true},
		{name: "cache unavailable", subject: "serving:controlplane:serving-demo", token: "secret-jwt", port: 8080, status: 503, unavailable: true},
		{name: "identity missing", token: "secret-jwt", port: 8080, status: 404, missing: true},
		{name: "identity ambiguous", token: "secret-jwt", port: 8080, status: 503, duplicate: true},
		{name: "parent deleting", token: "secret-jwt", port: 8080, status: 503, deleting: true},
		{name: "port removed after admission", subject: "serving:controlplane:serving-demo", token: "secret-jwt", port: 8080, status: 403, changePort: true},
		{name: "port removed after binding write", subject: "serving:controlplane:serving-demo", token: "secret-jwt", port: 8080, status: 503, changePortAfterWrite: true, wantBinding: true},
		{name: "missing token", port: 8080, status: 401},
		{name: "verifier unavailable", token: "secret-jwt", port: 8080, status: 503, missingVerifier: true},
		{name: "invalid signature", token: "secret-jwt", port: 8080, status: 401, invalid: true},
		{name: "old UID", subject: "serving:controlplane:serving-demo", uid: "old", token: "secret-jwt", port: 8080, status: 404},
		{name: "wrong Serving", subject: "serving:controlplane:other", token: "secret-jwt", port: 8080, status: 401},
		{name: "missing metadata", subject: "serving:controlplane:serving-demo", token: "secret-jwt", port: 8080, status: 401, missingMetadata: true},
		{name: "old subject only", subject: "serving:serving-demo:serving-uid", token: "secret-jwt", port: 8080, status: 401, missingMetadata: true},
		{name: "mixed token", subject: "serving:controlplane:serving-demo", token: "secret-jwt", port: 8080, status: 401, mixed: true},
		{name: "port denied", subject: "serving:controlplane:serving-demo", token: "secret-jwt", port: 9090, status: 403},
		{name: "invalid member IP", subject: "serving:controlplane:serving-demo", token: "secret-jwt", port: 8080, status: 503, invalidRoute: true, wantBinding: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, agents.AddToScheme(scheme))
			parent := &agents.SandboxServing{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "demo", UID: "serving-uid", Labels: map[string]string{sessionbinding.ServingIDLabel: "serving-demo"}}, Spec: agents.SandboxServingSpec{Ports: []int32{8080}}}
			box := &agents.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "member", UID: "member-uid", Labels: map[string]string{sessionbinding.ServingUIDLabel: "serving-uid"}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(parent, agents.GroupVersion.WithKind("SandboxServing"))}}, Status: agents.SandboxStatus{Phase: agents.SandboxRunning, PodInfo: agents.PodInfo{PodIP: "10.0.0.1"}, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}}}
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
					if err := c.Patch(ctx, obj, patch, opts...); err != nil {
						return err
					}
					if tt.changePortAfterWrite {
						var updated agents.SandboxServing
						if err := c.Get(ctx, client.ObjectKeyFromObject(parent), &updated); err != nil {
							return err
						}
						updated.Spec.Ports = []int32{9090}
						return c.Update(ctx, &updated)
					}
					return nil
				},
			}
			if tt.changePort {
				changed := false
				hooks.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*agents.SandboxServing); ok && !changed {
						changed = true
						var updated agents.SandboxServing
						if err := c.Get(ctx, key, &updated); err != nil {
							return err
						}
						updated.Spec.Ports = []int32{9090}
						if err := c.Update(ctx, &updated); err != nil {
							return err
						}
					}
					return c.Get(ctx, key, obj, opts...)
				}
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(hooks).WithIndex(parent, infracache.IndexServingID, func(o client.Object) []string { return []string{o.GetLabels()[sessionbinding.ServingIDLabel]} }).Build()
			service := NewService(func() oidc.Verifier {
				if tt.missingVerifier {
					return nil
				}
				return admissionVerifier{subject: tt.subject, uid: tt.uid, mixed: tt.mixed, invalid: tt.invalid, missingMetadata: tt.missingMetadata}
			})
			require.NoError(t, service.Configure(kubernetes.NewStore(c, func() bool { return !tt.unavailable }, kubernetes.Options{})))
			route, err := service.ResolveRoute(context.Background(), "serving-demo", "session", tt.port, tt.token)
			if tt.status == 0 {
				require.NoError(t, err)
				assert.Equal(t, "member", route.Name)
				assert.Empty(t, route.AccessToken)
			} else {
				var rejection *Error
				require.ErrorAs(t, err, &rejection)
				assert.Equal(t, tt.status, rejection.Status)
			}
			if !tt.wantBinding {
				assert.Zero(t, bindingWrites, "rejected requests must not write bindings")
				if !tt.changePort {
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
			for _, value := range current.Annotations {
				assert.NotContains(t, value, "secret-jwt")
			}
		})
	}
}
