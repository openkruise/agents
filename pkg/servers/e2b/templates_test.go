package e2b

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDeleteTemplate(t *testing.T) {
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}

	tests := []struct {
		name               string
		templateID         string
		setupTemplate      bool                      // whether to create checkpoint + template using CreateCheckpointAndTemplate
		mockDeleteTemplate error                     // mock error for DefaultDeleteSandboxTemplate
		user               *models.CreatedTeamAPIKey // user for the request
		expectStatus       int
		expectError        bool
	}{
		{
			name:          "delete template successfully",
			templateID:    "test-tmpl-delete-success",
			setupTemplate: true,
			user:          user,
			expectStatus:  http.StatusNoContent,
		},
		{
			name:               "delete template with infra error",
			templateID:         "test-tmpl-delete-error",
			setupTemplate:      true,
			mockDeleteTemplate: fmt.Errorf("mock delete template error"),
			user:               user,
			expectStatus:       http.StatusInternalServerError,
			expectError:        true,
		},
		{
			name:          "user is nil returns unauthorized",
			templateID:    "test-tmpl-no-user",
			setupTemplate: false,
			user:          nil,
			expectStatus:  http.StatusUnauthorized,
			expectError:   true,
		},
		{
			name:          "non-owner user returns 204 (idempotent)",
			templateID:    "test-tmpl-non-owner",
			setupTemplate: true,
			user: &models.CreatedTeamAPIKey{
				ID:   uuid.New(),
				Key:  "different-key",
				Name: "different-user",
			},
			expectStatus: http.StatusNoContent,
		},
		{
			name:          "template not found",
			templateID:    "test-tmpl-not-found",
			setupTemplate: false,
			user:          user,
			expectStatus:  http.StatusNoContent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, _, teardown := Setup(t)
			defer teardown()

			if tt.setupTemplate {
				_ = CreateCheckpointAndTemplate(t, controller, tt.templateID)
				// Set owner annotation on the checkpoint
				cp, err := controller.client.SandboxClient.ApiV1alpha1().Checkpoints(Namespace).Get(t.Context(), tt.templateID, metav1.GetOptions{})
				require.NoError(t, err)
				if cp.Annotations == nil {
					cp.Annotations = map[string]string{}
				}
				cp.Annotations[v1alpha1.AnnotationOwner] = user.ID.String()
				_, err = controller.client.SandboxClient.ApiV1alpha1().Checkpoints(Namespace).Update(t.Context(), cp, metav1.UpdateOptions{})
				require.NoError(t, err)
				time.Sleep(50 * time.Millisecond) // wait for cache sync
			}

			// Set up decorator mock for template deletion
			if tt.mockDeleteTemplate != nil {
				orig := sandboxcr.DefaultDeleteSandboxTemplate
				sandboxcr.DefaultDeleteSandboxTemplate = func(ctx context.Context, c *clients.ClientSet, namespace, name string) error {
					return tt.mockDeleteTemplate
				}
				t.Cleanup(func() { sandboxcr.DefaultDeleteSandboxTemplate = orig })
			}

			req := NewRequest(t, nil, nil, map[string]string{
				"templateID": tt.templateID,
			}, tt.user)

			resp, apiErr := controller.DeleteTemplate(req)

			if tt.expectError {
				require.NotNil(t, apiErr)
				if apiErr.Code == 0 {
					apiErr.Code = http.StatusInternalServerError
				}
				assert.Equal(t, tt.expectStatus, apiErr.Code)
			} else {
				require.Nil(t, apiErr)
				assert.Equal(t, tt.expectStatus, resp.Code)
			}
		})
	}
}
