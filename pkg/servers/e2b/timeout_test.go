package e2b

import (
	"net/http"
	"testing"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSetTimeout(t *testing.T) {
	controller, client, teardown := Setup(t)
	defer teardown()
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}
	templateName := "test"

	start := time.Now()

	tests := []struct {
		name         string
		autoPause    bool
		phase        v1alpha1.SandboxPhase
		timeout      int
		expectStatus int
		checker      func(t *testing.T, sbx *v1alpha1.Sandbox, timeout time.Duration)
	}{
		{
			name:    "default",
			phase:   v1alpha1.SandboxRunning,
			timeout: 30,
			checker: func(t *testing.T, sbx *v1alpha1.Sandbox, timeout time.Duration) {
				assert.WithinDuration(t, start.Add(timeout), sbx.Spec.ShutdownTime.Time, 2*time.Second)
				assert.Nil(t, sbx.Spec.PauseTime)
			},
		},
		{
			name:         "default, too small",
			phase:        v1alpha1.SandboxRunning,
			timeout:      -1,
			expectStatus: http.StatusInternalServerError,
		},
		{
			name:         "default, too big",
			phase:        v1alpha1.SandboxRunning,
			timeout:      models.DefaultMaxTimeout + 1,
			expectStatus: http.StatusInternalServerError,
		},
		{
			name:      "auto pause",
			phase:     v1alpha1.SandboxRunning,
			autoPause: true,
			timeout:   30,
			checker: func(t *testing.T, sbx *v1alpha1.Sandbox, timeout time.Duration) {
				assert.WithinDuration(t, start.Add(time.Duration(models.DefaultMaxTimeout)*time.Second), sbx.Spec.ShutdownTime.Time, 2*time.Second)
				assert.WithinDuration(t, start.Add(timeout), sbx.Spec.PauseTime.Time, 2*time.Second)
			},
		},
		{
			name:         "not running",
			phase:        v1alpha1.SandboxPaused,
			timeout:      30,
			expectStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanup := CreateSandboxPool(t, client.SandboxClient, templateName, 1)
			defer cleanup()
			initTimeoutSeconds := 30
			initEndAt := start.Add(time.Duration(initTimeoutSeconds) * time.Second)

			createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				TemplateID: templateName,
				AutoPause:  tt.autoPause,
				Timeout:    initTimeoutSeconds,
			}, nil, user))
			assert.Nil(t, err)
			assert.Equal(t, models.SandboxStateRunning, createResp.Body.State)
			AssertEndAt(t, initEndAt, createResp.Body.EndAt)
			AvoidGetFromCache(t, createResp.Body.SandboxID, client.SandboxClient)

			UpdateSandboxWhen(t, client.SandboxClient, createResp.Body.SandboxID, Immediately,
				DoSetSandboxStatus(tt.phase, metav1.ConditionFalse, metav1.ConditionTrue))

			_, apiError := controller.SetSandboxTimeout(NewRequest(t, nil, models.SetTimeoutRequest{
				TimeoutSeconds: tt.timeout,
			}, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user))

			if tt.expectStatus != 0 {
				assert.NotNil(t, apiError)
				if apiError != nil {
					assert.Equal(t, tt.expectStatus, apiError.Code)
				}
			} else {
				assert.Nil(t, apiError)
				describeResp, err := controller.DescribeSandbox(NewRequest(t, nil, nil, map[string]string{
					"sandboxID": createResp.Body.SandboxID,
				}, user))
				assert.Nil(t, err)
				timeoutDuration := time.Duration(tt.timeout) * time.Second
				AssertEndAt(t, start.Add(timeoutDuration), describeResp.Body.EndAt)
				tt.checker(t, GetSandbox(t, createResp.Body.SandboxID, client.SandboxClient), timeoutDuration)
			}
		})
	}
}
