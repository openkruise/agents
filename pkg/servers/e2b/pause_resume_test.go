package e2b

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPauseSandbox(t *testing.T) {
	templateName := "test-template"
	controller, client, teardown := Setup(t)
	defer teardown()
	cleanup := CreateSandboxPool(t, client.SandboxClient, templateName, 10)
	defer cleanup()
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}
	createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
		TemplateID: templateName,
	}, nil, user))
	assert.Nil(t, err)
	assert.Equal(t, models.SandboxStateRunning, createResp.Body.State)

	req := NewRequest(t, nil, nil, map[string]string{
		"sandboxID": createResp.Body.SandboxID,
	}, user)
	// pause first time
	pauseResp, err := controller.PauseSandbox(req)
	assert.Nil(t, err)
	AvoidGetFromCache(t, createResp.Body.SandboxID, client.SandboxClient)
	describeResp, err := controller.DescribeSandbox(req)
	assert.Nil(t, err)
	assert.Equal(t, http.StatusNoContent, pauseResp.Code)
	assert.Equal(t, models.SandboxStatePaused, describeResp.Body.State)

	// pause again
	pauseResp, err = controller.PauseSandbox(req)
	assert.NotNil(t, err)
	if err != nil {
		assert.Equal(t, http.StatusConflict, err.Code)
	}
	describeResp, err = controller.DescribeSandbox(req)
	assert.Nil(t, err)
	assert.Equal(t, models.SandboxStatePaused, describeResp.Body.State)
}

func TestConnectSandbox(t *testing.T) {
	templateName := "test-template"
	controller, client, teardown := Setup(t)
	defer teardown()
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}

	tests := []struct {
		name         string
		paused       bool   // if sandbox is set paused
		pausing      bool   // if sandbox is performing pausing
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
			expectStatus: http.StatusInternalServerError,
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
			cleanup := CreateSandboxPool(t, client.SandboxClient, templateName, 1)
			defer cleanup()

			createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				TemplateID: templateName,
			}, nil, user))
			assert.Nil(t, err)
			assert.Equal(t, models.SandboxStateRunning, createResp.Body.State)

			req := NewRequest(t, nil, nil, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user)

			if tt.paused {
				pauseResp, err := controller.PauseSandbox(req)
				assert.Nil(t, err)
				AvoidGetFromCache(t, createResp.Body.SandboxID, client.SandboxClient)
				assert.Equal(t, http.StatusNoContent, pauseResp.Code)
				describeResp, err := controller.DescribeSandbox(req)
				assert.Nil(t, err)
				assert.Equal(t, models.SandboxStatePaused, describeResp.Body.State)
				status := metav1.ConditionTrue
				if tt.pausing {
					status = metav1.ConditionFalse
				}
				sbx := GetSandbox(t, createResp.Body.SandboxID, client.SandboxClient)
				sbx.Status.Phase = agentsv1alpha1.SandboxPaused
				sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
					Type:   string(agentsv1alpha1.SandboxConditionPaused),
					Status: status,
				})
				_, err2 := client.ApiV1alpha1().Sandboxes(sbx.Namespace).UpdateStatus(context.Background(), sbx, metav1.UpdateOptions{})
				assert.NoError(t, err2)
				time.AfterFunc(60*time.Millisecond, func() {
					sbx := GetSandbox(t, createResp.Body.SandboxID, client.SandboxClient)
					sbx.Status.Phase = agentsv1alpha1.SandboxRunning
					sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
						Type:   string(agentsv1alpha1.SandboxConditionReady),
						Status: metav1.ConditionTrue,
					})
					client.ApiV1alpha1().Sandboxes(sbx.Namespace).UpdateStatus(context.Background(), sbx, metav1.UpdateOptions{})
				})
			}

			if tt.sandboxID == "" {
				tt.sandboxID = createResp.Body.SandboxID
			}
			time.Sleep(10 * time.Millisecond)
			now := time.Now()
			resumeResp, err := controller.ConnectSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
				TimeoutSeconds: tt.timeout,
			}, map[string]string{
				"sandboxID": tt.sandboxID,
			}, user))

			if tt.expectStatus >= 300 {
				assert.NotNil(t, err)
				if err != nil {
					if err.Code == 0 {
						err.Code = http.StatusInternalServerError
					}
					assert.Equal(t, tt.expectStatus, err.Code)
				}
			} else {
				assert.Nil(t, err, fmt.Sprintf("err: %v", err))
				assert.Equal(t, tt.expectStatus, resumeResp.Code)
				assert.Equal(t, models.SandboxStateRunning, resumeResp.Body.State)
				endAt, err := time.Parse(time.RFC3339, resumeResp.Body.EndAt)
				assert.NoError(t, err)
				AssertTimeAlmostEqual(t, now.Add(time.Duration(tt.timeout)*time.Second), endAt)
			}
		})
	}
}

func TestResumeSandbox(t *testing.T) {
	templateName := "test-template"
	controller, client, teardown := Setup(t)
	defer teardown()
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}

	tests := []struct {
		name         string
		paused       bool   // if sandbox is set paused
		pausing      bool   // if sandbox is performing pausing
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
			timeout:      300,
			expectStatus: http.StatusNoContent,
		},
		{
			name:         "resume sandbox: pausing",
			paused:       true,
			pausing:      true,
			timeout:      300,
			expectStatus: http.StatusInternalServerError,
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
			expectStatus: http.StatusInternalServerError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanup := CreateSandboxPool(t, client.SandboxClient, templateName, 1)
			defer cleanup()

			createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				TemplateID: templateName,
			}, nil, user))
			assert.Nil(t, err)
			assert.Equal(t, models.SandboxStateRunning, createResp.Body.State)
			AvoidGetFromCache(t, createResp.Body.SandboxID, client.SandboxClient)

			req := NewRequest(t, nil, nil, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user)

			if tt.paused {
				pauseResp, err := controller.PauseSandbox(req)
				assert.Nil(t, err)
				assert.Equal(t, http.StatusNoContent, pauseResp.Code)
				describeResp, err := controller.DescribeSandbox(req)
				assert.Nil(t, err)
				assert.Equal(t, models.SandboxStatePaused, describeResp.Body.State)
				status := metav1.ConditionTrue
				if tt.pausing {
					status = metav1.ConditionFalse
				}
				sbx := GetSandbox(t, createResp.Body.SandboxID, client.SandboxClient)
				sbx.Status.Phase = agentsv1alpha1.SandboxPaused
				sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
					Type:   string(agentsv1alpha1.SandboxConditionPaused),
					Status: status,
				})
				_, err2 := client.ApiV1alpha1().Sandboxes(sbx.Namespace).UpdateStatus(context.Background(), sbx, metav1.UpdateOptions{})
				assert.NoError(t, err2)
				time.AfterFunc(60*time.Millisecond, func() {
					sbx := GetSandbox(t, createResp.Body.SandboxID, client.SandboxClient)
					sbx.Status.Phase = agentsv1alpha1.SandboxRunning
					sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
						Type:   string(agentsv1alpha1.SandboxConditionReady),
						Status: metav1.ConditionTrue,
					})
					client.ApiV1alpha1().Sandboxes(sbx.Namespace).UpdateStatus(context.Background(), sbx, metav1.UpdateOptions{})
				})
			}

			if tt.sandboxID == "" {
				tt.sandboxID = createResp.Body.SandboxID
			}
			time.Sleep(10 * time.Millisecond)
			now := time.Now()
			resumeResp, err := controller.ResumeSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
				TimeoutSeconds: tt.timeout,
			}, map[string]string{
				"sandboxID": tt.sandboxID,
			}, user))

			if tt.expectStatus >= 300 {
				assert.NotNil(t, err)
				if err != nil {
					if err.Code == 0 {
						err.Code = http.StatusInternalServerError
					}
					assert.Equal(t, tt.expectStatus, err.Code)
				}
			} else {
				assert.Nil(t, err, fmt.Sprintf("err: %v", err))
				assert.Equal(t, tt.expectStatus, resumeResp.Code)

				describeResp, err := controller.DescribeSandbox(req)
				assert.Nil(t, err)
				assert.Equal(t, models.SandboxStateRunning, describeResp.Body.State)
				endAt, err2 := time.Parse(time.RFC3339, describeResp.Body.EndAt)
				assert.NoError(t, err2)
				AssertTimeAlmostEqual(t, now.Add(time.Duration(tt.timeout)*time.Second), endAt)
			}
		})
	}
}
