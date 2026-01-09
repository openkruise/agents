package e2b

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func imageChecker(image string, controller *Controller) func(t *testing.T, resp *models.Sandbox) {
	return func(t *testing.T, resp *models.Sandbox) {
		sbx, err := controller.manager.GetClaimedSandbox(t.Context(), keys.AdminKeyID.String(), resp.SandboxID)
		assert.NoError(t, err)
		assert.Equal(t, image, sbx.GetImage())
	}
}

func TestCreateSandbox(t *testing.T) {
	controller, client, teardown := Setup(t)
	defer teardown()
	templateName := "test-template"
	tests := []struct {
		name        string
		available   int
		userName    string
		request     models.NewSandboxRequest
		expectError *web.ApiError
		postCheck   func(t *testing.T, resp *models.Sandbox)
	}{
		{
			name:      "success",
			available: 2,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Timeout:    600,
				Metadata: map[string]string{
					"test-metadata": "test-value",
				},
				EnvVars: models.EnvVars{
					"TEST_ENV": "test-value",
				},
			},
			postCheck: imageChecker("old-image", controller),
		},
		{
			name:      "success with default timeout",
			available: 2,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					"test-key": "test-value",
				},
			},
		},
		{
			name:      "success with minimum timeout",
			available: 2,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Timeout:    30,
			},
		},
		{
			name:      "success with maximum timeout",
			available: 2,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Timeout:    7200,
			},
		},
		{
			name:      "fail with timeout too small",
			available: 2,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Timeout:    29,
			},
			expectError: &web.ApiError{
				Code:    400,
				Message: "timeout should between 30 and 2592000",
			},
		},
		{
			name:      "fail with timeout too large",
			available: 2,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Timeout:    2592001,
			},
			expectError: &web.ApiError{
				Code:    400,
				Message: "timeout should between 30 and 2592000",
			},
		},
		{
			name:      "fail with unqualified metadata key",
			available: 2,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					"invalid@key": "test-value",
				},
			},
			expectError: &web.ApiError{
				Code:    400,
				Message: "Unqualified metadata key [invalid@key]: name part must consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character (e.g. 'MyName',  or 'my.name',  or '123-abc', regex used for validation is '([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]')",
			},
		},
		{
			name:      "fail with forbidden metadata key",
			available: 2,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					v1alpha1.E2BPrefix + "key": "test-value",
				},
			},
			expectError: &web.ApiError{
				Code:    400,
				Message: "Forbidden metadata key [e2b.agents.kruise.io/key]: cannot contain prefixes: [e2b.agents.kruise.io/ agents.kruise.io/]",
			},
		},
		{
			name:      "fail without user",
			available: 2,
			userName:  "",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
			},
			expectError: &web.ApiError{
				Code:    401,
				Message: "User is empty",
			},
		},
		{
			name:      "fail with no available sandboxes",
			available: 0,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
			},
			expectError: &web.ApiError{
				Code:    0,
				Message: "Internal: failed to claim sandbox: no available sandboxes for template test-template (no stock)",
			},
		},
		{
			name:      "claim with image",
			available: 1,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					models.ExtensionKeyClaimWithImage: "new-image",
				},
			},
			postCheck: imageChecker("new-image", controller),
		},
		{
			name:      "claim with bad image",
			available: 1,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					models.ExtensionKeyClaimWithImage: "bad-@@-image",
				},
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: "Bad extension param: invalid image [bad-@@-image]: invalid reference format",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var user *models.CreatedTeamAPIKey
			if tt.userName != "" {
				user = &models.CreatedTeamAPIKey{
					ID:   keys.AdminKeyID,
					Key:  InitKey,
					Name: tt.userName,
				}
			}
			cleanup := CreateSandboxPool(t, client.SandboxClient, templateName, tt.available)
			defer cleanup()
			now := time.Now()
			resp, apiError := controller.CreateSandbox(NewRequest(t, nil, tt.request, nil, user))
			if tt.expectError != nil {
				assert.NotNil(t, apiError)
				if apiError != nil {
					assert.Equal(t, tt.expectError.Code, apiError.Code)
					assert.Equal(t, tt.expectError.Message, apiError.Message)
				}
			} else {
				assert.Nil(t, apiError)
				sbx := resp.Body
				assert.True(t, strings.HasPrefix(sbx.SandboxID, fmt.Sprintf("%s--%s-", Namespace, templateName)))
				for k, v := range tt.request.Metadata {
					if !ValidateMetadataKey(k) {
						continue
					}
					assert.Equal(t, v, sbx.Metadata[k], fmt.Sprintf("metadata key: %s", k))
				}
				startedAt, err := time.Parse(time.RFC3339, sbx.StartedAt)
				assert.NoError(t, err)
				assert.WithinDuration(t, now, startedAt, 5*time.Second)
				timeout := 300
				if tt.request.Timeout != 0 {
					timeout = tt.request.Timeout
				}
				endAt, err := time.Parse(time.RFC3339, sbx.EndAt)
				assert.NoError(t, err)
				assert.WithinDuration(t, startedAt.Add(time.Duration(timeout)*time.Second), endAt, 5*time.Second)
				assert.Equal(t, models.SandboxStateRunning, sbx.State)
			}
		})
	}
}

func TestAutoPause(t *testing.T) {
	controller, client, teardown := Setup(t)
	defer teardown()
	timeout := 300
	now := time.Now()
	timeoutTime := now.Add(time.Duration(timeout) * time.Second)
	maxTimeoutTime := now.Add(time.Duration(models.DefaultMaxTimeout) * time.Second)
	templateName := "auto-pause"
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "test-user",
	}
	tests := []struct {
		name          string
		autoPause     bool
		createChecker func(t *testing.T, sbx *v1alpha1.Sandbox)
		pauseChecker  func(t *testing.T, sbx *v1alpha1.Sandbox)
		resumeChecker func(t *testing.T, sbx *v1alpha1.Sandbox)
	}{
		{
			name:      "autoPause == false",
			autoPause: false,
			createChecker: func(t *testing.T, sbx *v1alpha1.Sandbox) {
				assert.Nil(t, sbx.Spec.PauseTime)
				assert.NotNil(t, sbx.Spec.ShutdownTime)
				if sbx.Spec.ShutdownTime != nil {
					assert.WithinDuration(t, sbx.Spec.ShutdownTime.Time, timeoutTime, 5*time.Second)
				}
			},
			pauseChecker: func(t *testing.T, sbx *v1alpha1.Sandbox) {
				assert.Nil(t, sbx.Spec.PauseTime)
				assert.NotNil(t, sbx.Spec.ShutdownTime)
				if sbx.Spec.ShutdownTime != nil {
					assert.WithinDuration(t, sbx.Spec.ShutdownTime.Time, maxTimeoutTime, 5*time.Second)
				}
			},
			resumeChecker: func(t *testing.T, sbx *v1alpha1.Sandbox) {
				assert.Nil(t, sbx.Spec.PauseTime)
				assert.NotNil(t, sbx.Spec.ShutdownTime)
				if sbx.Spec.ShutdownTime != nil {
					assert.WithinDuration(t, sbx.Spec.ShutdownTime.Time, timeoutTime, 5*time.Second)
				}
			},
		},
		{
			name:      "autoPause == true",
			autoPause: true,
			createChecker: func(t *testing.T, sbx *v1alpha1.Sandbox) {
				assert.NotNil(t, sbx.Spec.PauseTime)
				if sbx.Spec.PauseTime != nil {
					assert.WithinDuration(t, sbx.Spec.PauseTime.Time, timeoutTime, 5*time.Second)
				}
				assert.NotNil(t, sbx.Spec.ShutdownTime)
				if sbx.Spec.ShutdownTime != nil {
					assert.WithinDuration(t, sbx.Spec.ShutdownTime.Time, maxTimeoutTime, 5*time.Second)
				}
			},
			pauseChecker: func(t *testing.T, sbx *v1alpha1.Sandbox) {
				assert.NotNil(t, sbx.Spec.PauseTime)
				if sbx.Spec.PauseTime != nil {
					assert.WithinDuration(t, sbx.Spec.PauseTime.Time, maxTimeoutTime, 5*time.Second)
				}
				assert.NotNil(t, sbx.Spec.ShutdownTime)
				if sbx.Spec.ShutdownTime != nil {
					assert.WithinDuration(t, sbx.Spec.ShutdownTime.Time, maxTimeoutTime, 5*time.Second)
				}
			},
			resumeChecker: func(t *testing.T, sbx *v1alpha1.Sandbox) {
				assert.NotNil(t, sbx.Spec.PauseTime)
				if sbx.Spec.PauseTime != nil {
					assert.WithinDuration(t, sbx.Spec.PauseTime.Time, timeoutTime, 5*time.Second)
				}
				assert.NotNil(t, sbx.Spec.ShutdownTime)
				if sbx.Spec.ShutdownTime != nil {
					assert.WithinDuration(t, sbx.Spec.ShutdownTime.Time, maxTimeoutTime, 5*time.Second)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanup := CreateSandboxPool(t, client.SandboxClient, templateName, 1)
			defer cleanup()

			createResp, apiError := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				TemplateID: templateName,
				AutoPause:  tt.autoPause,
				Timeout:    timeout,
			}, nil, user))
			assert.Nil(t, apiError)
			AssertEndAt(t, timeoutTime, createResp.Body.EndAt)
			tt.createChecker(t, GetSandbox(t, createResp.Body.SandboxID, client.SandboxClient))
			AvoidGetFromCache(t, createResp.Body.SandboxID, client.SandboxClient)

			_, apiError = controller.PauseSandbox(NewRequest(t, nil, nil, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user))
			assert.Nil(t, apiError)
			UpdateSandboxWhen(t, client.SandboxClient, createResp.Body.SandboxID, func(sbx *v1alpha1.Sandbox) bool {
				return sbx.Spec.Paused == true
			}, DoSetSandboxStatus(v1alpha1.SandboxPaused, metav1.ConditionTrue, metav1.ConditionFalse))
			describeResp, apiError := controller.DescribeSandbox(NewRequest(t, nil, nil, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user))
			assert.Nil(t, apiError)
			AssertEndAt(t, maxTimeoutTime, describeResp.Body.EndAt)
			tt.pauseChecker(t, GetSandbox(t, createResp.Body.SandboxID, client.SandboxClient))
			go UpdateSandboxWhen(t, client.SandboxClient, createResp.Body.SandboxID, func(sbx *v1alpha1.Sandbox) bool {
				return sbx.Spec.Paused == false
			}, DoSetSandboxStatus(v1alpha1.SandboxRunning, metav1.ConditionFalse, metav1.ConditionTrue))
			connectResp, apiError := controller.ConnectSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
				TimeoutSeconds: timeout,
			}, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user))
			assert.Nil(t, apiError)
			AssertEndAt(t, timeoutTime, connectResp.Body.EndAt)
			tt.resumeChecker(t, GetSandbox(t, createResp.Body.SandboxID, client.SandboxClient))
		})
	}
}
