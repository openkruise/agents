package e2b

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/stretchr/testify/assert"
)

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
					InternalPrefix + "key": "test-value",
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
					assert.Equal(t, v, sbx.Metadata[k], fmt.Sprintf("metadata key: %s", k))
				}
				startedAt, err := time.Parse(time.RFC3339, sbx.StartedAt)
				assert.NoError(t, err)
				AssertTimeAlmostEqual(t, now, startedAt)
				timeout := 300
				if tt.request.Timeout != 0 {
					timeout = tt.request.Timeout
				}
				endAt, err := time.Parse(time.RFC3339, sbx.EndAt)
				assert.NoError(t, err)
				AssertTimeAlmostEqual(t, startedAt.Add(time.Duration(timeout)*time.Second), endAt)
				assert.Equal(t, models.SandboxStateRunning, sbx.State)
			}
		})
	}
}
