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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/api/v1alpha1"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

func TestEndpointIdentityAndDiscovery(t *testing.T) {
	user := &models.CreatedTeamAPIKey{ID: uuid.New(), Key: "owner-key", Team: &models.Team{Name: "default"}}
	for _, tt := range []struct {
		name, mode, owner, ip string
		missing               bool
		status                int
	}{
		{name: "direct owned sandbox", mode: "direct", owner: user.ID.String(), ip: "10.0.0.42", status: 200},
		{name: "ingress owned sandbox", mode: "ingress", owner: user.ID.String(), ip: "10.0.0.42", status: 200},
		{name: "missing address is SDK-retryable", mode: "direct", owner: user.ID.String(), status: 404},
		{name: "foreign owner is hidden", mode: "direct", owner: "another-owner", ip: "10.0.0.42", status: 404},
		{name: "missing sandbox is hidden", mode: "direct", missing: true, status: 404},
	} {
		t.Run(tt.name, func(t *testing.T) {
			probe := &createIdentityInfra{}
			if !tt.missing {
				probe.result = sandboxcr.AsSandbox(&v1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "example", UID: types.UID("sandbox-uid"), ResourceVersion: "1", Annotations: map[string]string{v1alpha1.AnnotationOwner: tt.owner, v1alpha1.AnnotationRuntimeAccessToken: "runtime-token"}}, Status: v1alpha1.SandboxStatus{Phase: v1alpha1.SandboxRunning, PodInfo: v1alpha1.PodInfo{PodIP: tt.ip}, Conditions: []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}}}, nil)
			}
			manager, err := sandboxmanager.NewSandboxManagerBuilder(config.SandboxManagerOptions{}).WithCustomInfra(func() (infra.Builder, error) { return probe, nil }).Build()
			require.NoError(t, err)
			mux := http.NewServeMux()
			require.NoError(t, RegisterRoutes(Deps{Mux: mux, Manager: manager, Keys: &fakeKeyStorage{byKey: map[string]*models.CreatedTeamAPIKey{user.Key: user}}, Endpoints: EndpointOptions{AccessMode: tt.mode, GatewayURL: "https://sandbox.example.test"}}))
			r := httptest.NewRequest("GET", "/v1/sandboxes/default--example/endpoints/44772", nil)
			r.Header.Set(HeaderOpenSandboxAPIKey, user.Key)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			require.Equal(t, tt.status, w.Code, w.Body.String())
			if w.Code == 200 {
				var endpoint Endpoint
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &endpoint))
				require.Equal(t, "runtime-token", endpoint.Headers["X-EXECD-ACCESS-TOKEN"])
				if tt.mode == "direct" {
					require.Equal(t, "10.0.0.42:44772", endpoint.Endpoint)
					require.Empty(t, endpoint.Headers["e2b-sandbox-id"])
				} else {
					require.Equal(t, "sandbox.example.test", endpoint.Endpoint)
					require.Equal(t, "default--example", endpoint.Headers["e2b-sandbox-id"])
				}
			} else if tt.ip == "" && !tt.missing {
				require.Contains(t, w.Body.String(), "KUBERNETES::POD_IP_NOT_AVAILABLE")
			} else {
				require.JSONEq(t, `{"code":"NOT_FOUND","message":"sandbox not found"}`, w.Body.String())
			}
		})
	}
}
