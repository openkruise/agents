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
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	timeoututils "github.com/openkruise/agents/pkg/utils/timeout"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type waitHooksCache interface {
	GetWaitHooks() *sync.Map
}

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

	// pause again — should be idempotent (sandbox already paused)
	start := time.Now()
	pauseResp, err = controller.PauseSandbox(req)
	assert.Nil(t, err)
	assert.Equal(t, http.StatusNoContent, pauseResp.Code)
	describeResp, err = controller.DescribeSandbox(req)
	assert.Nil(t, err)
	assert.Equal(t, models.SandboxStatePaused, describeResp.Body.State)
	endAt, parseErr := time.Parse(time.RFC3339, describeResp.Body.EndAt)
	assert.NoError(t, parseErr)
	expectEndAt := start.Add(timeoututils.DefaultReservePausedSandboxFor)
	assert.WithinDuration(t, expectEndAt, endAt, 5*time.Second, "expect end at: %s, but got %s", expectEndAt, endAt)
}

func TestPauseSandboxManualRetention(t *testing.T) {
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}
	tests := []struct {
		name              string
		headerPresent     bool
		headerValue       string
		initialAnnotation *string
		autoPause         bool
		neverTimeout      bool
		alreadyPaused     bool
		expectStatus      int
		expectMessage     string
		expectAnnotation  string
		expectShutdown    func(now time.Time) time.Time
	}{
		{
			name:             "no header no annotation uses default and persists default",
			expectAnnotation: timeoututils.ReservePausedSandboxForDefaultValue,
			expectShutdown: func(now time.Time) time.Time {
				return now.Add(timeoututils.DefaultReservePausedSandboxFor)
			},
		},
		{
			name:             "header default uses default and persists default",
			headerPresent:    true,
			headerValue:      timeoututils.ReservePausedSandboxForDefaultValue,
			expectAnnotation: timeoututils.ReservePausedSandboxForDefaultValue,
			expectShutdown: func(now time.Time) time.Time {
				return now.Add(timeoututils.DefaultReservePausedSandboxFor)
			},
		},
		{
			name:             "header duration uses duration and persists it",
			headerPresent:    true,
			headerValue:      "1h",
			expectAnnotation: "1h",
			expectShutdown: func(now time.Time) time.Time {
				return now.Add(time.Hour)
			},
		},
		{
			name:          "invalid header returns bad request",
			headerPresent: true,
			headerValue:   "0s",
			expectStatus:  http.StatusBadRequest,
			expectMessage: "Bad extension param",
		},
		{
			name:          "empty header returns bad request",
			headerPresent: true,
			expectStatus:  http.StatusBadRequest,
			expectMessage: "Bad extension param",
		},
		{
			name:              "no header existing valid annotation uses annotation",
			initialAnnotation: stringPtr("30m"),
			expectAnnotation:  "30m",
			expectShutdown: func(now time.Time) time.Time {
				return now.Add(30 * time.Minute)
			},
		},
		{
			name:              "invalid annotation fails open and backfills default",
			initialAnnotation: stringPtr("forever"),
			expectAnnotation:  timeoututils.ReservePausedSandboxForDefaultValue,
			expectShutdown: func(now time.Time) time.Time {
				return now.Add(timeoututils.DefaultReservePausedSandboxFor)
			},
		},
		{
			name:              "valid header overrides invalid annotation",
			headerPresent:     true,
			headerValue:       "30m",
			initialAnnotation: stringPtr("forever"),
			expectAnnotation:  "30m",
			expectShutdown: func(now time.Time) time.Time {
				return now.Add(30 * time.Minute)
			},
		},
		{
			name:             "auto pause timeout moves pause and shutdown to manual retention deadline",
			headerPresent:    true,
			headerValue:      "1h",
			autoPause:        true,
			expectAnnotation: "1h",
			expectShutdown: func(now time.Time) time.Time {
				return now.Add(time.Hour)
			},
		},
		{
			name:             "never timeout pause does not set deadline",
			neverTimeout:     true,
			expectAnnotation: timeoututils.ReservePausedSandboxForDefaultValue,
		},
		{
			name:              "already paused keeps first writer annotation",
			headerPresent:     true,
			headerValue:       "1h",
			initialAnnotation: stringPtr(timeoututils.ReservePausedSandboxForDefaultValue),
			alreadyPaused:     true,
			expectAnnotation:  timeoututils.ReservePausedSandboxForDefaultValue,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, fc, teardown := Setup(t)
			defer teardown()
			templateName := "test-template-manual-retention"
			cleanup := CreateSandboxPool(t, controller, templateName, 1)
			defer cleanup()

			request := models.NewSandboxRequest{
				TemplateID: templateName,
				AutoPause:  tt.autoPause,
				Timeout:    600,
				Metadata: map[string]string{
					models.ExtensionKeySkipInitRuntime: agentsv1alpha1.True,
				},
			}
			if tt.neverTimeout {
				request.Metadata[models.ExtensionKeyNeverTimeout] = agentsv1alpha1.True
			}
			createResp, err := controller.CreateSandbox(NewRequest(t, nil, request, nil, user))
			require.Nil(t, err)
			require.Equal(t, models.SandboxStateRunning, createResp.Body.State)
			sandboxID := createResp.Body.SandboxID

			sbx := GetSandbox(t, sandboxID, fc)
			if sbx.Annotations == nil {
				sbx.Annotations = map[string]string{}
			}
			if tt.initialAnnotation != nil {
				sbx.Annotations[agentsv1alpha1.AnnotationReservePausedSandboxFor] = *tt.initialAnnotation
			} else {
				delete(sbx.Annotations, agentsv1alpha1.AnnotationReservePausedSandboxFor)
			}
			if tt.alreadyPaused {
				sbx.Spec.Paused = true
			}
			originalPauseTimeSet := sbx.Spec.PauseTime != nil
			var originalPauseTime time.Time
			if originalPauseTimeSet {
				originalPauseTime = sbx.Spec.PauseTime.Time
			}
			originalShutdownTimeSet := sbx.Spec.ShutdownTime != nil
			var originalShutdownTime time.Time
			if originalShutdownTimeSet {
				originalShutdownTime = sbx.Spec.ShutdownTime.Time
			}
			require.NoError(t, fc.Update(t.Context(), sbx))

			EnableWaitSim(t, controller, sandboxID)
			if tt.alreadyPaused {
				UpdateSandboxWhen(t, fc, sandboxID, Immediately,
					DoSetSandboxStatus(agentsv1alpha1.SandboxPaused, metav1.ConditionTrue, metav1.ConditionFalse))
			} else if tt.expectStatus == 0 {
				go UpdateSandboxWhen(t, fc, sandboxID, func(sbx *agentsv1alpha1.Sandbox) bool {
					return sbx.Spec.Paused == true
				}, DoSetSandboxStatus(agentsv1alpha1.SandboxPaused, metav1.ConditionTrue, metav1.ConditionFalse))
			}

			req := NewRequest(t, nil, nil, map[string]string{"sandboxID": sandboxID}, user)
			if tt.headerPresent {
				req.Header.Set(models.ExtensionHeaderReservePausedSandboxFor, tt.headerValue)
			}
			beforePause := time.Now()
			pauseResp, apiErr := controller.PauseSandbox(req)
			if tt.expectStatus != 0 {
				require.NotNil(t, apiErr)
				assert.Equal(t, tt.expectStatus, apiErr.Code)
				assert.Contains(t, apiErr.Message, tt.expectMessage)
				return
			}
			require.Nil(t, apiErr)
			assert.Equal(t, http.StatusNoContent, pauseResp.Code)

			updated := GetSandbox(t, sandboxID, fc)
			assert.Equal(t, tt.expectAnnotation, updated.Annotations[agentsv1alpha1.AnnotationReservePausedSandboxFor])
			if tt.neverTimeout {
				assert.Nil(t, updated.Spec.PauseTime)
				assert.Nil(t, updated.Spec.ShutdownTime)
			} else if tt.alreadyPaused {
				if originalPauseTimeSet {
					require.NotNil(t, updated.Spec.PauseTime)
					assert.True(t, updated.Spec.PauseTime.Time.Equal(originalPauseTime))
				} else {
					assert.Nil(t, updated.Spec.PauseTime)
				}
				if originalShutdownTimeSet {
					require.NotNil(t, updated.Spec.ShutdownTime)
					assert.True(t, updated.Spec.ShutdownTime.Time.Equal(originalShutdownTime))
				} else {
					assert.Nil(t, updated.Spec.ShutdownTime)
				}
			} else if tt.expectShutdown != nil && !tt.alreadyPaused {
				require.NotNil(t, updated.Spec.ShutdownTime)
				expectedShutdown := tt.expectShutdown(beforePause)
				assert.WithinDuration(t, expectedShutdown, updated.Spec.ShutdownTime.Time, 5*time.Second)
				if tt.autoPause {
					require.NotNil(t, updated.Spec.PauseTime)
					assert.WithinDuration(t, expectedShutdown, updated.Spec.PauseTime.Time, 5*time.Second)
				}
			}
		})
	}
}

func stringPtr(value string) *string {
	return &value
}

func TestPauseSandboxConflict(t *testing.T) {
	tests := []struct {
		name          string
		prepare       func(t *testing.T, controller *Controller, sandboxID string) func()
		expectStatus  int
		expectMessage string
	}{
		{
			name: "active resume wait returns conflict",
			prepare: func(t *testing.T, controller *Controller, sandboxID string) func() {
				sbx := GetSandbox(t, sandboxID, controller.cache.GetClient())
				task, err := controller.cache.NewSandboxResumeTask(t.Context(), sbx)
				require.NoError(t, err)
				return task.Release
			},
			expectStatus:  http.StatusConflict,
			expectMessage: "another action(Resume)'s wait task already exists",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			templateName := "test-template-pause-conflict"
			controller, _, teardown := Setup(t)
			defer teardown()
			cleanup := CreateSandboxPool(t, controller, templateName, 1)
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
			require.Equal(t, models.SandboxStateRunning, createResp.Body.State)

			release := tt.prepare(t, controller, createResp.Body.SandboxID)
			if release != nil {
				defer release()
			}
			_, apiErr := controller.PauseSandbox(NewRequest(t, nil, nil, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user))
			require.NotNil(t, apiErr)
			assert.Equal(t, tt.expectStatus, apiErr.Code)
			assert.Contains(t, apiErr.Message, tt.expectMessage)
		})
	}
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
		}, DoSetSandboxStatus(agentsv1alpha1.SandboxPaused, metav1.ConditionFalse, ""))
	} else if resuming {
		// Set resuming state: Spec.Paused=false, Phase=Resuming, Ready=false
		// This means sandbox is transitioning from paused to running
		sbx := GetSandbox(t, sandboxID, client)
		sbx.Spec.Paused = false
		require.NoError(t, client.Update(t.Context(), sbx))
		// Update status to reflect resuming state
		UpdateSandboxWhen(t, client, sandboxID, func(sbx *agentsv1alpha1.Sandbox) bool {
			return sbx.Spec.Paused == false
		}, DoSetSandboxStatus(agentsv1alpha1.SandboxResuming, "", metav1.ConditionFalse))
	}
}

func setInFlightResumeTimeout(t *testing.T, client client.Client, sandboxID string, endAt time.Time) {
	sbx := GetSandbox(t, sandboxID, client)
	sbx.Spec.ShutdownTime = &metav1.Time{Time: endAt}
	sbx.Spec.PauseTime = nil
	require.NoError(t, client.Update(t.Context(), sbx))
}

func waitForResumeUpdate(controller *Controller, waitForResumeHook bool) WhenFunc {
	return func(sbx *agentsv1alpha1.Sandbox) bool {
		if sbx.Spec.Paused {
			return false
		}
		if !waitForResumeHook {
			return true
		}
		cache, ok := controller.cache.(waitHooksCache)
		if !ok || cache.GetWaitHooks() == nil {
			return false
		}
		value, ok := cache.GetWaitHooks().Load(cacheutils.WaitHookKey[*agentsv1alpha1.Sandbox](sbx))
		if !ok {
			return false
		}
		entry, ok := value.(*cacheutils.WaitEntry[*agentsv1alpha1.Sandbox])
		return ok && entry.Action == cacheutils.WaitActionResume
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
			var inFlightResumeEndAt time.Time
			if tt.resuming {
				inFlightResumeEndAt = time.Now().Add(10 * time.Minute).Truncate(time.Second)
				setInFlightResumeTimeout(t, fc, createResp.Body.SandboxID, inFlightResumeEndAt)
			}

			if tt.sandboxID == "" {
				tt.sandboxID = createResp.Body.SandboxID
			}
			if tt.expectStatus < 300 {
				waitForResumeHook := tt.paused && !tt.pausing
				go UpdateSandboxWhen(t, fc, createResp.Body.SandboxID, waitForResumeUpdate(controller, waitForResumeHook),
					DoSetSandboxStatus(agentsv1alpha1.SandboxRunning, "", metav1.ConditionTrue))
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
				if tt.resuming {
					expectEndAt = inFlightResumeEndAt
				}
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

func TestConnectSandboxExtendOnlySkipDoesNotBackfillReservePausedAnnotation(t *testing.T) {
	tests := []struct {
		name           string
		templateName   string
		timeoutSeconds int
	}{
		{
			name:           "extend-only skip does not backfill reserve paused annotation",
			templateName:   "test-connect-extend-only-skip-no-retention-backfill",
			timeoutSeconds: 300,
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
				AutoPause:  true,
				Timeout:    600,
				Metadata: map[string]string{
					models.ExtensionKeySkipInitRuntime: agentsv1alpha1.True,
				},
			}, nil, user))
			require.Nil(t, err)
			require.Equal(t, models.SandboxStateRunning, createResp.Body.State)

			before := GetSandbox(t, createResp.Body.SandboxID, fc)
			require.NotNil(t, before.Spec.PauseTime)
			require.NotNil(t, before.Spec.ShutdownTime)
			delete(before.Annotations, agentsv1alpha1.AnnotationReservePausedSandboxFor)
			require.NoError(t, fc.Update(t.Context(), before))
			pauseBefore := before.Spec.PauseTime.Time
			shutdownBefore := before.Spec.ShutdownTime.Time

			connectResp, apiErr := controller.ConnectSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
				TimeoutSeconds: tt.timeoutSeconds,
			}, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user))
			require.Nil(t, apiErr)
			assert.Equal(t, http.StatusOK, connectResp.Code)

			after := GetSandbox(t, createResp.Body.SandboxID, fc)
			require.NotNil(t, after.Spec.PauseTime)
			require.NotNil(t, after.Spec.ShutdownTime)
			assert.WithinDuration(t, pauseBefore, after.Spec.PauseTime.Time, time.Second)
			assert.WithinDuration(t, shutdownBefore, after.Spec.ShutdownTime.Time, time.Second)
			assert.NotContains(t, after.Annotations, agentsv1alpha1.AnnotationReservePausedSandboxFor)
		})
	}
}

func TestConnectSandboxConcurrentPausedTimeouts(t *testing.T) {
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}
	tests := []struct {
		name         string
		templateName string
		autoPause    bool
	}{
		{
			name:         "manual pause",
			templateName: "test-template-concurrent-connect-manual-pause",
			autoPause:    false,
		},
		{
			name:         "auto pause",
			templateName: "test-template-concurrent-connect-auto-pause",
			autoPause:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, fc, teardown := Setup(t)
			defer teardown()
			cleanup := CreateSandboxPool(t, controller, tt.templateName, 1)
			defer cleanup()

			createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				TemplateID: tt.templateName,
				AutoPause:  tt.autoPause,
				Timeout:    600,
				Metadata: map[string]string{
					models.ExtensionKeySkipInitRuntime: agentsv1alpha1.True,
				},
			}, nil, user))
			require.Nil(t, err)
			require.Equal(t, models.SandboxStateRunning, createResp.Body.State)

			EnableWaitSim(t, controller, createResp.Body.SandboxID)
			if tt.autoPause {
				sbx := GetSandbox(t, createResp.Body.SandboxID, fc)
				sbx.Spec.Paused = true
				require.NoError(t, fc.Update(t.Context(), sbx))
				UpdateSandboxWhen(t, fc, createResp.Body.SandboxID, Immediately,
					DoSetSandboxStatus(agentsv1alpha1.SandboxPaused, metav1.ConditionTrue, metav1.ConditionFalse))
			} else {
				pauseSandboxHelper(t, controller, fc, createResp.Body.SandboxID, false, false, user)
			}

			go UpdateSandboxWhen(t, fc, createResp.Body.SandboxID, func(sbx *agentsv1alpha1.Sandbox) bool {
				return !sbx.Spec.Paused
			}, DoSetSandboxStatus(agentsv1alpha1.SandboxRunning, metav1.ConditionFalse, metav1.ConditionTrue))

			type connectResult struct {
				timeoutSeconds int
				code           int
				state          string
				err            string
			}
			results := make(chan connectResult, 2)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for _, timeoutSeconds := range []int{900, 300} {
				timeoutSeconds := timeoutSeconds
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					resp, apiErr := controller.ConnectSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
						TimeoutSeconds: timeoutSeconds,
					}, map[string]string{
						"sandboxID": createResp.Body.SandboxID,
					}, user))
					if apiErr != nil {
						results <- connectResult{timeoutSeconds: timeoutSeconds, err: apiErr.Error()}
						return
					}
					results <- connectResult{
						timeoutSeconds: timeoutSeconds,
						code:           resp.Code,
						state:          resp.Body.State,
					}
				}()
			}

			startedAt := time.Now()
			close(start)
			wg.Wait()
			close(results)

			for result := range results {
				require.Empty(t, result.err, "ConnectSandbox(%d) failed", result.timeoutSeconds)
				assert.Less(t, result.code, http.StatusMultipleChoices, "ConnectSandbox(%d) status", result.timeoutSeconds)
				assert.Equal(t, models.SandboxStateRunning, result.state)
			}

			updated := GetSandbox(t, createResp.Body.SandboxID, fc)
			expectedEndAt := startedAt.Add(900 * time.Second)
			if tt.autoPause {
				require.NotNil(t, updated.Spec.PauseTime)
				assert.WithinDuration(t, expectedEndAt, updated.Spec.PauseTime.Time, 5*time.Second)
			} else {
				require.NotNil(t, updated.Spec.ShutdownTime)
				assert.WithinDuration(t, expectedEndAt, updated.Spec.ShutdownTime.Time, 5*time.Second)
				assert.Nil(t, updated.Spec.PauseTime)
			}
		})
	}
}

// TestResumeSandboxMissingRetentionUsesDefault verifies that Resume placeholder
// timeout math (PauseTime + retention -> ShutdownTime) still works using the
// default retention when the reserve-paused annotation is missing.
func TestResumeSandboxMissingRetentionUsesDefault(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}

	templateName := "test-resume-placeholder-retention-backfill"
	cleanup := CreateSandboxPool(t, controller, templateName, 1)
	defer cleanup()

	createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
		TemplateID: templateName,
		AutoPause:  true,
		Timeout:    300,
		Metadata: map[string]string{
			models.ExtensionKeySkipInitRuntime: agentsv1alpha1.True,
		},
	}, nil, user))
	require.Nil(t, err)
	require.Equal(t, models.SandboxStateRunning, createResp.Body.State)

	EnableWaitSim(t, controller, createResp.Body.SandboxID)
	pauseSandboxHelper(t, controller, fc, createResp.Body.SandboxID, false, false, user)

	// Remove the annotation AFTER pause so we can verify Resume does not recreate it.
	// Pause may have backfilled it; we explicitly remove it post-pause to isolate Resume behavior.
	sbx := GetSandbox(t, createResp.Body.SandboxID, fc)
	delete(sbx.Annotations, agentsv1alpha1.AnnotationReservePausedSandboxFor)
	require.NoError(t, fc.Update(t.Context(), sbx))

	go UpdateSandboxWhen(t, fc, createResp.Body.SandboxID, func(sbx *agentsv1alpha1.Sandbox) bool {
		return sbx.Spec.Paused == false
	}, DoSetSandboxStatus(agentsv1alpha1.SandboxRunning, metav1.ConditionFalse, metav1.ConditionTrue))

	beforeResume := time.Now()
	_, apiErr := controller.ResumeSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
		TimeoutSeconds: 600,
	}, map[string]string{
		"sandboxID": createResp.Body.SandboxID,
	}, user))
	require.Nil(t, apiErr)

	after := GetSandbox(t, createResp.Body.SandboxID, fc)
	if got, ok := after.Annotations[agentsv1alpha1.AnnotationReservePausedSandboxFor]; ok {
		assert.Equal(t, timeoututils.ReservePausedSandboxForDefaultValue, got)
	}
	// Placeholder timeout math still works using default retention
	require.NotNil(t, after.Spec.PauseTime)
	require.NotNil(t, after.Spec.ShutdownTime)
	assert.WithinDuration(t, beforeResume.Add(600*time.Second), after.Spec.PauseTime.Time, 5*time.Second)
	assert.WithinDuration(t, after.Spec.PauseTime.Time.Add(timeoututils.DefaultReservePausedSandboxFor), after.Spec.ShutdownTime.Time, 5*time.Second)
}

// TestResumeSandboxCustomRetentionPlaceholderDoesNotBackfillAnnotation verifies
// that when a paused sandbox has a custom retention annotation (e.g. "30m"),
// Resume preserves it and uses the custom retention for placeholder timeout math.
// ResumeOptions never carries annotation backfill.
func TestResumeSandboxCustomRetentionPlaceholderDoesNotBackfillAnnotation(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}

	templateName := "test-resume-custom-retention-placeholder"
	cleanup := CreateSandboxPool(t, controller, templateName, 1)
	defer cleanup()

	createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
		TemplateID: templateName,
		AutoPause:  true,
		Timeout:    300,
		Metadata: map[string]string{
			models.ExtensionKeySkipInitRuntime: agentsv1alpha1.True,
		},
	}, nil, user))
	require.Nil(t, err)
	require.Equal(t, models.SandboxStateRunning, createResp.Body.State)

	EnableWaitSim(t, controller, createResp.Body.SandboxID)
	pauseSandboxHelper(t, controller, fc, createResp.Body.SandboxID, false, false, user)

	// Set a custom retention annotation on the paused sandbox
	sbx := GetSandbox(t, createResp.Body.SandboxID, fc)
	if sbx.Annotations == nil {
		sbx.Annotations = map[string]string{}
	}
	sbx.Annotations[agentsv1alpha1.AnnotationReservePausedSandboxFor] = "30m"
	require.NoError(t, fc.Update(t.Context(), sbx))

	go UpdateSandboxWhen(t, fc, createResp.Body.SandboxID, func(sbx *agentsv1alpha1.Sandbox) bool {
		return sbx.Spec.Paused == false
	}, DoSetSandboxStatus(agentsv1alpha1.SandboxRunning, metav1.ConditionFalse, metav1.ConditionTrue))

	beforeResume := time.Now()
	_, apiErr := controller.ResumeSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
		TimeoutSeconds: 600,
	}, map[string]string{
		"sandboxID": createResp.Body.SandboxID,
	}, user))
	require.Nil(t, apiErr)

	after := GetSandbox(t, createResp.Body.SandboxID, fc)
	// Annotation remains "30m" — not overwritten by Resume
	assert.Equal(t, "30m", after.Annotations[agentsv1alpha1.AnnotationReservePausedSandboxFor])
	// Placeholder timeout uses 30m retention math
	require.NotNil(t, after.Spec.PauseTime)
	require.NotNil(t, after.Spec.ShutdownTime)
	assert.WithinDuration(t, beforeResume.Add(600*time.Second), after.Spec.PauseTime.Time, 5*time.Second)
	assert.WithinDuration(t, after.Spec.PauseTime.Time.Add(30*time.Minute), after.Spec.ShutdownTime.Time, 5*time.Second)
}

func TestPauseSandboxErrorCode(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		expectStatus int
	}{
		{
			name:         "manager conflict returns conflict",
			err:          managererrors.NewError(managererrors.ErrorConflict, "pause conflict"),
			expectStatus: http.StatusConflict,
		},
		{
			name:         "wait task conflict returns conflict",
			err:          fmt.Errorf("pause failed: %w", cacheutils.ErrWaitTaskConflict),
			expectStatus: http.StatusConflict,
		},
		{
			name:         "kubernetes not found returns not found",
			err:          apierrors.NewNotFound(schema.GroupResource{Group: agentsv1alpha1.GroupVersion.Group, Resource: "sandboxes"}, "sandbox-id"),
			expectStatus: http.StatusNotFound,
		},
		{
			name:         "unknown error returns internal server error",
			err:          errors.New("pause failed"),
			expectStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expectStatus, pauseSandboxErrorCode(tt.err))
		})
	}
}

func TestResumeSandboxErrorCode(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		expectStatus int
	}{
		{
			name:         "manager conflict returns conflict",
			err:          managererrors.NewError(managererrors.ErrorConflict, "resume conflict"),
			expectStatus: http.StatusConflict,
		},
		{
			name:         "wait task conflict returns conflict",
			err:          fmt.Errorf("resume failed: %w", cacheutils.ErrWaitTaskConflict),
			expectStatus: http.StatusConflict,
		},
		{
			name:         "kubernetes not found returns not found",
			err:          apierrors.NewNotFound(schema.GroupResource{Group: agentsv1alpha1.GroupVersion.Group, Resource: "sandboxes"}, "sandbox-id"),
			expectStatus: http.StatusNotFound,
		},
		{
			name:         "unknown error returns internal server error",
			err:          errors.New("resume failed"),
			expectStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expectStatus, resumeSandboxErrorCode(tt.err))
		})
	}
}

func TestResumeSandbox(t *testing.T) {
	tests := []struct {
		name         string
		paused       bool   // if sandbox is set paused
		pausing      bool   // if sandbox is performing pausing
		resuming     bool   // if sandbox is performing resuming (Ready condition is false)
		notReady     bool   // if running sandbox is not ready and therefore dead
		sandboxID    string // if not set, use the created sandbox ID
		timeout      int
		expectStatus int
	}{
		{
			// Running sandboxes succeed idempotently: infra Resume short-circuits
			// on cond.Ready==True and the handler falls through to ExtendOnly
			// timeout update (mirrors ConnectSandbox's Running path).
			name:         "running sandbox",
			paused:       false,
			timeout:      300,
			expectStatus: http.StatusNoContent,
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
			// IsSandboxResumable rejects Paused+!Ready ("SandboxIsPausing") with
			// ErrorConflict, which resumeSandboxErrorCode maps to 409.
			name:         "resume sandbox: pausing",
			paused:       true,
			pausing:      true,
			timeout:      300,
			expectStatus: http.StatusConflict,
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
			name:         "running not-ready sandbox",
			notReady:     true,
			timeout:      300,
			expectStatus: http.StatusNotFound,
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
			if tt.notReady {
				UpdateSandboxWhen(t, fc, createResp.Body.SandboxID, Immediately,
					DoSetSandboxStatus(agentsv1alpha1.SandboxRunning, "", metav1.ConditionFalse))
			}
			var inFlightResumeEndAt time.Time
			if tt.resuming {
				inFlightResumeEndAt = time.Now().Add(10 * time.Minute).Truncate(time.Second)
				setInFlightResumeTimeout(t, fc, createResp.Body.SandboxID, inFlightResumeEndAt)
			}

			if tt.sandboxID == "" {
				tt.sandboxID = createResp.Body.SandboxID
			}
			if tt.expectStatus < 300 {
				// Only schedule async update when expecting success
				waitForResumeHook := tt.paused && !tt.pausing
				go UpdateSandboxWhen(t, fc, createResp.Body.SandboxID, waitForResumeUpdate(controller, waitForResumeHook),
					DoSetSandboxStatus(agentsv1alpha1.SandboxRunning, "", metav1.ConditionTrue))
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
				if tt.resuming {
					expectEndAt = inFlightResumeEndAt
				}
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
		name              string
		initialTimeout    int
		timeoutSeconds    int
		preConnectState   string
		autoPause         bool
		neverTimeout      bool // override currentEndAt to zero
		initialAnnotation *string
		expectAnnotation  string
		expectUpdated     bool
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
			name:            "resumed sandbox (was paused), shorter timeout is skipped (ExtendOnly)",
			initialTimeout:  600,
			timeoutSeconds:  300,
			preConnectState: agentsv1alpha1.SandboxStatePaused,
			expectUpdated:   false,
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
			name:              "running sandbox with auto-pause, invalid annotation backfills default on accepted update",
			initialTimeout:    300,
			timeoutSeconds:    600,
			preConnectState:   agentsv1alpha1.SandboxStateRunning,
			autoPause:         true,
			initialAnnotation: stringPtr("forever"),
			expectAnnotation:  timeoututils.ReservePausedSandboxForDefaultValue,
			expectUpdated:     true,
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

			if tt.initialAnnotation != nil {
				sbx := GetSandbox(t, createResp.Body.SandboxID, fc)
				if sbx.Annotations == nil {
					sbx.Annotations = map[string]string{}
				}
				sbx.Annotations[agentsv1alpha1.AnnotationReservePausedSandboxFor] = *tt.initialAnnotation
				require.NoError(t, fc.Update(t.Context(), sbx))
			}

			req := NewRequest(t, nil, nil, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user)
			sbx, apiErr := controller.getSandboxOfUser(req.Context(), createResp.Body.SandboxID, claimedSandboxStates)
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
					// For auto-pause: ShutdownTime follows the persisted paused retention from PauseTime.
					require.NotNil(t, updatedSbx.Spec.ShutdownTime)
					assert.WithinDuration(t, expectedEndAt.Add(timeoututils.DefaultReservePausedSandboxFor),
						updatedSbx.Spec.ShutdownTime.Time, 5*time.Second,
						"ShutdownTime should be PauseTime plus default paused retention")
					require.NotNil(t, updatedSbx.Spec.PauseTime)
					assert.WithinDuration(t, expectedEndAt, updatedSbx.Spec.PauseTime.Time, 5*time.Second,
						"PauseTime should be updated to requested timeout")
					if tt.expectAnnotation != "" {
						assert.Equal(t, tt.expectAnnotation, updatedSbx.Annotations[agentsv1alpha1.AnnotationReservePausedSandboxFor])
					}
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

// pickPlaceholderDeadline returns the spec field that buildSetTimeoutOptions
// populates for a (autoPause) sandbox: PauseTime when auto-pause is enabled,
// ShutdownTime otherwise.
func pickPlaceholderDeadline(sbx *agentsv1alpha1.Sandbox, autoPause bool) (*metav1.Time, string) {
	if autoPause {
		return sbx.Spec.PauseTime, "PauseTime"
	}
	return sbx.Spec.ShutdownTime, "ShutdownTime"
}

// placeholderAssertion returns the hook the Resume-wait observer fires if
// it sees Spec.Paused=false. At that instant the Resume mutator has just
// committed and the e2b handler is still blocked inside Resume's Wait, so the
// observed payload is the atomic placeholder — proving Resume() wrote
// Timeout and Spec.Paused=false in the same Update. After asserting the
// placeholder shape, the hook releases the Wait by flipping Status to Running.
func placeholderAssertion(beforeT time.Time, neverTimeout, autoPause bool, wantEffective int) func(*testing.T, client.Client, *agentsv1alpha1.Sandbox) {
	return func(t *testing.T, c client.Client, sbx *agentsv1alpha1.Sandbox) {
		if neverTimeout {
			assert.Nil(t, sbx.Spec.PauseTime, "never-timeout sandbox must retain nil PauseTime through Resume")
			assert.Nil(t, sbx.Spec.ShutdownTime, "never-timeout sandbox must retain nil ShutdownTime through Resume")
		} else {
			deadline, field := pickPlaceholderDeadline(sbx, autoPause)
			if assert.NotNilf(t, deadline, "placeholder %s must be set in the same Update as Paused=false", field) {
				expectMin := beforeT.Add(time.Duration(wantEffective) * time.Second).Add(-2 * time.Second)
				expectMax := beforeT.Add(time.Duration(wantEffective) * time.Second).Add(2 * time.Second)
				assert.False(t, deadline.Time.Before(expectMin),
					"placeholder %s %s must reflect effectiveTimeout window (>= %s)", field, deadline, expectMin)
				assert.False(t, deadline.Time.After(expectMax),
					"placeholder %s %s must reflect effectiveTimeout window (<= %s, before post-Resume slide)", field, deadline, expectMax)
			}
		}
		DoSetSandboxStatus(agentsv1alpha1.SandboxRunning, "", metav1.ConditionTrue)(t, c, sbx)
	}
}

// assertFinalDeadline verifies the persisted deadline is bounded by:
//
//	beforeT + wantEffective  <= deadline <= beforeT + wantEffective + 30s
//
// The lower bound is the placeholder written at Resume entry; the upper
// bound is the post-Resume ExtendOnly slide (≈ Resume wall-clock duration,
// with 30s of slack for goroutine scheduling on the fake client).
func assertFinalDeadline(t *testing.T, final *agentsv1alpha1.Sandbox, autoPause bool, beforeT time.Time, wantEffective int) {
	t.Helper()
	expectMin := beforeT.Add(time.Duration(wantEffective) * time.Second).Truncate(time.Second)
	expectMax := beforeT.Add(time.Duration(wantEffective+30) * time.Second)
	deadline, _ := pickPlaceholderDeadline(final, autoPause)
	require.NotNil(t, deadline, "timed sandbox must have the buildSetTimeoutOptions-selected deadline (autoPause=%v)", autoPause)
	assert.True(t, !deadline.Time.Before(expectMin),
		"deadline %s must be >= expected min %s (effective=%ds)", deadline, expectMin, wantEffective)
	assert.True(t, !deadline.Time.After(expectMax),
		"deadline %s must be <= expected max %s (effective=%ds + ~Resume duration)", deadline, expectMax, wantEffective)
}

// TestConnectSandbox_ResumeFloorAndPlaceholder covers the four Connect floor
// + atomic-placeholder scenarios: below-floor / above-floor / never-timeout
// for paused sandboxes, plus the running case where the floor must not fire.
func TestConnectSandbox_ResumeFloorAndPlaceholder(t *testing.T) {
	const minResume = 120
	templateName := "test-template-floor-connect"
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}

	cases := []struct {
		name           string
		paused         bool
		autoPause      bool // false → buildSetTimeoutOptions writes ShutdownTime only
		neverTimeout   bool // create sandbox as never-timeout via metadata key
		requestTimeout int
		// Expected effective timeout after floor enforcement.
		// For never-timeout this is unused (assertion below skips it).
		wantEffective int
		wantStatus    int
	}{
		{name: "paused-autopause-below-floor", paused: true, autoPause: true, requestTimeout: 60, wantEffective: minResume, wantStatus: http.StatusCreated},
		{name: "paused-autopause-above-floor", paused: true, autoPause: true, requestTimeout: 600, wantEffective: 600, wantStatus: http.StatusCreated},
		{name: "paused-no-autopause-below-floor", paused: true, autoPause: false, requestTimeout: 60, wantEffective: minResume, wantStatus: http.StatusCreated},
		{name: "paused-never-timeout", paused: true, autoPause: true, neverTimeout: true, requestTimeout: 60, wantStatus: http.StatusCreated},
		{name: "running-floor-skipped", paused: false, autoPause: true, requestTimeout: 60, wantEffective: 60, wantStatus: http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			controller, fc, teardown := SetupWithMinResumeTimeout(t, minResume)
			defer teardown()
			cleanup := CreateSandboxPool(t, controller, templateName, 1)
			defer cleanup()

			metadata := map[string]string{
				models.ExtensionKeySkipInitRuntime: agentsv1alpha1.True,
			}
			// Create with a generous timeout so auto-pause is not an issue mid-test.
			// For never-timeout, set the extension key — Timeout=0 alone falls
			// back to DefaultTimeoutSeconds in parseCreateSandboxRequest.
			if tc.neverTimeout {
				metadata[models.ExtensionKeyNeverTimeout] = agentsv1alpha1.True
			}
			createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				Timeout:    600,
				AutoPause:  tc.autoPause,
				Metadata:   metadata,
				TemplateID: templateName,
			}, nil, user))
			require.Nil(t, err)
			sandboxID := createResp.Body.SandboxID

			EnableWaitSim(t, controller, sandboxID)

			if tc.paused {
				pauseSandboxHelper(t, controller, fc, sandboxID, false, false, user)
			}

			// beforeConnect must be sampled BEFORE the goroutine launches: the
			// placeholder assertion compares Spec.PauseTime/ShutdownTime
			// against (beforeConnect + wantEffective).
			beforeConnect := time.Now()
			if tc.paused {
				hook := placeholderAssertion(beforeConnect, tc.neverTimeout, tc.autoPause, tc.wantEffective)
				go UpdateSandboxWhen(t, fc, sandboxID, waitForResumeUpdate(controller, true), hook)
			}

			resp, apiErr := controller.ConnectSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
				TimeoutSeconds: tc.requestTimeout,
			}, map[string]string{"sandboxID": sandboxID}, user))
			require.Nil(t, apiErr)
			assert.Equal(t, tc.wantStatus, resp.Code)

			final := GetSandbox(t, sandboxID, fc)
			assert.False(t, final.Spec.Paused, "Spec.Paused must be false after Connect on Paused")

			if tc.neverTimeout {
				assert.Nil(t, final.Spec.PauseTime, "never-timeout sandbox must retain nil PauseTime")
				assert.Nil(t, final.Spec.ShutdownTime, "never-timeout sandbox must retain nil ShutdownTime")
				return
			}
			// Running case: the floor is skipped and no Resume occurs;
			// updateConnectTimeout under ExtendOnly preserves the pre-existing
			// create-time deadline. "Floor not applied" is already covered by
			// wantStatus == StatusOK (a Resume would yield 201).
			if !tc.paused {
				return
			}
			assertFinalDeadline(t, final, tc.autoPause, beforeConnect, tc.wantEffective)
		})
	}
}

// TestResumeSandbox_ResumeFloorAndPlaceholder mirrors
// TestConnectSandbox_ResumeFloorAndPlaceholder for the legacy ResumeSandbox
// handler. Non-paused cases are omitted (legacy returns 409). Status code
// is StatusNoContent.
func TestResumeSandbox_ResumeFloorAndPlaceholder(t *testing.T) {
	const minResume = 120
	templateName := "test-template-floor-resume"
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}

	cases := []struct {
		name           string
		autoPause      bool
		neverTimeout   bool
		requestTimeout int
		wantEffective  int
	}{
		{name: "paused-autopause-below-floor", autoPause: true, requestTimeout: 60, wantEffective: minResume},
		{name: "paused-autopause-above-floor", autoPause: true, requestTimeout: 600, wantEffective: 600},
		{name: "paused-no-autopause-below-floor", autoPause: false, requestTimeout: 60, wantEffective: minResume},
		{name: "paused-never-timeout", autoPause: true, neverTimeout: true, requestTimeout: 60},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			controller, fc, teardown := SetupWithMinResumeTimeout(t, minResume)
			defer teardown()
			cleanup := CreateSandboxPool(t, controller, templateName, 1)
			defer cleanup()

			metadata := map[string]string{
				models.ExtensionKeySkipInitRuntime: agentsv1alpha1.True,
			}
			if tc.neverTimeout {
				metadata[models.ExtensionKeyNeverTimeout] = agentsv1alpha1.True
			}
			createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				Timeout:    600,
				AutoPause:  tc.autoPause,
				Metadata:   metadata,
				TemplateID: templateName,
			}, nil, user))
			require.Nil(t, err)
			sandboxID := createResp.Body.SandboxID

			EnableWaitSim(t, controller, sandboxID)
			pauseSandboxHelper(t, controller, fc, sandboxID, false, false, user)

			beforeResume := time.Now()
			hook := placeholderAssertion(beforeResume, tc.neverTimeout, tc.autoPause, tc.wantEffective)
			go UpdateSandboxWhen(t, fc, sandboxID, waitForResumeUpdate(controller, true), hook)

			resp, apiErr := controller.ResumeSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
				TimeoutSeconds: tc.requestTimeout,
			}, map[string]string{"sandboxID": sandboxID}, user))
			require.Nil(t, apiErr)
			assert.Equal(t, http.StatusNoContent, resp.Code)

			final := GetSandbox(t, sandboxID, fc)
			assert.False(t, final.Spec.Paused, "Spec.Paused must be false after Resume")

			if tc.neverTimeout {
				assert.Nil(t, final.Spec.PauseTime, "never-timeout sandbox must retain nil PauseTime")
				assert.Nil(t, final.Spec.ShutdownTime, "never-timeout sandbox must retain nil ShutdownTime")
				return
			}
			assertFinalDeadline(t, final, tc.autoPause, beforeResume, tc.wantEffective)
		})
	}
}

func TestComputeTimeoutOptions(t *testing.T) {
	now := time.Date(2026, time.June, 11, 10, 0, 0, 123, time.UTC)
	tests := []struct {
		name            string
		autoPause       bool
		timeoutSeconds  int
		pausedRetention time.Duration
		wantPause       *time.Time // nil means zero / not set
		wantShutdown    time.Time
	}{
		{
			name:            "auto pause with retention",
			autoPause:       true,
			timeoutSeconds:  300,
			pausedRetention: 2 * time.Hour,
			wantPause:       ptr.To(time.Date(2026, time.June, 11, 10, 5, 0, 0, time.UTC)),
			wantShutdown:    time.Date(2026, time.June, 11, 12, 5, 0, 0, time.UTC),
		},
		{
			name:            "no auto pause sets shutdown only",
			autoPause:       false,
			timeoutSeconds:  600,
			pausedRetention: 0,
			wantPause:       nil,
			wantShutdown:    now.Add(600 * time.Second),
		},
		{
			name:            "auto pause with zero retention",
			autoPause:       true,
			timeoutSeconds:  60,
			pausedRetention: 0,
			wantPause:       ptr.To(time.Date(2026, time.June, 11, 10, 1, 0, 0, time.UTC)),
			wantShutdown:    time.Date(2026, time.June, 11, 10, 1, 0, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeTimeoutOptions(tt.autoPause, now, tt.timeoutSeconds, tt.pausedRetention)
			if tt.wantPause == nil {
				assert.True(t, got.PauseTime.IsZero(), "PauseTime should be zero")
			} else {
				assert.Equal(t, *tt.wantPause, got.PauseTime)
			}
			assert.Equal(t, tt.wantShutdown, got.ShutdownTime)
		})
	}
}
