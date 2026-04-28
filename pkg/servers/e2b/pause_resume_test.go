package e2b

import (
	"context"
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
	"github.com/openkruise/agents/pkg/utils"
)

func TestPauseSandbox(t *testing.T) {
	templateName := "test-template"
	controller, client, teardown := Setup(t)
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

func TestConnectSandbox(t *testing.T) {
	DefaultTimeoutSeconds := 300
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
			timeout:      DefaultTimeoutSeconds,
			expectStatus: http.StatusOK,
		},
		{
			name:         "resume sandbox: paused",
			paused:       true,
			pausing:      false,
			timeout:      DefaultTimeoutSeconds,
			expectStatus: http.StatusCreated,
		},
		{
			name:         "resume sandbox: pausing",
			paused:       true,
			pausing:      true,
			timeout:      DefaultTimeoutSeconds,
			expectStatus: http.StatusInternalServerError,
		},
		{
			name:         "not found",
			paused:       false,
			sandboxID:    "not-exist",
			timeout:      DefaultTimeoutSeconds,
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
			controller, client, teardown := Setup(t)
			defer teardown()
			user := &models.CreatedTeamAPIKey{
				ID:   keys.AdminKeyID,
				Key:  InitKey,
				Name: "admin",
			}

			cleanup := CreateSandboxPool(t, controller, templateName, 1)
			defer cleanup()

			createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				Timeout: DefaultTimeoutSeconds,
				Metadata: map[string]string{
					models.ExtensionKeySkipInitRuntime: agentsv1alpha1.True,
				},
				TemplateID: templateName,
			}, nil, user))
			require.Nil(t, err)
			assert.Equal(t, models.SandboxStateRunning, createResp.Body.State)

			req := NewRequest(t, nil, nil, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user)

			done := make(chan struct{})
			if tt.paused {
				pauseResp, err := controller.PauseSandbox(req)
				assert.Nil(t, err)
				AvoidGetFromCache(t, createResp.Body.SandboxID, client.SandboxClient)
				assert.Equal(t, http.StatusNoContent, pauseResp.Code)
				describeResp, err := controller.DescribeSandbox(req)
				assert.Nil(t, err)
				assert.Equal(t, models.SandboxStatePaused, describeResp.Body.State)
				var condStatus metav1.ConditionStatus
				if tt.pausing {
					condStatus = metav1.ConditionFalse
				} else {
					condStatus = metav1.ConditionTrue
				}
				UpdateSandboxWhen(t, client.SandboxClient, describeResp.Body.SandboxID, func(sbx *agentsv1alpha1.Sandbox) bool {
					return sbx.Spec.Paused == true
				}, DoSetSandboxStatus(agentsv1alpha1.SandboxPaused, condStatus, metav1.ConditionFalse))
				// Only start goroutine to resume sandbox if it's fully paused (not pausing)
				// When pausing, the test expects ConnectSandbox to fail, so no need to simulate resume
				if !tt.pausing {
					go func() {
						defer close(done)
						UpdateSandboxWhen(t, client.SandboxClient, describeResp.Body.SandboxID, func(sbx *agentsv1alpha1.Sandbox) bool {
							return sbx.Spec.Paused == false
						}, DoSetSandboxStatus(agentsv1alpha1.SandboxRunning, metav1.ConditionFalse, metav1.ConditionTrue))
					}()
				} else {
					close(done)
				}
			} else {
				close(done)
			}

			if tt.sandboxID == "" {
				tt.sandboxID = createResp.Body.SandboxID
			}
			time.Sleep(10 * time.Millisecond)
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
			<-done
		})
	}
}

func TestConnectSandboxRunningTimeoutGuard(t *testing.T) {
	templateName := "test-template-connect-timeout-guard"
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}
	tests := []struct {
		name            string
		autoPause       bool
		initialTimeout  int
		requestTimeout  int
		expectUnchanged bool
	}{
		{name: "shorter running timeout is ignored", autoPause: false, initialTimeout: 600, requestTimeout: 300, expectUnchanged: true},
		{name: "longer running timeout extends", autoPause: false, initialTimeout: 300, requestTimeout: 600, expectUnchanged: false},
		{name: "shorter auto-pause timeout is ignored", autoPause: true, initialTimeout: 600, requestTimeout: 300, expectUnchanged: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, client, teardown := Setup(t)
			defer teardown()
			cleanup := CreateSandboxPool(t, controller, templateName, 1)
			defer cleanup()

			createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				TemplateID: templateName,
				AutoPause:  tt.autoPause,
				Timeout:    tt.initialTimeout,
				Metadata: map[string]string{
					models.ExtensionKeySkipInitRuntime: agentsv1alpha1.True,
				},
			}, nil, user))
			require.Nil(t, err)
			assert.Equal(t, models.SandboxStateRunning, createResp.Body.State)
			AvoidGetFromCache(t, createResp.Body.SandboxID, client.SandboxClient)

			baselineEndAt := createResp.Body.EndAt
			baselineParsed, parseErr := time.Parse(time.RFC3339, baselineEndAt)
			require.NoError(t, parseErr)

			sbxBefore := GetSandbox(t, createResp.Body.SandboxID, client.SandboxClient)
			var pauseBefore, shutdownBefore time.Time
			if sbxBefore.Spec.PauseTime != nil {
				pauseBefore = sbxBefore.Spec.PauseTime.Time
			}
			if sbxBefore.Spec.ShutdownTime != nil {
				shutdownBefore = sbxBefore.Spec.ShutdownTime.Time
			}

			connectNow := time.Now()
			connectResp, apiErr := controller.ConnectSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
				TimeoutSeconds: tt.requestTimeout,
			}, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user))
			require.Nil(t, apiErr)
			assert.Equal(t, http.StatusOK, connectResp.Code)
			assert.Equal(t, models.SandboxStateRunning, connectResp.Body.State)

			if tt.expectUnchanged {
				endAtAfter, parseErr2 := time.Parse(time.RFC3339, connectResp.Body.EndAt)
				require.NoError(t, parseErr2)
				assert.WithinDuration(t, baselineParsed, endAtAfter, 2*time.Second,
					"EndAt should match pre-connect baseline when shorter/equal connect timeout is ignored")
				sbxAfter := GetSandbox(t, createResp.Body.SandboxID, client.SandboxClient)
				if !pauseBefore.IsZero() {
					require.NotNil(t, sbxAfter.Spec.PauseTime)
					assert.WithinDuration(t, pauseBefore, sbxAfter.Spec.PauseTime.Time, time.Second)
				}
				if !shutdownBefore.IsZero() {
					require.NotNil(t, sbxAfter.Spec.ShutdownTime)
					assert.WithinDuration(t, shutdownBefore, sbxAfter.Spec.ShutdownTime.Time, time.Second)
				}
			} else {
				AssertEndAt(t, connectNow.Add(time.Duration(tt.requestTimeout)*time.Second), connectResp.Body.EndAt)
				sbxAfter := GetSandbox(t, createResp.Body.SandboxID, client.SandboxClient)
				if tt.autoPause {
					require.NotNil(t, sbxAfter.Spec.PauseTime)
					assert.WithinDuration(t, connectNow.Add(time.Duration(tt.requestTimeout)*time.Second), sbxAfter.Spec.PauseTime.Time, 5*time.Second)
				} else {
					require.NotNil(t, sbxAfter.Spec.ShutdownTime)
					assert.WithinDuration(t, connectNow.Add(time.Duration(tt.requestTimeout)*time.Second), sbxAfter.Spec.ShutdownTime.Time, 5*time.Second)
				}
			}
		})
	}
}

func TestResumeSandbox(t *testing.T) {
	templateName := "test-template"
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
			controller, client, teardown := Setup(t)
			defer teardown()
			cleanup := CreateSandboxPool(t, controller, templateName, 1)
			defer cleanup()

			createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					models.ExtensionKeySkipInitRuntime: agentsv1alpha1.True,
				},
			}, nil, user))
			require.Nil(t, err)
			assert.Equal(t, models.SandboxStateRunning, createResp.Body.State)
			AvoidGetFromCache(t, createResp.Body.SandboxID, client.SandboxClient)

			req := NewRequest(t, nil, nil, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user)

			done := make(chan struct{})
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
				utils.SetSandboxCondition(&sbx.Status, metav1.Condition{
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
					_, _ = client.ApiV1alpha1().Sandboxes(sbx.Namespace).UpdateStatus(context.Background(), sbx, metav1.UpdateOptions{})
					close(done)
				})
			} else {
				close(done)
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
				assert.NotNil(t, err, err.Error())
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
				assert.WithinDuration(t, now.Add(time.Duration(tt.timeout)*time.Second), endAt, 5*time.Second)
			}
			<-done
		})
	}
}

func TestUpdateConnectTimeout(t *testing.T) {
	templateName := "test-update-connect-timeout"
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}

	tests := []struct {
		name            string
		initialTimeout  int
		timeoutSeconds  int
		preConnectState string
		autoPause       bool
		neverTimeout    bool // override currentEndAt to zero
		expectUpdated   bool
	}{
		{
			name:            "never-timeout is skipped",
			initialTimeout:  300,
			timeoutSeconds:  300,
			preConnectState: agentsv1alpha1.SandboxStateRunning,
			neverTimeout:    true,
			expectUpdated:   false,
		},
		{
			name:            "running sandbox, shorter timeout is skipped",
			initialTimeout:  600,
			timeoutSeconds:  300,
			preConnectState: agentsv1alpha1.SandboxStateRunning,
			expectUpdated:   false,
		},
		{
			name:            "running sandbox, longer timeout updates",
			initialTimeout:  300,
			timeoutSeconds:  600,
			preConnectState: agentsv1alpha1.SandboxStateRunning,
			expectUpdated:   true,
		},
		{
			name:            "running sandbox, equal timeout updates",
			initialTimeout:  300,
			timeoutSeconds:  300,
			preConnectState: agentsv1alpha1.SandboxStateRunning,
			expectUpdated:   true,
		},
		{
			name:            "resumed sandbox (was paused), shorter timeout updates",
			initialTimeout:  600,
			timeoutSeconds:  300,
			preConnectState: agentsv1alpha1.SandboxStatePaused,
			expectUpdated:   true,
		},
		{
			name:            "running sandbox with auto-pause, shorter timeout is skipped",
			initialTimeout:  600,
			timeoutSeconds:  300,
			preConnectState: agentsv1alpha1.SandboxStateRunning,
			autoPause:       true,
			expectUpdated:   false,
		},
		{
			name:            "running sandbox with auto-pause, longer timeout updates",
			initialTimeout:  300,
			timeoutSeconds:  600,
			preConnectState: agentsv1alpha1.SandboxStateRunning,
			autoPause:       true,
			expectUpdated:   true,
		},
		{
			name:            "running sandbox with auto-pause, equal timeout updates",
			initialTimeout:  300,
			timeoutSeconds:  300,
			preConnectState: agentsv1alpha1.SandboxStateRunning,
			autoPause:       true,
			expectUpdated:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, client, teardown := Setup(t)
			defer teardown()

			cleanup := CreateSandboxPool(t, controller, templateName, 1)
			defer cleanup()

			createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				TemplateID: templateName,
				Timeout:    tt.initialTimeout,
				AutoPause:  tt.autoPause,
				Metadata: map[string]string{
					models.ExtensionKeySkipInitRuntime: agentsv1alpha1.True,
				},
			}, nil, user))
			require.Nil(t, err)
			require.Equal(t, models.SandboxStateRunning, createResp.Body.State)
			AvoidGetFromCache(t, createResp.Body.SandboxID, client.SandboxClient)

			req := NewRequest(t, nil, nil, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user)
			sbx, apiErr := controller.getSandboxOfUser(req.Context(), createResp.Body.SandboxID)
			require.Nil(t, apiErr)
			require.NotNil(t, sbx)

			_, currentEndAt := ParseTimeout(sbx)
			if tt.neverTimeout {
				currentEndAt = time.Time{}
			}

			beforeCall := time.Now()
			result := controller.updateConnectTimeout(req.Context(), sbx, tt.timeoutSeconds,
				tt.preConnectState, tt.autoPause, currentEndAt)
			require.Nil(t, result)

			updatedSbx := GetSandbox(t, createResp.Body.SandboxID, client.SandboxClient)

			if tt.expectUpdated {
				expectedEndAt := beforeCall.Add(time.Duration(tt.timeoutSeconds) * time.Second)
				if tt.autoPause {
					// For auto-pause: ShutdownTime = now + maxTimeout, PauseTime = now + timeoutSeconds
					require.NotNil(t, updatedSbx.Spec.ShutdownTime)
					assert.WithinDuration(t, beforeCall.Add(time.Duration(controller.maxTimeout)*time.Second),
						updatedSbx.Spec.ShutdownTime.Time, 5*time.Second,
						"ShutdownTime should be maxTimeout from call time")
					require.NotNil(t, updatedSbx.Spec.PauseTime)
					assert.WithinDuration(t, expectedEndAt, updatedSbx.Spec.PauseTime.Time, 5*time.Second,
						"PauseTime should be updated to requested timeout")
				} else {
					require.NotNil(t, updatedSbx.Spec.ShutdownTime)
					assert.WithinDuration(t, expectedEndAt, updatedSbx.Spec.ShutdownTime.Time, 5*time.Second,
						"ShutdownTime should be updated to requested timeout")
					require.Nil(t, updatedSbx.Spec.PauseTime)
				}
			} else if !tt.neverTimeout {
				// For skipped running sandbox cases: ShutdownTime should be unchanged
				initialEndAt, parseErr := time.Parse(time.RFC3339, createResp.Body.EndAt)
				require.NoError(t, parseErr)
				if tt.autoPause {
					// For auto-pause, EndAt reflects PauseTime
					require.NotNil(t, updatedSbx.Spec.PauseTime)
					assert.WithinDuration(t, initialEndAt, updatedSbx.Spec.PauseTime.Time, 5*time.Second,
						"PauseTime should be unchanged")
				} else {
					require.NotNil(t, updatedSbx.Spec.ShutdownTime)
					assert.WithinDuration(t, initialEndAt, updatedSbx.Spec.ShutdownTime.Time, 5*time.Second,
						"ShutdownTime should be unchanged")
				}
			}
		})
	}
}
