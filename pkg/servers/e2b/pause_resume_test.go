package e2b

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestPauseSandbox(t *testing.T) {
	templateName := "test-template"
	controller, fc, teardown := Setup(t)
	defer teardown()
	cleanup := CreateSandboxPool(t, controller, templateName, 10)
	defer cleanup()
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}
	createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
		TemplateID: templateName,
		Metadata: map[string]string{
			models.ExtensionKeySkipInitRuntime: agentsv1alpha1.True,
		},
	}, nil, user))
	require.Nil(t, err)
	assert.Equal(t, models.SandboxStateRunning, createResp.Body.State)

	EnableWaitSim(t, controller, createResp.Body.SandboxID)

	req := NewRequest(t, nil, nil, map[string]string{
		"sandboxID": createResp.Body.SandboxID,
	}, user)
	// pause first time
	go UpdateSandboxWhen(t, fc, createResp.Body.SandboxID, func(sbx *agentsv1alpha1.Sandbox) bool {
		return sbx.Spec.Paused == true
	}, DoSetSandboxStatus(agentsv1alpha1.SandboxPaused, metav1.ConditionTrue, metav1.ConditionFalse))
	pauseResp, err := controller.PauseSandbox(req)
	assert.Nil(t, err)
	describeResp, err := controller.DescribeSandbox(req)
	assert.Nil(t, err)
	assert.Equal(t, http.StatusNoContent, pauseResp.Code)
	assert.Equal(t, models.SandboxStatePaused, describeResp.Body.State)

	// pause again
	start := time.Now()
	pauseResp, err = controller.PauseSandbox(req)
	assert.NotNil(t, err)
	if err != nil {
		assert.Equal(t, http.StatusConflict, err.Code)
	}
	describeResp, err = controller.DescribeSandbox(req)
	assert.Nil(t, err)
	assert.Equal(t, models.SandboxStatePaused, describeResp.Body.State)
	endAt, parseErr := time.Parse(time.RFC3339, describeResp.Body.EndAt)
	assert.NoError(t, parseErr)
	expectEndAt := start.AddDate(1000, 0, 0)
	assert.WithinDuration(t, expectEndAt, endAt, 5*time.Second, "expect end at: %s, but got %s", expectEndAt, endAt)
}

func pauseSandboxHelper(t *testing.T, controller *Controller, client client.Client, sandboxID string, pausing, resuming bool, user *models.CreatedTeamAPIKey) {
	req := NewRequest(t, nil, nil, map[string]string{
		"sandboxID": sandboxID,
	}, user)
	// First, make the sandbox paused
	go UpdateSandboxWhen(t, client, sandboxID, func(sbx *agentsv1alpha1.Sandbox) bool {
		return sbx.Spec.Paused == true
	}, DoSetSandboxStatus(agentsv1alpha1.SandboxPaused, metav1.ConditionTrue, metav1.ConditionFalse))
	pauseResp, err := controller.PauseSandbox(req)
	require.Nil(t, err)
	require.Equal(t, http.StatusNoContent, pauseResp.Code)
	describeResp, err := controller.DescribeSandbox(req)
	require.Nil(t, err)
	require.Equal(t, models.SandboxStatePaused, describeResp.Body.State)
	// If pausing, modify it again
	if pausing {
		UpdateSandboxWhen(t, client, sandboxID, func(sbx *agentsv1alpha1.Sandbox) bool {
			return sbx.Spec.Paused == true
		}, DoSetSandboxStatus(agentsv1alpha1.SandboxPaused, metav1.ConditionFalse, metav1.ConditionFalse))
	} else if resuming {
		// Set resuming state: Spec.Paused=false, Phase=Running, Ready=false
		// This means sandbox is transitioning from paused to running
		sbx := GetSandbox(t, sandboxID, client)
		sbx.Spec.Paused = false
		require.NoError(t, client.Update(t.Context(), sbx))
		// Update status to reflect resuming state
		UpdateSandboxWhen(t, client, sandboxID, func(sbx *agentsv1alpha1.Sandbox) bool {
			return sbx.Spec.Paused == false
		}, DoSetSandboxStatus(agentsv1alpha1.SandboxPaused, metav1.ConditionFalse, metav1.ConditionFalse))
	}
}

func TestConnectSandbox(t *testing.T) {
	tests := []struct {
		name         string
		paused       bool   // if sandbox is set paused
		pausing      bool   // if sandbox is performing pausing (Paused condition is false)
		resuming     bool   // if sandbox is performing resuming (Ready condition is false)
		sandboxID    string // if not set, use the created sandbox ID
		timeout      int
		expectStatus int
	}{
		{
			name:         "running sandbox",
			paused:       false,
			timeout:      300,
			expectStatus: http.StatusOK,
		},
		{
			name:         "resume sandbox: paused",
			paused:       true,
			pausing:      false,
			timeout:      300,
			expectStatus: http.StatusCreated,
		},
		{
			name:         "resume sandbox: pausing",
			paused:       true,
			pausing:      true,
			timeout:      300,
			expectStatus: http.StatusBadRequest,
		},
		{
			name:         "resume sandbox: resuming",
			paused:       true,
			pausing:      false,
			resuming:     true,
			timeout:      300,
			expectStatus: http.StatusCreated,
		},
		{
			name:         "not found",
			paused:       false,
			sandboxID:    "not-exist",
			timeout:      300,
			expectStatus: http.StatusNotFound,
		},
		{
			name:         "bad request",
			paused:       false,
			timeout:      -1,
			expectStatus: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			templateName := "test-template"
			controller, fc, teardown := Setup(t)
			defer teardown()
			user := &models.CreatedTeamAPIKey{
				ID:   keys.AdminKeyID,
				Key:  InitKey,
				Name: "admin",
			}

			cleanup := CreateSandboxPool(t, controller, templateName, 1)
			defer cleanup()

			createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				Metadata: map[string]string{
					models.ExtensionKeySkipInitRuntime: agentsv1alpha1.True,
				},
				TemplateID: templateName,
			}, nil, user))
			require.Nil(t, err)
			assert.Equal(t, models.SandboxStateRunning, createResp.Body.State)

			EnableWaitSim(t, controller, createResp.Body.SandboxID)

			if tt.paused {
				pauseSandboxHelper(t, controller, fc, createResp.Body.SandboxID, tt.pausing, tt.resuming, user)
			}

			if tt.sandboxID == "" {
				tt.sandboxID = createResp.Body.SandboxID
			}
			if tt.expectStatus < 300 {
				go UpdateSandboxWhen(t, fc, createResp.Body.SandboxID, func(sbx *agentsv1alpha1.Sandbox) bool {
					return sbx.Spec.Paused == false
				}, DoSetSandboxStatus(agentsv1alpha1.SandboxRunning, metav1.ConditionFalse, metav1.ConditionTrue))
			}
			now := time.Now()
			connectResp, err := controller.ConnectSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
				TimeoutSeconds: tt.timeout,
			}, map[string]string{
				"sandboxID": tt.sandboxID,
			}, user))

			if tt.expectStatus >= 300 {
				require.NotNil(t, err, fmt.Sprintf("%v", err))
				if err.Code == 0 {
					err.Code = http.StatusInternalServerError
				}
				assert.Equal(t, tt.expectStatus, err.Code)
			} else {
				require.Nil(t, err, fmt.Sprintf("err: %v", err))
				assert.Equal(t, tt.expectStatus, connectResp.Code)
				assert.Equal(t, models.SandboxStateRunning, connectResp.Body.State)
				endAt, err := time.Parse(time.RFC3339, connectResp.Body.EndAt)
				require.NoError(t, err)
				expectEndAt := now.Add(time.Duration(tt.timeout) * time.Second)
				assert.WithinDuration(t, expectEndAt, endAt, 5*time.Second,
					fmt.Sprintf("expect end at: %s, but got %s", expectEndAt, endAt))
			}
		})
	}
}

func TestResumeSandbox(t *testing.T) {
	tests := []struct {
		name         string
		paused       bool   // if sandbox is set paused
		pausing      bool   // if sandbox is performing pausing
		resuming     bool   // if sandbox is performing resuming (Ready condition is false)
		sandboxID    string // if not set, use the created sandbox ID
		timeout      int
		expectStatus int
	}{
		{
			name:         "running sandbox",
			paused:       false,
			timeout:      300,
			expectStatus: http.StatusConflict,
		},
		{
			name:         "resume sandbox: paused",
			paused:       true,
			pausing:      false,
			resuming:     false,
			timeout:      300,
			expectStatus: http.StatusNoContent,
		},
		{
			name:         "resume sandbox: pausing",
			paused:       true,
			pausing:      true,
			timeout:      300,
			expectStatus: http.StatusBadRequest,
		},
		{
			name:         "resume sandbox: resuming",
			paused:       true,
			pausing:      false,
			resuming:     true,
			timeout:      300,
			expectStatus: http.StatusNoContent,
		},
		{
			name:         "not found",
			paused:       false,
			sandboxID:    "not-exist",
			timeout:      300,
			expectStatus: http.StatusNotFound,
		},
		{
			name:         "bad request",
			paused:       false,
			timeout:      -1,
			expectStatus: http.StatusInternalServerError, // E2B returns 500 for bad timeout
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			templateName := "test-template"
			controller, fc, teardown := Setup(t)
			defer teardown()
			user := &models.CreatedTeamAPIKey{
				ID:   keys.AdminKeyID,
				Key:  InitKey,
				Name: "admin",
			}

			cleanup := CreateSandboxPool(t, controller, templateName, 1)
			defer cleanup()

			createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				Metadata: map[string]string{
					models.ExtensionKeySkipInitRuntime: agentsv1alpha1.True,
				},
				TemplateID: templateName,
			}, nil, user))
			require.Nil(t, err)
			assert.Equal(t, models.SandboxStateRunning, createResp.Body.State)

			EnableWaitSim(t, controller, createResp.Body.SandboxID)

			if tt.paused {
				pauseSandboxHelper(t, controller, fc, createResp.Body.SandboxID, tt.pausing, tt.resuming, user)
			}

			if tt.sandboxID == "" {
				tt.sandboxID = createResp.Body.SandboxID
			}
			if tt.expectStatus < 300 {
				// Only schedule async update when expecting success
				go UpdateSandboxWhen(t, fc, createResp.Body.SandboxID, func(sbx *agentsv1alpha1.Sandbox) bool {
					return sbx.Spec.Paused == false
				}, DoSetSandboxStatus(agentsv1alpha1.SandboxRunning, metav1.ConditionFalse, metav1.ConditionTrue))
			}
			now := time.Now()
			resumeResp, apiErr := controller.ResumeSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
				TimeoutSeconds: tt.timeout,
			}, map[string]string{
				"sandboxID": tt.sandboxID,
			}, user))

			if tt.expectStatus >= 300 {
				require.NotNil(t, apiErr, fmt.Sprintf("%v", apiErr))
				if apiErr.Code == 0 {
					apiErr.Code = http.StatusInternalServerError
				}
				assert.Equal(t, tt.expectStatus, apiErr.Code)
			} else {
				require.Nil(t, apiErr, fmt.Sprintf("err: %v", apiErr))
				assert.Equal(t, tt.expectStatus, resumeResp.Code)
				// Use DescribeSandbox to verify final state since ResumeSandbox returns empty body
				describeResp, describeErr := controller.DescribeSandbox(NewRequest(t, nil, nil, map[string]string{
					"sandboxID": tt.sandboxID,
				}, user))
				require.Nil(t, describeErr)
				assert.Equal(t, models.SandboxStateRunning, describeResp.Body.State)
				endAt, parseErr := time.Parse(time.RFC3339, describeResp.Body.EndAt)
				require.NoError(t, parseErr)
				expectEndAt := now.Add(time.Duration(tt.timeout) * time.Second)
				assert.WithinDuration(t, expectEndAt, endAt, 5*time.Second,
					fmt.Sprintf("expect end at: %s, but got %s", expectEndAt, endAt))
			}
		})
	}
}
