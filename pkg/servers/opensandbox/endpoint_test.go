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
	"time"

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
	const configuredGateway = "https://sandbox.example.test"
	for _, tt := range []struct {
		name                       string
		mode, gateway, port, query string
		state, token               string
		missing, foreign, noIP     bool
		wantStatus                 int
		wantCode, wantEndpoint     string
	}{
		{name: "direct owned sandbox", mode: "direct", wantStatus: 200, wantEndpoint: "10.0.0.42:44772"},
		{name: "ingress owned sandbox", mode: "ingress", wantStatus: 200, wantEndpoint: "sandbox.example.test"},
		{name: "egress port maps to the sidecar", mode: "direct", port: "18080", wantStatus: 200, wantEndpoint: "10.0.0.42:44772"},
		{name: "missing address is SDK-retryable", mode: "direct", noIP: true, wantStatus: 404, wantCode: "KUBERNETES::POD_IP_NOT_AVAILABLE"},
		{name: "foreign owner is hidden", mode: "direct", foreign: true, wantStatus: 404, wantCode: "NOT_FOUND"},
		{name: "missing sandbox is hidden", mode: "direct", missing: true, wantStatus: 404, wantCode: "NOT_FOUND"},
		{name: "invalid port", mode: "direct", port: "0", wantStatus: 400, wantCode: "INVALID_REQUEST"},
		{name: "paused sandbox", mode: "direct", state: "paused", wantStatus: 409, wantCode: "CONFLICT"},
		{name: "dead sandbox", mode: "direct", state: "dead", wantStatus: 409, wantCode: "CONFLICT"},
		{name: "claimed sandbox not ready", mode: "direct", state: "provisioning", wantStatus: 409, wantCode: "CONFLICT"},
		{name: "expired sandbox", mode: "direct", state: "expired", wantStatus: 409, wantCode: "CONFLICT"},
		{name: "claimed sandbox without address is retryable", mode: "direct", state: "provisioning", noIP: true, wantStatus: 404, wantCode: "KUBERNETES::POD_IP_NOT_AVAILABLE"},
		{name: "expired sandbox without address is retryable", mode: "direct", state: "expired", noIP: true, wantStatus: 404, wantCode: "KUBERNETES::POD_IP_NOT_AVAILABLE"},
		{name: "execution credential unavailable", mode: "direct", token: "none", wantStatus: 503, wantCode: "SERVICE_UNAVAILABLE"},
		{name: "ingress without a configured origin", mode: "ingress", gateway: "none", wantStatus: 503, wantCode: "SERVICE_UNAVAILABLE"},
		{name: "lifecycle server proxy", mode: "direct", query: "?use_server_proxy=true", wantStatus: 501, wantCode: "NOT_IMPLEMENTED"},
		{name: "signed endpoint url", mode: "direct", query: "?expires=60", wantStatus: 501, wantCode: "NOT_IMPLEMENTED"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gateway := configuredGateway
			if tt.gateway == "none" {
				gateway = ""
			}
			port := tt.port
			if port == "" {
				port = "44772"
			}
			token := "runtime-token"
			if tt.token == "none" {
				token = ""
			}
			owner := user.ID.String()
			if tt.foreign {
				owner = "another-owner"
			}
			probe := &createIdentityInfra{}
			ip := "10.0.0.42"
			if tt.noIP {
				ip = ""
			}
			if !tt.missing {
				annotations := map[string]string{v1alpha1.AnnotationOwner: owner}
				if token != "" {
					annotations[v1alpha1.AnnotationRuntimeAccessToken] = token
				}
				ready := metav1.ConditionTrue
				if tt.state == "provisioning" {
					ready = metav1.ConditionFalse
				}
				box := &v1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "example", UID: types.UID("sandbox-uid"), ResourceVersion: "1", Annotations: annotations},
					Spec:       v1alpha1.SandboxSpec{Paused: tt.state == "paused"},
					Status: v1alpha1.SandboxStatus{
						Phase:   v1alpha1.SandboxRunning,
						PodInfo: v1alpha1.PodInfo{PodIP: ip},
						Conditions: []metav1.Condition{{
							Type: string(v1alpha1.SandboxConditionReady), Status: ready,
							Reason: "Ready", LastTransitionTime: metav1.Now(),
						}},
					},
				}
				switch tt.state {
				case "dead":
					box.Status.Phase = v1alpha1.SandboxSucceeded
				case "expired":
					deadline := metav1.NewTime(time.Now().Add(-time.Minute))
					box.Spec.ShutdownTime = &deadline
				}
				probe.result = sandboxcr.AsSandbox(box, nil)
			}
			manager, err := sandboxmanager.NewSandboxManagerBuilder(config.SandboxManagerOptions{}).WithCustomInfra(func() (infra.Builder, error) { return probe, nil }).Build()
			require.NoError(t, err)
			mux := http.NewServeMux()
			require.NoError(t, RegisterRoutes(Deps{Mux: mux, Manager: manager, Keys: &fakeKeyStorage{byKey: map[string]*models.CreatedTeamAPIKey{user.Key: user}}, Endpoints: EndpointOptions{AccessMode: tt.mode, GatewayURL: gateway}}))
			r := httptest.NewRequest("GET", "/v1/sandboxes/default--example/endpoints/"+port+tt.query, nil)
			r.Header.Set(HeaderOpenSandboxAPIKey, user.Key)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			require.Equal(t, tt.wantStatus, w.Code, w.Body.String())
			if tt.wantStatus != http.StatusOK {
				var failure ErrorResponse
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &failure))
				require.Equal(t, tt.wantCode, failure.Code)
				return
			}
			var endpoint Endpoint
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &endpoint))
			require.Equal(t, tt.wantEndpoint, endpoint.Endpoint)
			require.Equal(t, "runtime-token", endpoint.Headers["X-EXECD-ACCESS-TOKEN"])
			if tt.mode == "direct" {
				require.Empty(t, endpoint.Headers["e2b-sandbox-id"])
			} else {
				require.Equal(t, "default--example", endpoint.Headers["e2b-sandbox-id"])
				require.Equal(t, "44772", endpoint.Headers["e2b-sandbox-port"])
			}
		})
	}
}

func TestEndpointOptionsValidate(t *testing.T) {
	for _, tt := range []struct {
		name             string
		mode, gateway    string
		wantErrorMessage string
	}{
		{name: "empty configuration", mode: "", gateway: ""},
		{name: "ingress mode", mode: "ingress", gateway: "https://sandbox.example.test"},
		{name: "direct mode", mode: "direct", gateway: "http://sandbox.example.test:7788"},
		{name: "origin with root path", mode: "ingress", gateway: "https://sandbox.example.test/"},
		{name: "unknown access mode", mode: "gateway", wantErrorMessage: "opensandbox access mode must be ingress or direct"},
		{name: "gateway url with path", mode: "ingress", gateway: "https://sandbox.example.test/p", wantErrorMessage: "opensandbox gateway URL must be an HTTP(S) origin without path, query or credentials"},
		{name: "gateway url with query", mode: "ingress", gateway: "https://sandbox.example.test?x=1", wantErrorMessage: "opensandbox gateway URL must be an HTTP(S) origin without path, query or credentials"},
		{name: "gateway url with credentials", mode: "ingress", gateway: "https://user:password@sandbox.example.test", wantErrorMessage: "opensandbox gateway URL must be an HTTP(S) origin without path, query or credentials"},
		{name: "gateway url with unsupported scheme", mode: "ingress", gateway: "ftp://sandbox.example.test", wantErrorMessage: "opensandbox gateway URL must be an HTTP(S) origin without path, query or credentials"},
		{name: "gateway url without host", mode: "ingress", gateway: "https://", wantErrorMessage: "opensandbox gateway URL must be an HTTP(S) origin without path, query or credentials"},
		{name: "gateway url without scheme", mode: "ingress", gateway: "//sandbox.example.test", wantErrorMessage: "opensandbox gateway URL must be an HTTP(S) origin without path, query or credentials"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := EndpointOptions{AccessMode: tt.mode, GatewayURL: tt.gateway}.validate()
			if tt.wantErrorMessage == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tt.wantErrorMessage)
		})
	}
}
