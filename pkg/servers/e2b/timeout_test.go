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
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
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
	sbx := GetSandbox(t, createResp.Body.SandboxID, client)
	require.Nil(t, sbx.Spec.ShutdownTime)
	require.Nil(t, sbx.Spec.PauseTime)

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
	sbx = GetSandbox(t, createResp.Body.SandboxID, client)
	require.Nil(t, sbx.Spec.ShutdownTime)
	require.Nil(t, sbx.Spec.PauseTime)

	EnableWaitSim(t, controller, createResp.Body.SandboxID)

	// Step 5: Pause the sandbox first (required before ResumeSandbox)
	pauseSandboxHelper(t, controller, client, createResp.Body.SandboxID, false, false, user)
	// Wait for pause to complete by checking state
	describeResp, err = controller.DescribeSandbox(NewRequest(t, nil, nil, map[string]string{
		"sandboxID": createResp.Body.SandboxID,
	}, user))
	assert.Nil(t, err)
	assert.Equal(t, models.SandboxStatePaused, describeResp.Body.State)
	sbx = GetSandbox(t, createResp.Body.SandboxID, client)
	require.Nil(t, sbx.Spec.ShutdownTime)
	require.Nil(t, sbx.Spec.PauseTime)

	// Step 6: Test ResumeSandbox - should also preserve never-timeout behavior
	go UpdateSandboxWhen(t, client, createResp.Body.SandboxID, func(sbx *v1alpha1.Sandbox) bool {
		return sbx.Spec.Paused == false
	}, DoSetSandboxStatus(v1alpha1.SandboxRunning, metav1.ConditionFalse, metav1.ConditionTrue))
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
	sbx = GetSandbox(t, createResp.Body.SandboxID, client)
	require.Nil(t, sbx.Spec.ShutdownTime)
	require.Nil(t, sbx.Spec.PauseTime)

	// Step 8: Test ConnectSandbox on running sandbox (should skip resume and preserve never-timeout)
	// ConnectSandbox on running sandbox should also preserve never-timeout behavior
	connectResp, apiError := controller.ConnectSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
		TimeoutSeconds: 600,
	}, map[string]string{
		"sandboxID": createResp.Body.SandboxID,
	}, user))
	require.Nil(t, apiError)
	assert.Equal(t, models.SandboxStateRunning, connectResp.Body.State)
	// Step 9: Check timeout after ConnectSandbox - should still be zero time
	endAtTime, parseErr = time.Parse(time.RFC3339, connectResp.Body.EndAt)
	assert.NoError(t, parseErr)
	require.True(t, endAtTime.IsZero(), fmt.Sprintf("EndAt should still be zero time after ConnectSandbox for never-timeout sandbox, actual: %s", endAtTime))
	sbx = GetSandbox(t, createResp.Body.SandboxID, client)
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

			UpdateSandboxWhen(t, client, createResp.Body.SandboxID, Immediately,
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
				tt.checker(t, GetSandbox(t, createResp.Body.SandboxID, client), timeoutDuration)
			}
		})
	}
}

// TestSetSandboxTimeoutStillShortensRunningSandbox locks scope: POST /timeout must still apply a shorter
// timeout for running sandboxes; the connect-only "do not shorten" guard must not affect this endpoint.
func TestSetSandboxTimeoutStillShortensRunningSandbox(t *testing.T) {
	controller, client, teardown := Setup(t)
	defer teardown()
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}
	templateName := "test-timeout-shorten-scope"

	cleanup := CreateSandboxPool(t, controller, templateName, 1)
	defer cleanup()

	initialTimeoutSeconds := 600
	createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
		TemplateID: templateName,
		Timeout:    initialTimeoutSeconds,
		Metadata: map[string]string{
			models.ExtensionKeySkipInitRuntime: v1alpha1.True,
		},
	}, nil, user))
	require.Nil(t, err)
	assert.Equal(t, models.SandboxStateRunning, createResp.Body.State)

	shorterSeconds := 300
	beforeSet := time.Now()
	_, apiError := controller.SetSandboxTimeout(NewRequest(t, nil, models.SetTimeoutRequest{
		TimeoutSeconds: shorterSeconds,
	}, map[string]string{
		"sandboxID": createResp.Body.SandboxID,
	}, user))
	assert.Nil(t, apiError)

	describeResp, err := controller.DescribeSandbox(NewRequest(t, nil, nil, map[string]string{
		"sandboxID": createResp.Body.SandboxID,
	}, user))
	assert.Nil(t, err)
	AssertEndAt(t, beforeSet.Add(time.Duration(shorterSeconds)*time.Second), describeResp.Body.EndAt)

	sbx := GetSandbox(t, createResp.Body.SandboxID, client)
	require.NotNil(t, sbx.Spec.ShutdownTime)
	assert.Nil(t, sbx.Spec.PauseTime)
	assert.WithinDuration(t, beforeSet.Add(time.Duration(shorterSeconds)*time.Second), sbx.Spec.ShutdownTime.Time, 5*time.Second)
}

func TestUpdateConnectTimeoutBaselinePolicy(t *testing.T) {
	templateName := "test-update-connect-timeout-pause-baseline"
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}

	tests := []struct {
		name               string
		currentTimeout     int
		requestTimeout     int
		changeCurrentAfter bool
		updatedTimeout     int
		expectedTimeout    int
	}{
		{
			name:            "matching baseline resets to requested timeout",
			currentTimeout:  600,
			requestTimeout:  300,
			expectedTimeout: 300,
		},
		{
			name:               "mismatched baseline uses extend only and skips shorter timeout",
			currentTimeout:     600,
			requestTimeout:     300,
			changeCurrentAfter: true,
			updatedTimeout:     900,
			expectedTimeout:    900,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, fc, teardown := Setup(t)
			defer teardown()

			cleanup := CreateSandboxPool(t, controller, templateName, 1)
			defer cleanup()

			createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				TemplateID: templateName,
				Timeout:    tt.currentTimeout,
				Metadata: map[string]string{
					models.ExtensionKeySkipInitRuntime: v1alpha1.True,
				},
			}, nil, user))
			require.Nil(t, err)

			req := NewRequest(t, nil, nil, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user)
			wrapped, apiErr := controller.getSandboxOfUser(req.Context(), createResp.Body.SandboxID)
			require.Nil(t, apiErr)
			baseline := wrapped.GetTimeout()

			if tt.changeCurrentAfter {
				sbx := GetSandbox(t, createResp.Body.SandboxID, fc)
				sbx.Spec.ShutdownTime = &metav1.Time{Time: time.Now().Add(time.Duration(tt.updatedTimeout) * time.Second)}
				require.NoError(t, fc.Update(t.Context(), sbx))
			}

			beforeCall := time.Now()
			errResp := controller.updateConnectTimeout(req.Context(), wrapped, tt.requestTimeout,
				v1alpha1.SandboxStatePaused, false, beforeCall.Add(time.Duration(tt.currentTimeout)*time.Second), baseline)
			require.Nil(t, errResp)

			updatedSbx := GetSandbox(t, createResp.Body.SandboxID, fc)
			require.NotNil(t, updatedSbx.Spec.ShutdownTime)
			assert.WithinDuration(t, beforeCall.Add(time.Duration(tt.expectedTimeout)*time.Second), updatedSbx.Spec.ShutdownTime.Time, 5*time.Second)
		})
	}
}

func TestResumeSandboxBaselinePreventsStaleConnectTimeout(t *testing.T) {
	tests := []struct {
		name                  string
		templateName          string
		initialTimeoutSeconds int
		resumeTimeoutSeconds  int
		connectTimeoutSeconds int
	}{
		{
			name:                  "stale paused connect baseline does not shorten timeout set by resume",
			templateName:          "test-resume-sandbox-baseline-stale-connect-timeout",
			initialTimeoutSeconds: 600,
			resumeTimeoutSeconds:  900,
			connectTimeoutSeconds: 300,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, fc, teardown := Setup(t)
			defer teardown()
			user := &models.CreatedTeamAPIKey{
				ID:   keys.AdminKeyID,
				Key:  InitKey,
				Name: "admin",
			}

			cleanup := CreateSandboxPool(t, controller, tt.templateName, 1)
			defer cleanup()

			createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				TemplateID: tt.templateName,
				Timeout:    tt.initialTimeoutSeconds,
				Metadata: map[string]string{
					models.ExtensionKeySkipInitRuntime: v1alpha1.True,
				},
			}, nil, user))
			require.Nil(t, err)

			EnableWaitSim(t, controller, createResp.Body.SandboxID)
			pauseSandboxHelper(t, controller, fc, createResp.Body.SandboxID, false, false, user)

			req := NewRequest(t, nil, models.SetTimeoutRequest{
				TimeoutSeconds: tt.resumeTimeoutSeconds,
			}, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user)
			wrappedBeforeResume, getErr := controller.getSandboxOfUser(req.Context(), createResp.Body.SandboxID)
			require.Nil(t, getErr)
			baseline := wrappedBeforeResume.GetTimeout()

			go UpdateSandboxWhen(t, fc, createResp.Body.SandboxID, func(sbx *v1alpha1.Sandbox) bool {
				return sbx.Spec.Paused == false
			}, DoSetSandboxStatus(v1alpha1.SandboxRunning, metav1.ConditionFalse, metav1.ConditionTrue))
			beforeFirstCall := time.Now()
			_, apiErr := controller.ResumeSandbox(req)
			require.Nil(t, apiErr)

			afterFirstCall := GetSandbox(t, createResp.Body.SandboxID, fc)
			require.NotNil(t, afterFirstCall.Spec.ShutdownTime)
			assert.WithinDuration(t, beforeFirstCall.Add(time.Duration(tt.resumeTimeoutSeconds)*time.Second), afterFirstCall.Spec.ShutdownTime.Time, 5*time.Second)

			wrapped, getErr := controller.getSandboxOfUser(req.Context(), createResp.Body.SandboxID)
			require.Nil(t, getErr)
			errResp := controller.updateConnectTimeout(req.Context(), wrapped, tt.connectTimeoutSeconds,
				v1alpha1.SandboxStatePaused, false, afterFirstCall.Spec.ShutdownTime.Time, baseline)
			require.Nil(t, errResp)

			afterSecondCall := GetSandbox(t, createResp.Body.SandboxID, fc)
			require.NotNil(t, afterSecondCall.Spec.ShutdownTime)
			assert.WithinDuration(t, afterFirstCall.Spec.ShutdownTime.Time, afterSecondCall.Spec.ShutdownTime.Time, 5*time.Second)
		})
	}
}
