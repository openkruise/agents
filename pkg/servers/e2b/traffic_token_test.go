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
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/utils"
	utilruntime "github.com/openkruise/agents/pkg/utils/runtime"
)

type e2bTrafficTokenProvider struct {
	calls atomic.Int32
}

func (p *e2bTrafficTokenProvider) IssueToken(_ context.Context, _ *v1alpha1.Sandbox, opts identity.TokenOptions) (*identity.TokenResponse, error) {
	p.calls.Add(1)
	return &identity.TokenResponse{
		AccessToken:           "refreshed-jwt",
		AccessTokenExpiration: time.Now().Add(opts.RequestedValidity).UTC().Format(time.RFC3339),
	}, nil
}

func (*e2bTrafficTokenProvider) PropagateSecurityToken(context.Context, *v1alpha1.Sandbox, *identity.TokenResponse, ...utilruntime.Option) error {
	return nil
}

func TestRefreshTrafficAccessToken(t *testing.T) {
	controller, client, teardown := Setup(t)
	defer teardown()
	identity.RegisterProvider(&e2bTrafficTokenProvider{})
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
	sandboxID := utils.GetSandboxID(sandbox)
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
	assert.Equal(t, "refreshed-jwt", response.Body.TrafficAccessToken)
	assert.NotEmpty(t, response.Body.TrafficAccessTokenExpiration)

	_, apiErr = controller.RefreshTrafficAccessToken(request)
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusTooManyRequests, apiErr.Code)
	assert.NotEmpty(t, apiErr.Headers["Retry-After"])
}

func TestConnectSandboxTrafficAccessTokenBootstrap(t *testing.T) {
	tests := []struct {
		name        string
		enableJWT   bool
		expectToken bool
	}{
		{name: "JWT protected sandbox receives bootstrap token", enableJWT: true, expectToken: true},
		{name: "unprotected sandbox keeps existing response", enableJWT: false},
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
					Name: "connect-bootstrap", Namespace: "sandbox-system", UID: "connect-bootstrap-uid",
					Labels: map[string]string{v1alpha1.LabelSandboxIsClaimed: v1alpha1.True}, Annotations: annotations,
				},
				Status: v1alpha1.SandboxStatus{
					Phase:      v1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}},
				},
			}
			CreateSandboxWithStatus(t, client, sandbox)
			sandboxID := utils.GetSandboxID(sandbox)
			require.Eventually(t, func() bool {
				_, err := controller.manager.GetInfra().GetSandbox(t.Context(), infra.GetSandboxOptions{SandboxID: sandboxID})
				return err == nil
			}, time.Second, 10*time.Millisecond)
			user := &models.CreatedTeamAPIKey{ID: keys.AdminKeyID, Team: models.AdminTeam()}
			response, apiErr := controller.ConnectSandbox(NewRequest(t, nil, models.SetTimeoutRequest{TimeoutSeconds: 30}, map[string]string{"sandboxID": sandboxID}, user))
			require.Nil(t, apiErr)
			require.NotNil(t, response.Body)
			if tt.expectToken {
				assert.Equal(t, "refreshed-jwt", response.Body.TrafficAccessToken)
				assert.NotEmpty(t, response.Body.TrafficAccessTokenExpiration)
				assert.Equal(t, int32(1), provider.calls.Load())
			} else {
				assert.Empty(t, response.Body.TrafficAccessToken)
				assert.Empty(t, response.Body.TrafficAccessTokenExpiration)
				assert.Zero(t, provider.calls.Load())
			}
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
		{name: "rate limited", err: managererrors.NewError(managererrors.ErrorRateLimited, "slow down"), wantStatus: http.StatusTooManyRequests},
		{name: "unavailable", err: managererrors.NewError(managererrors.ErrorUnavailable, "issuer"), wantStatus: http.StatusServiceUnavailable},
		{name: "unknown", err: context.Canceled, wantStatus: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiErr := trafficTokenAPIError(tt.err, 1500*time.Millisecond)
			assert.Equal(t, tt.wantStatus, apiErr.Code)
			if tt.wantStatus == http.StatusTooManyRequests {
				assert.Equal(t, "2", apiErr.Headers["Retry-After"])
			}
			encoded, err := json.Marshal(apiErr)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), tt.err.Error())
		})
	}
}
