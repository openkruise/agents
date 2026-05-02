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
	} else if pausing {
		sbx := GetSandbox(t, sandboxID, client)
		if sbx.Annotations == nil {
			sbx.Annotations = map[string]string{}
		}
		sbx.Annotations["singleflight."+agentsv1alpha1.InternalPrefix+"pause-resume"] = fmt.Sprintf("1:false:%d", time.Now().Unix())
		require.NoError(t, client.Update(t.Context(), sbx))
	}
}

func TestConnectSandbox(t *testing.T) {
	DefaultTimeoutSeconds := 300
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
			expectStatus: http.StatusCreated,
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
				Timeout: DefaultTimeoutSeconds,
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
			if tt.pausing && tt.expectStatus < 300 {
				time.AfterFunc(20*time.Millisecond, func() {
					var sbx agentsv1alpha1.Sandbox
					if err := fc.Get(t.Context(), client.ObjectKey{Name: createResp.Body.SandboxID, Namespace: "default"}, &sbx); err != nil {
						return
					}
					if sbx.Annotations == nil {
						sbx.Annotations = map[string]string{}
					}
					sbx.Annotations["singleflight."+agentsv1alpha1.InternalPrefix+"pause-resume"] = fmt.Sprintf("1:true:%d", time.Now().Unix())
					if err := fc.Update(t.Context(), &sbx); err != nil {
						return
					}
					UpdateSandboxWhen(t, fc, createResp.Body.SandboxID, func(current *agentsv1alpha1.Sandbox) bool {
						return current.Annotations["singleflight."+agentsv1alpha1.InternalPrefix+"pause-resume"] != ""
					}, DoSetSandboxStatus(agentsv1alpha1.SandboxPaused, metav1.ConditionTrue, metav1.ConditionFalse))
				})
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
			controller, fc, teardown := Setup(t)
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

			baselineEndAt := createResp.Body.EndAt
			baselineParsed, parseErr := time.Parse(time.RFC3339, baselineEndAt)
			require.NoError(t, parseErr)

			sbxBefore := GetSandbox(t, createResp.Body.SandboxID, fc)
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
				sbxAfter := GetSandbox(t, createResp.Body.SandboxID, fc)
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
				sbxAfter := GetSandbox(t, createResp.Body.SandboxID, fc)
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
			expectStatus: http.StatusNoContent,
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
			if tt.pausing && tt.expectStatus < 300 {
				time.AfterFunc(20*time.Millisecond, func() {
					var sbx agentsv1alpha1.Sandbox
					if err := fc.Get(t.Context(), client.ObjectKey{Name: createResp.Body.SandboxID, Namespace: "default"}, &sbx); err != nil {
						return
					}
					if sbx.Annotations == nil {
						sbx.Annotations = map[string]string{}
					}
					sbx.Annotations["singleflight."+agentsv1alpha1.InternalPrefix+"pause-resume"] = fmt.Sprintf("1:true:%d", time.Now().Unix())
					if err := fc.Update(t.Context(), &sbx); err != nil {
						return
					}
					UpdateSandboxWhen(t, fc, createResp.Body.SandboxID, func(current *agentsv1alpha1.Sandbox) bool {
						return current.Annotations["singleflight."+agentsv1alpha1.InternalPrefix+"pause-resume"] != ""
					}, DoSetSandboxStatus(agentsv1alpha1.SandboxPaused, metav1.ConditionTrue, metav1.ConditionFalse))
				})
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
			controller, fc, teardown := Setup(t)
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

			updatedSbx := GetSandbox(t, createResp.Body.SandboxID, fc)

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
