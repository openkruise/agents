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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandboxid"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	utilruntime "github.com/openkruise/agents/pkg/utils/runtime"
)

type e2bTrafficTokenProvider struct {
	calls atomic.Int32
}

func (p *e2bTrafficTokenProvider) IssueToken(_ context.Context, _ *v1alpha1.Sandbox, opts identity.TokenOptions) (*identity.TokenResponse, error) {
	call := p.calls.Add(1)
	return &identity.TokenResponse{
		AccessToken:           fmt.Sprintf("refreshed-jwt-%d", call),
		AccessTokenExpiration: time.Now().Add(opts.RequestedValidity).UTC().Format(time.RFC3339),
	}, nil
}

func (*e2bTrafficTokenProvider) PropagateSecurityToken(context.Context, *v1alpha1.Sandbox, *identity.TokenResponse, ...utilruntime.Option) error {
	return nil
}

func TestRefreshTrafficAccessToken(t *testing.T) {
	controller, client, teardown := Setup(t)
	defer teardown()
	provider := &e2bTrafficTokenProvider{}
	identity.RegisterProvider(provider)
	t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })

	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "refresh-token",
			Namespace: "sandbox-system",
			UID:       "refresh-token-uid",
			Labels:    map[string]string{v1alpha1.LabelSandboxIsClaimed: v1alpha1.True},
			Annotations: map[string]string{
				v1alpha1.AnnotationOwner:         keys.AdminKeyID.String(),
				identity.AnnotationEnableJwtAuth: v1alpha1.True,
			},
		},
		Status: v1alpha1.SandboxStatus{
			Phase:      v1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}},
		},
	}
	CreateSandboxWithStatus(t, client, sandbox)
	sandboxID := sandboxid.Resolve(sandbox)
	require.Eventually(t, func() bool {
		_, err := controller.manager.GetInfra().GetSandbox(t.Context(), infra.GetSandboxOptions{SandboxID: sandboxID})
		return err == nil
	}, time.Second, 10*time.Millisecond)

	request := httptest.NewRequest(http.MethodPost, "/sandboxes/"+sandboxID+"/traffic-access-token", nil)
	request.SetPathValue("sandboxID", sandboxID)
	request = request.WithContext(context.WithValue(request.Context(), "user", &models.CreatedTeamAPIKey{ID: keys.AdminKeyID, Team: models.AdminTeam()}))
	response, apiErr := controller.RefreshTrafficAccessToken(request)
	require.Nil(t, apiErr)
	require.NotNil(t, response.Body)
	assert.Equal(t, "refreshed-jwt-1", response.Body.TrafficAccessToken)
	assert.NotEmpty(t, response.Body.TrafficAccessTokenExpiration)

	second, apiErr := controller.RefreshTrafficAccessToken(request)
	require.Nil(t, apiErr)
	require.NotNil(t, second.Body)
	assert.Equal(t, "refreshed-jwt-2", second.Body.TrafficAccessToken)
	assert.Equal(t, int32(2), provider.calls.Load())
}

func TestTrafficAccessTokenRouteIsKruiseOnly(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	_, nativePattern := controller.mux.Handler(httptest.NewRequest(http.MethodPost, "/sandboxes/sandbox-id/traffic-access-token", nil))
	assert.NotEqual(t, "POST /sandboxes/{sandboxID}/traffic-access-token", nativePattern)
	_, kruisePattern := controller.mux.Handler(httptest.NewRequest(http.MethodPost, "/kruise/api/sandboxes/sandbox-id/traffic-access-token", nil))
	assert.Equal(t, "POST /kruise/api/sandboxes/{sandboxID}/traffic-access-token", kruisePattern)
}

func TestConnectSandboxDoesNotIssueTrafficAccessToken(t *testing.T) {
	tests := []struct {
		name      string
		enableJWT bool
	}{
		{name: "JWT protected sandbox", enableJWT: true},
		{name: "unprotected sandbox"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, client, teardown := Setup(t)
			defer teardown()
			provider := &e2bTrafficTokenProvider{}
			identity.RegisterProvider(provider)
			t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })
			annotations := map[string]string{v1alpha1.AnnotationOwner: keys.AdminKeyID.String()}
			if tt.enableJWT {
				annotations[identity.AnnotationEnableJwtAuth] = v1alpha1.True
			}
			sandbox := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name: "connect-no-token", Namespace: "sandbox-system", UID: "connect-no-token-uid",
					Labels: map[string]string{v1alpha1.LabelSandboxIsClaimed: v1alpha1.True}, Annotations: annotations,
				},
				Status: v1alpha1.SandboxStatus{
					Phase:      v1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}},
				},
			}
			CreateSandboxWithStatus(t, client, sandbox)
			sandboxID := sandboxid.Resolve(sandbox)
			require.Eventually(t, func() bool {
				_, err := controller.manager.GetInfra().GetSandbox(t.Context(), infra.GetSandboxOptions{SandboxID: sandboxID})
				return err == nil
			}, time.Second, 10*time.Millisecond)
			user := &models.CreatedTeamAPIKey{ID: keys.AdminKeyID, Team: models.AdminTeam()}
			response, apiErr := controller.ConnectSandbox(NewRequest(t, nil, models.SetTimeoutRequest{TimeoutSeconds: 30}, map[string]string{"sandboxID": sandboxID}, user))
			require.Nil(t, apiErr)
			require.NotNil(t, response.Body)
			assert.Empty(t, response.Body.TrafficAccessToken)
			assert.Empty(t, response.Body.TrafficAccessTokenExpiration)
			assert.Zero(t, provider.calls.Load())
		})
	}
}

func TestTrafficTokenAPIError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{name: "bad request", err: managererrors.NewError(managererrors.ErrorBadRequest, "bad"), wantStatus: http.StatusBadRequest},
		{name: "not found", err: managererrors.NewError(managererrors.ErrorNotFound, "missing"), wantStatus: http.StatusNotFound},
		{name: "not allowed is concealed", err: managererrors.NewError(managererrors.ErrorNotAllowed, "owner"), wantStatus: http.StatusNotFound},
		{name: "conflict", err: managererrors.NewError(managererrors.ErrorConflict, "state"), wantStatus: http.StatusConflict},
		{name: "unavailable", err: managererrors.NewError(managererrors.ErrorUnavailable, "issuer"), wantStatus: http.StatusServiceUnavailable},
		{name: "unknown", err: context.Canceled, wantStatus: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiErr := trafficTokenAPIError(tt.err)
			assert.Equal(t, tt.wantStatus, apiErr.Code)
			encoded, err := json.Marshal(apiErr)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), tt.err.Error())
		})
	}
}
