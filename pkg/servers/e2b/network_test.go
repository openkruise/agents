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
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

func boolPointer(value bool) *bool {
	return &value
}

func TestValidateSandboxNetwork(t *testing.T) {
	tests := []struct {
		name        string
		network     *models.SandboxNetwork
		expectError string
	}{
		{
			name: "accepts supported targets",
			network: &models.SandboxNetwork{
				AllowOut: []string{"api.example.com", "*.example.net", "10.0.0.1", "10.0.0.0/8"},
				DenyOut:  []string{"192.0.2.1", "2001:db8::/32"},
				Rules: map[string][]models.SandboxNetworkRule{
					"api.example.com": {{Transform: &models.SandboxNetworkTransform{Headers: map[string]string{"X-Api-Key": "secret"}}}},
				},
			},
		},
		{
			name:        "rejects domains in deny list",
			network:     &models.SandboxNetwork{DenyOut: []string{"example.com"}},
			expectError: "invalid denyOut target",
		},
		{
			name:        "rejects malformed allow target",
			network:     &models.SandboxNetwork{AllowOut: []string{"not a domain"}},
			expectError: "invalid allowOut target",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiErr := validateSandboxNetwork(tt.network)
			if tt.expectError == "" {
				assert.Nil(t, apiErr)
				return
			}
			require.NotNil(t, apiErr)
			assert.Equal(t, http.StatusBadRequest, apiErr.Code)
			assert.Contains(t, apiErr.Message, tt.expectError)
		})
	}
}

func TestSandboxNetworkLifecycle(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, controller *Controller, user *models.CreatedTeamAPIKey)
	}{
		{
			name: "create stores and returns network configuration",
			run: func(t *testing.T, controller *Controller, user *models.CreatedTeamAPIKey) {
				cleanup := CreateSandboxPool(t, controller, "network-create", 1)
				defer cleanup()
				response, apiErr := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
					TemplateID:          "network-create",
					AllowInternetAccess: boolPointer(false),
					Network: &models.SandboxNetwork{
						AllowOut:           []string{"api.example.com"},
						AllowPublicTraffic: boolPointer(false),
						MaskRequestHost:    "sandbox.example.com",
					},
					Metadata: map[string]string{models.ExtensionKeySkipInitRuntime: agentsv1alpha1.True},
				}, nil, user))
				require.Nil(t, apiErr)
				assert.False(t, response.Body.AllowInternetAccess)
				require.NotNil(t, response.Body.Network)
				assert.Equal(t, []string{"api.example.com"}, response.Body.Network.AllowOut)
				assert.Equal(t, boolPointer(false), response.Body.Network.AllowPublicTraffic)

				persisted := GetSandbox(t, response.Body.SandboxID, getTestCRClient(controller))
				state, err := unmarshalSandboxNetworkState(persisted.Annotations[agentsv1alpha1.AnnotationNetworkConfig])
				require.NoError(t, err)
				assert.False(t, state.AllowInternetAccess)
				assert.Equal(t, response.Body.Network, state.Network)
			},
		},
		{
			name: "update replaces egress and preserves create-only settings",
			run: func(t *testing.T, controller *Controller, user *models.CreatedTeamAPIKey) {
				cleanup := CreateSandboxPool(t, controller, "network-update", 1)
				defer cleanup()
				createResponse, apiErr := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
					TemplateID: "network-update",
					Network: &models.SandboxNetwork{
						AllowOut:           []string{"old.example.com"},
						DenyOut:            []string{"192.0.2.1"},
						AllowPublicTraffic: boolPointer(false),
						MaskRequestHost:    "sandbox.example.com",
					},
					Metadata: map[string]string{models.ExtensionKeySkipInitRuntime: agentsv1alpha1.True},
				}, nil, user))
				require.Nil(t, apiErr)
				sandboxID := createResponse.Body.SandboxID

				request := httptest.NewRequest(http.MethodPut, "/sandboxes/"+sandboxID+"/network", bytes.NewBufferString(`{
					"allowOut":["new.example.com"],
					"rules":{"new.example.com":[{}]},
					"allow_internet_access":false
				}`))
				request.Header.Set("X-API-Key", InitKey)
				response := httptest.NewRecorder()
				controller.mux.ServeHTTP(response, request)
				assert.Equal(t, http.StatusNoContent, response.Code)
				assert.Empty(t, response.Body.String())

				persisted := GetSandbox(t, sandboxID, getTestCRClient(controller))
				state, decodeErr := unmarshalSandboxNetworkState(persisted.Annotations[agentsv1alpha1.AnnotationNetworkConfig])
				require.NoError(t, decodeErr)
				assert.False(t, state.AllowInternetAccess)
				assert.Equal(t, []string{"new.example.com"}, state.Network.AllowOut)
				assert.Empty(t, state.Network.DenyOut)
				assert.Equal(t, boolPointer(false), state.Network.AllowPublicTraffic)
				assert.Equal(t, "sandbox.example.com", state.Network.MaskRequestHost)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, _, teardown := Setup(t)
			defer teardown()
			user := &models.CreatedTeamAPIKey{ID: keys.AdminKeyID, Key: InitKey, Name: "admin"}
			tt.run(t, controller, user)
		})
	}
}
