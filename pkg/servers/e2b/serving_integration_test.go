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

package e2b

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	agents "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/configreload"
	"github.com/openkruise/agents/pkg/identity"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	sessionbinding "github.com/openkruise/agents/pkg/servingsession/binding"
)

// Record the API authorization scope while issuing through the real default provider.
type servingAPITestProvider struct {
	identity.IdentityProvider
	scope identity.ServingTokenScope
	calls int
}

func (p *servingAPITestProvider) IssueServingToken(ctx context.Context, scope identity.ServingTokenScope, opts identity.TokenOptions) (*identity.TokenResponse, error) {
	p.scope = scope
	p.calls++
	return p.IdentityProvider.(identity.ServingTokenIssuer).IssueServingToken(ctx, scope, opts)
}

func TestServingNativeLifecycleAndAuthorization(t *testing.T) {
	parent := &agents.SandboxServing{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a", Name: "serving", UID: "serving-uid", ResourceVersion: "1",
		Labels: map[string]string{sessionbinding.ServingIDLabel: "serving-a"},
	}, Spec: agents.SandboxServingSpec{Ports: []int32{8080}}}
	bindings := map[string]any{}
	for i := 0; i < 55; i++ {
		bindings[fmt.Sprintf("session-%02d", i)] = map[string]any{"state": "Active", "lastRequestAt": "2026-11-10T00:00:00Z", "future": "discarded"}
	}
	raw, err := json.Marshal(bindings)
	require.NoError(t, err)
	member := &agents.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a", Name: "member", UID: "member-uid", ResourceVersion: "1",
		Labels:          map[string]string{sessionbinding.ServingUIDLabel: "serving-uid", agents.LabelSandboxID: "member-id"},
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(parent, agents.GroupVersion.WithKind("SandboxServing"))},
		Annotations:     map[string]string{sessionbinding.BindingsAnnotation: string(raw), "unrelated": "preserved"},
	}}
	cache, fc, err := cachetest.NewTestServingCache(t, parent, member,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "sandbox-system", Name: keys.KeySecretName}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b"}},
	)
	require.NoError(t, err)
	opts := config.InitOptions(config.SandboxManagerOptions{DisableEnvoyExtProc: true, SystemNamespace: "sandbox-system"})
	sc := NewController(ControllerOptions{Manager: opts, KeyConfig: &keys.Config{Mode: keys.StorageModeSecret, Namespace: "sandbox-system", AdminKey: InitKey}})
	manager, err := sandboxmanager.NewSandboxManagerBuilder(opts).WithCustomInfra(func() (infra.Builder, error) {
		return sandboxcr.NewInfraBuilder(opts).WithCache(cache).WithAPIReader(fc), nil
	}).Build()
	require.NoError(t, err)
	require.NoError(t, manager.GetInfra().Run(t.Context()))
	sc.manager, sc.cache = manager, cache
	sc.registerRoutes()
	require.NoError(t, sc.initKeyStorage(t.Context()))
	_, err = sc.keys.Reload(t.Context(), map[string]string{configreload.AdminKey: InitKey})
	require.NoError(t, err)
	admin := adminTestUser()
	teamA, err := sc.keys.CreateKey(t.Context(), admin, keys.CreateKeyOptions{Name: "team-a-first", TeamName: "team-a"})
	require.NoError(t, err)
	sharedA, err := sc.keys.CreateKey(t.Context(), admin, keys.CreateKeyOptions{Name: "team-a-second", TeamName: "team-a"})
	require.NoError(t, err)
	teamB, err := sc.keys.CreateKey(t.Context(), admin, keys.CreateKeyOptions{Name: "team-b", TeamName: "team-b"})
	require.NoError(t, err)
	refreshKeyStorageForTest(t, sc)
	request := func(method, suffix, key string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/kruise/api/v1alpha1/sandboxservings/serving-a"+suffix, nil)
		r.Header.Set(models.HeaderApiKey, key)
		response := httptest.NewRecorder()
		sc.mux.ServeHTTP(response, r)
		return response
	}
	for _, caller := range []struct {
		name, key string
		status    int
	}{
		{"team first", teamA.Key, 200}, {"same team other key", sharedA.Key, 200}, {"admin", InitKey, 200}, {"cross team", teamB.Key, 404}, {"missing key", "", 401},
	} {
		t.Run(caller.name, func(t *testing.T) {
			response := request(http.MethodGet, "/sessions/session-00", caller.key)
			require.Equal(t, caller.status, response.Code, response.Body.String())
			if caller.status == 200 {
				var body map[string]any
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
				require.Equal(t, map[string]any{"sessionId": "session-00", "servingID": "serving-a", "sandboxID": "member-id", "state": "Active", "lastRequestAt": "2026-11-10T00:00:00Z"}, body)
			}
		})
	}
	response := request(http.MethodGet, "/sessions", teamA.Key)
	require.Equal(t, 200, response.Code, response.Body.String())
	var sessions []models.ServingSession
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &sessions))
	require.Len(t, sessions, 50)
	require.Equal(t, "session-00", sessions[0].SessionID)
	require.Equal(t, "session-49", sessions[49].SessionID)
	cursor := response.Header().Get("x-next-token")
	require.NotEmpty(t, cursor)
	response = request(http.MethodGet, "/sessions?nextToken="+url.QueryEscape(cursor), sharedA.Key)
	require.Equal(t, 200, response.Code, response.Body.String())
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &sessions))
	require.Len(t, sessions, 5)
	require.Equal(t, "session-50", sessions[0].SessionID)
	require.Empty(t, response.Header().Get("x-next-token"))
	response = request(http.MethodGet, "/sessions?nextToken=bad-cursor", teamA.Key)
	require.Equal(t, 400, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"message":"Invalid session cursor"`)
	for _, key := range []string{teamA.Key, sharedA.Key} {
		response = request(http.MethodPost, "/sessions/session-00:close", key)
		require.Equal(t, 200, response.Code, response.Body.String())
		var closed map[string]any
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &closed))
		require.Equal(t, map[string]any{"sessionId": "session-00", "servingID": "serving-a", "state": "Closed"}, closed)
	}
	response = request(http.MethodGet, "/sessions/session-00", sharedA.Key)
	require.Equal(t, 404, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"message":"Session not found"`)
	require.NoError(t, fc.Get(t.Context(), ctrlclient.ObjectKeyFromObject(member), member))
	require.Equal(t, "preserved", member.Annotations["unrelated"])
	var remaining map[string]map[string]any
	require.NoError(t, json.Unmarshal([]byte(member.Annotations[sessionbinding.BindingsAnnotation]), &remaining))
	require.Len(t, remaining, 54)
	require.Equal(t, map[string]any{"state": "Active", "lastRequestAt": "2026-11-10T00:00:00Z"}, remaining["session-01"])
	provider := &servingAPITestProvider{IdentityProvider: identity.NewDefaultIdentityProvider()}
	identity.RegisterProvider(provider)
	t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })
	for _, caller := range []struct {
		key     string
		subject string
		status  int
	}{{teamA.Key, teamA.ID.String(), 200}, {sharedA.Key, sharedA.ID.String(), 200}, {InitKey, keys.AdminKeyID.String(), 200}, {teamB.Key, "", 404}, {"", "", 401}} {
		beforeCalls := provider.calls
		response = request(http.MethodPost, "/traffic-access-token", caller.key)
		require.Equal(t, caller.status, response.Code, response.Body.String())
		if caller.status != 200 {
			require.Equal(t, beforeCalls, provider.calls)
		}
		if caller.status == 200 {
			require.Equal(t, identity.ServingTokenScope{ServingID: "serving-a", ServingUID: "serving-uid", Subject: caller.subject}, provider.scope)
			var token models.TrafficAccessToken
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &token))
			_, err = uuid.Parse(token.TrafficAccessToken)
			require.NoError(t, err)
			expires, parseErr := time.Parse(time.RFC3339, token.TrafficAccessTokenExpiration)
			require.NoError(t, parseErr)
			require.True(t, expires.After(time.Now()))
		}
	}
}
