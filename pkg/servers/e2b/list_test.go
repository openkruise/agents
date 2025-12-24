package e2b

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/stretchr/testify/assert"
)

func TestListSandboxes(t *testing.T) {
	templateName := "test-template"
	controller, client, teardown := Setup(t)
	defer teardown()
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}
	tests := []struct {
		name           string
		createRequests []models.NewSandboxRequest // use metadata key "state" to control sandbox state, default running
		queryParams    map[string]string
		expectListed   func(sandbox *models.Sandbox) bool
		expectError    *web.ApiError
	}{
		{
			name: "list by metadata",
			createRequests: []models.NewSandboxRequest{
				{
					TemplateID: templateName,
					Metadata: map[string]string{
						"testKey": "value1",
					},
				},
				{
					TemplateID: templateName,
					Metadata: map[string]string{
						"testKey": "value2",
					},
				},
			},
			queryParams: map[string]string{
				"metadata": "testKey=value1",
			},
			expectListed: func(sbx *models.Sandbox) bool {
				return sbx.Metadata["testKey"] == "value1"
			},
		},
		{
			name: "list by single state",
			createRequests: []models.NewSandboxRequest{
				{
					TemplateID: templateName,
					Metadata: map[string]string{
						"state": "paused",
					},
				},
				{
					TemplateID: templateName,
					Metadata: map[string]string{
						"state": "running",
					},
				},
			},
			queryParams: map[string]string{
				"state": "running",
			},
			expectListed: func(sbx *models.Sandbox) bool {
				return sbx.Metadata["state"] == "running"
			},
		},
		{
			name: "list by multi state",
			createRequests: []models.NewSandboxRequest{
				{
					TemplateID: templateName,
					Metadata: map[string]string{
						"state": "paused",
					},
				},
				{
					TemplateID: templateName,
					Metadata: map[string]string{
						"state": "running",
					},
				},
			},
			queryParams: map[string]string{
				"state": "running,paused",
			},
			expectListed: func(sbx *models.Sandbox) bool {
				return sbx.Metadata["state"] == "running" || sbx.Metadata["state"] == "paused"
			},
		},
		{
			name: "list by illegal state",
			createRequests: []models.NewSandboxRequest{
				{
					TemplateID: templateName,
					Metadata: map[string]string{
						"state": "paused",
					},
				},
				{
					TemplateID: templateName,
					Metadata: map[string]string{
						"state": "running",
					},
				},
			},
			queryParams: map[string]string{
				"state": "foo",
			},
			expectError: &web.ApiError{
				Code: http.StatusBadRequest,
				Message: fmt.Sprintf("Only '%s' and '%s' state are supported, not: '%s'",
					models.SandboxStateRunning, models.SandboxStatePaused, "foo"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanup := CreateSandboxPool(t, client.SandboxClient, templateName, 10)
			defer cleanup()

			var expectedListed []models.Sandbox
			var createdRequests []*http.Request
			var expectStates []string
			for _, request := range tt.createRequests {
				resp, apiError := controller.CreateSandbox(NewRequest(t, nil, request, nil, user))
				assert.Nil(t, apiError)
				sandbox := resp.Body
				expectState := "running"
				if sandbox.Metadata["state"] != "" {
					expectState = sandbox.Metadata["state"]
				}
				req := NewRequest(t, nil, nil, map[string]string{
					"sandboxID": sandbox.SandboxID,
				}, user)
				if expectState == "paused" {
					_, err := controller.PauseSandbox(req)
					assert.Nil(t, err)
				}
				createdRequests = append(createdRequests, req)
				expectStates = append(expectStates, expectState)
			}

			time.Sleep(100 * time.Millisecond)

			for i, request := range createdRequests {
				describe, err := controller.DescribeSandbox(request)
				assert.Nil(t, err)
				sandbox := describe.Body
				assert.Equal(t, expectStates[i], sandbox.State)
				sandbox.EnvdAccessToken = "" // token is not listed
				if err == nil && tt.expectListed != nil && tt.expectListed(sandbox) {
					expectedListed = append(expectedListed, *sandbox)
				}
			}

			resp, apiError := controller.ListSandboxes(NewRequest(t, tt.queryParams, nil, nil, user))
			if tt.expectError != nil {
				assert.NotNil(t, apiError)
				if apiError != nil {
					assert.Equal(t, tt.expectError.Code, apiError.Code)
					assert.Equal(t, tt.expectError.Message, apiError.Message)
				}
			} else {
				assert.Nil(t, apiError)
				list := resp.Body
				var gotListed []models.Sandbox
				for _, sandbox := range list {
					gotListed = append(gotListed, *sandbox)
				}
				assert.ElementsMatch(t, expectedListed, gotListed)
			}
		})
	}
}
