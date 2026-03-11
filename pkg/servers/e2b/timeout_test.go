package e2b

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSetSandboxTimeoutWithNeverTimeout(t *testing.T) {
	controller, client, teardown := Setup(t)
	defer teardown()
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}
	templateName := "test-never-timeout"

	cleanup := CreateSandboxPool(t, controller, templateName, 1)
	defer cleanup()

	// Step 1: Create sandbox with never-timeout=true
	createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
		TemplateID: templateName,
		Timeout:    300,
		Metadata: map[string]string{
			models.ExtensionKeySkipInitRuntime: v1alpha1.True,
			models.ExtensionKeyNeverTimeout:    v1alpha1.True,
		},
	}, nil, user))
	assert.Nil(t, err)
	assert.Equal(t, models.SandboxStateRunning, createResp.Body.State)

	// Step 2: Check timeout - EndAt should be zero time (never timeout)
	// When never-timeout is set, EndAt is formatted as RFC3339 zero value "0001-01-01T00:00:00Z"
	endAtTime, parseErr := time.Parse(time.RFC3339, createResp.Body.EndAt)
	assert.NoError(t, parseErr)
	require.True(t, endAtTime.IsZero(), "EndAt should be zero time for never-timeout sandbox")
	sbx := GetSandbox(t, createResp.Body.SandboxID, client.SandboxClient)
	require.Nil(t, sbx.Spec.ShutdownTime)
	require.Nil(t, sbx.Spec.PauseTime)
	AvoidGetFromCache(t, createResp.Body.SandboxID, client.SandboxClient)

	// Step 3: Call SetSandboxTimeout
	_, apiError := controller.SetSandboxTimeout(NewRequest(t, nil, models.SetTimeoutRequest{
		TimeoutSeconds: 600,
	}, map[string]string{
		"sandboxID": createResp.Body.SandboxID,
	}, user))
	assert.Nil(t, apiError)

	// Step 4: Check timeout again - should still be zero time (never timeout)
	describeResp, err := controller.DescribeSandbox(NewRequest(t, nil, nil, map[string]string{
		"sandboxID": createResp.Body.SandboxID,
	}, user))
	assert.Nil(t, err)
	endAtTime, parseErr = time.Parse(time.RFC3339, describeResp.Body.EndAt)
	assert.NoError(t, parseErr)
	require.True(t, endAtTime.IsZero(), "EndAt should still be zero time after SetSandboxTimeout for never-timeout sandbox")
	sbx = GetSandbox(t, createResp.Body.SandboxID, client.SandboxClient)
	require.Nil(t, sbx.Spec.ShutdownTime)
	require.Nil(t, sbx.Spec.PauseTime)

	// Step 5: Pause the sandbox first (required before ResumeSandbox)
	_, apiError = controller.PauseSandbox(NewRequest(t, nil, nil, map[string]string{
		"sandboxID": createResp.Body.SandboxID,
	}, user))
	assert.Nil(t, apiError)
	AvoidGetFromCache(t, createResp.Body.SandboxID, client.SandboxClient)
	// Wait for pause to complete by checking state
	describeResp, err = controller.DescribeSandbox(NewRequest(t, nil, nil, map[string]string{
		"sandboxID": createResp.Body.SandboxID,
	}, user))
	assert.Nil(t, err)
	assert.Equal(t, models.SandboxStatePaused, describeResp.Body.State)
	sbx = GetSandbox(t, createResp.Body.SandboxID, client.SandboxClient)
	require.Nil(t, sbx.Spec.ShutdownTime)
	require.Nil(t, sbx.Spec.PauseTime)

	// Step 6: Test ResumeSandbox - should also preserve never-timeout behavior
	_, apiError = controller.ResumeSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
		TimeoutSeconds: 600,
	}, map[string]string{
		"sandboxID": createResp.Body.SandboxID,
	}, user))
	assert.Nil(t, apiError)

	// Step 7: Check timeout after ResumeSandbox - should still be zero time
	describeResp, err = controller.DescribeSandbox(NewRequest(t, nil, nil, map[string]string{
		"sandboxID": createResp.Body.SandboxID,
	}, user))
	assert.Nil(t, err)
	endAtTime, parseErr = time.Parse(time.RFC3339, describeResp.Body.EndAt)
	assert.NoError(t, parseErr)
	require.True(t, endAtTime.IsZero(), "EndAt should still be zero time after ResumeSandbox for never-timeout sandbox")
	sbx = GetSandbox(t, createResp.Body.SandboxID, client.SandboxClient)
	require.Nil(t, sbx.Spec.ShutdownTime)
	require.Nil(t, sbx.Spec.PauseTime)

	// Step 8: Test ConnectSandbox on running sandbox (should skip resume and preserve never-timeout)
	// ConnectSandbox on running sandbox should also preserve never-timeout behavior
	connectResp, apiError := controller.ConnectSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
		TimeoutSeconds: 600,
	}, map[string]string{
		"sandboxID": createResp.Body.SandboxID,
	}, user))
	assert.Nil(t, apiError)
	assert.Equal(t, models.SandboxStateRunning, connectResp.Body.State)
	// Step 9: Check timeout after ConnectSandbox - should still be zero time
	endAtTime, parseErr = time.Parse(time.RFC3339, connectResp.Body.EndAt)
	assert.NoError(t, parseErr)
	require.True(t, endAtTime.IsZero(), fmt.Sprintf("EndAt should still be zero time after ConnectSandbox for never-timeout sandbox, actual: %s", endAtTime))
	sbx = GetSandbox(t, createResp.Body.SandboxID, client.SandboxClient)
	require.Nil(t, sbx.Spec.ShutdownTime)
	require.Nil(t, sbx.Spec.PauseTime)
}

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
			cleanup := CreateSandboxPool(t, controller, templateName, 1)
			defer cleanup()
			initTimeoutSeconds := 30
			initEndAt := start.Add(time.Duration(initTimeoutSeconds) * time.Second)

			createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				TemplateID: templateName,
				AutoPause:  tt.autoPause,
				Timeout:    initTimeoutSeconds,
				Metadata: map[string]string{
					models.ExtensionKeySkipInitRuntime: v1alpha1.True,
				},
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
