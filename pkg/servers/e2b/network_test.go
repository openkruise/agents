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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
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
		{
			name:        "rejects malformed rule target",
			network:     &models.SandboxNetwork{Rules: map[string][]models.SandboxNetworkRule{"": {{}}}},
			expectError: "invalid rules target",
		},
		{
			name: "accepts nil network",
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

	t.Run("rejects domains when disabled", func(t *testing.T) {
		err := validateNetworkTarget("example.com", false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expected an IP address or CIDR")
	})
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
		{
			name: "update creates network state from existing empty config",
			run: func(t *testing.T, controller *Controller, user *models.CreatedTeamAPIKey) {
				sbx := CreateClaimedSandboxCR(t, controller, Namespace, "network-empty", "network-empty", user.ID.String(), nil)
				sandboxID := sbx.Namespace + "--" + sbx.Name

				request := NewRequest(t, nil, models.UpdateSandboxNetworkRequest{
					AllowOut:            []string{"api.example.com"},
					AllowInternetAccess: boolPointer(false),
				}, map[string]string{"sandboxID": sandboxID}, user)
				response, apiErr := controller.UpdateSandboxNetwork(request)
				require.Nil(t, apiErr)
				assert.Equal(t, http.StatusNoContent, response.Code)

				persisted := GetSandbox(t, sandboxID, getTestCRClient(controller))
				state, decodeErr := unmarshalSandboxNetworkState(persisted.Annotations[agentsv1alpha1.AnnotationNetworkConfig])
				require.NoError(t, decodeErr)
				assert.False(t, state.AllowInternetAccess)
				require.NotNil(t, state.Network)
				assert.Equal(t, []string{"api.example.com"}, state.Network.AllowOut)
			},
		},
		{
			name: "update rejects invalid and unavailable sandboxes",
			run: func(t *testing.T, controller *Controller, user *models.CreatedTeamAPIKey) {
				badJSON := httptest.NewRequest(http.MethodPut, "/sandboxes/missing/network", bytes.NewBufferString("{"))
				badJSON.SetPathValue("sandboxID", "missing")
				badJSON = badJSON.WithContext(context.WithValue(badJSON.Context(), "user", user))
				_, apiErr := controller.UpdateSandboxNetwork(badJSON)
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusBadRequest, apiErr.Code)

				invalidNetwork := NewRequest(t, nil, models.UpdateSandboxNetworkRequest{
					AllowOut: []string{"not a domain"},
				}, map[string]string{"sandboxID": "missing"}, user)
				_, apiErr = controller.UpdateSandboxNetwork(invalidNetwork)
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusBadRequest, apiErr.Code)

			},
		},
		{
			name: "update rejects non-running sandboxes and bad stored config",
			run: func(t *testing.T, controller *Controller, user *models.CreatedTeamAPIKey) {
				fc := getTestCRClient(controller)
				paused := CreateClaimedSandboxCR(t, controller, Namespace, "network-paused", "network-paused", user.ID.String(), nil)
				pausedID := paused.Namespace + "--" + paused.Name
				UpdateSandboxWhen(t, fc, pausedID, Immediately, DoSetSandboxStatus(agentsv1alpha1.SandboxPaused, metav1.ConditionTrue, metav1.ConditionFalse))
				ctx := context.WithValue(t.Context(), "user", user)
				require.Eventually(t, func() bool {
					cached, apiErr := controller.getSandboxOfUser(ctx, pausedID, liveSandboxStates)
					if apiErr != nil {
						return false
					}
					state, _ := cached.GetState()
					return state == agentsv1alpha1.SandboxStatePaused
				}, time.Second, 10*time.Millisecond)

				request := NewRequest(t, nil, models.UpdateSandboxNetworkRequest{
					AllowOut: []string{"api.example.com"},
				}, map[string]string{"sandboxID": pausedID}, user)
				_, apiErr := controller.UpdateSandboxNetwork(request)
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusConflict, apiErr.Code)

				corrupt := CreateClaimedSandboxCR(t, controller, Namespace, "network-corrupt", "network-corrupt", user.ID.String(), map[string]string{
					agentsv1alpha1.AnnotationNetworkConfig: "{",
				})
				corruptID := corrupt.Namespace + "--" + corrupt.Name
				request = NewRequest(t, nil, models.UpdateSandboxNetworkRequest{
					AllowOut: []string{"api.example.com"},
				}, map[string]string{"sandboxID": corruptID}, user)
				_, apiErr = controller.UpdateSandboxNetwork(request)
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusInternalServerError, apiErr.Code)
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

func TestSandboxNetworkState(t *testing.T) {
	t.Run("create request defaults network fields", func(t *testing.T) {
		state := networkStateFromCreateRequest(models.NewSandboxRequest{
			Network: &models.SandboxNetwork{AllowOut: []string{"api.example.com"}},
		})
		assert.True(t, state.AllowInternetAccess)
		require.NotNil(t, state.Network)
		assert.Equal(t, boolPointer(true), state.Network.AllowPublicTraffic)
	})

	t.Run("create request preserves explicit internet access", func(t *testing.T) {
		state := networkStateFromCreateRequest(models.NewSandboxRequest{
			AllowInternetAccess: boolPointer(false),
		})
		assert.False(t, state.AllowInternetAccess)
		assert.Nil(t, state.Network)
	})

	t.Run("empty and invalid persisted state keep safe defaults", func(t *testing.T) {
		state, err := unmarshalSandboxNetworkState("")
		require.NoError(t, err)
		assert.True(t, state.AllowInternetAccess)
		assert.Nil(t, state.Network)

		state, err = unmarshalSandboxNetworkState("{")
		require.Error(t, err)
		assert.True(t, state.AllowInternetAccess)
		assert.Nil(t, state.Network)
	})

	t.Run("marshal round trip", func(t *testing.T) {
		raw, err := marshalSandboxNetworkState(sandboxNetworkState{
			AllowInternetAccess: false,
			Network:             &models.SandboxNetwork{AllowOut: []string{"api.example.com"}},
		})
		require.NoError(t, err)

		var decoded sandboxNetworkState
		require.NoError(t, json.Unmarshal([]byte(raw), &decoded))
		assert.False(t, decoded.AllowInternetAccess)
		require.NotNil(t, decoded.Network)
		assert.Equal(t, []string{"api.example.com"}, decoded.Network.AllowOut)
	})
}

func TestParseCreateSandboxRequestRejectsInvalidNetwork(t *testing.T) {
	ctrl := &Controller{maxTimeout: 3600}
	request := NewRequest(t, nil, models.NewSandboxRequest{
		TemplateID: "test-template",
		Network:    &models.SandboxNetwork{Rules: map[string][]models.SandboxNetworkRule{"": {{}}}},
	}, nil, nil)

	_, apiErr := ctrl.parseCreateSandboxRequest(request)
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.Code)
	assert.Contains(t, apiErr.Message, "invalid rules target")
}

func TestConvertToE2BSandboxHandlesInvalidNetworkState(t *testing.T) {
	sbx := &sandboxcr.Sandbox{
		Sandbox: &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sandbox-1",
				Namespace: "default",
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationNetworkConfig: "{",
				},
			},
		},
	}

	got := (&Controller{}).convertToE2BSandbox(sbx, "")
	assert.True(t, got.AllowInternetAccess)
	assert.Nil(t, got.Network)
}
