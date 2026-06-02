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

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type waitHooksCache interface {
	GetWaitHooks() *sync.Map
}

func TestMapConnectResumeError(t *testing.T) {
	gvr := schema.GroupResource{Resource: "sandboxes"}
	tests := []struct {
		name         string
		err          error
		isSystem     bool
		expectStatus int
	}{
		{name: "not found maps to 404", err: apierrors.NewNotFound(gvr, "sbx"), isSystem: false, expectStatus: http.StatusNotFound},
		{name: "not found maps to 404 for system caller", err: apierrors.NewNotFound(gvr, "sbx"), isSystem: true, expectStatus: http.StatusNotFound},
		{name: "conflict non-system maps to 400", err: managererrors.NewError(managererrors.ErrorConflict, "conflict"), isSystem: false, expectStatus: http.StatusBadRequest},
		{name: "conflict system maps to 409", err: managererrors.NewError(managererrors.ErrorConflict, "conflict"), isSystem: true, expectStatus: http.StatusConflict},
		{name: "other maps to 500", err: errors.New("boom"), isSystem: false, expectStatus: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiErr := mapConnectResumeError(tt.err, tt.isSystem)
			assert.Equal(t, tt.expectStatus, apiErr.Code)
		})
	}
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
	expectEndAt := start.AddDate(1000, 0, 0)
	assert.WithinDuration(t, expectEndAt, endAt, 5*time.Second, "expect end at: %s, but got %s", expectEndAt, endAt)
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

func TestConnectSandbox_SystemCaller_NoToken(t *testing.T) {
	const defaultTimeoutSeconds = 300
	templateName := "test-template-system-connect-token"
	controller, _, teardown := Setup(t)
	defer teardown()
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
		Team: models.AdminTeam(),
	}
	cleanup := CreateSandboxPool(t, controller, templateName, 1)
	defer cleanup()

	createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
		Timeout:    defaultTimeoutSeconds,
		TemplateID: templateName,
		Metadata: map[string]string{
			models.ExtensionKeySkipInitRuntime: agentsv1alpha1.True,
		},
	}, nil, user))
	require.Nil(t, err)

	req := NewRequest(t, nil, models.SetTimeoutRequest{TimeoutSeconds: defaultTimeoutSeconds}, map[string]string{
		"sandboxID": createResp.Body.SandboxID,
	}, models.NewSystemUser(keys.SystemKeyNameConnect, keys.SystemKeyIDConnect))
	req = req.WithContext(WithSystemCaller(req.Context(), &SystemCaller{
		Name:       keys.SystemKeyNameConnect,
		ID:         keys.SystemKeyIDConnect,
		Scopes:     []keys.SystemAuth{keys.SystemAuthConnect},
		CrossOwner: true,
	}))

	connectResp, apiErr := controller.ConnectSandbox(req)
	require.Nil(t, apiErr)
	assert.Equal(t, http.StatusOK, connectResp.Code)
	assert.Empty(t, connectResp.Body.EnvdAccessToken)
}

func TestMapConnectResumeError_SystemCaller409NormalCaller400(t *testing.T) {
	tests := []struct {
		name           string
		err            error
		isSystemCaller bool
		expectStatus   int
	}{
		{
			name:           "normal caller manager conflict remains bad request",
			err:            managererrors.NewError(managererrors.ErrorConflict, "resume conflict"),
			isSystemCaller: false,
			expectStatus:   http.StatusBadRequest,
		},
		{
			name:           "system caller manager conflict returns conflict",
			err:            managererrors.NewError(managererrors.ErrorConflict, "resume conflict"),
			isSystemCaller: true,
			expectStatus:   http.StatusConflict,
		},
		{
			name:           "normal caller wait task conflict remains internal",
			err:            fmt.Errorf("resume failed: %w", cacheutils.ErrWaitTaskConflict),
			isSystemCaller: false,
			expectStatus:   http.StatusInternalServerError,
		},
		{
			name:           "system caller wait task conflict returns conflict",
			err:            fmt.Errorf("resume failed: %w", cacheutils.ErrWaitTaskConflict),
			isSystemCaller: true,
			expectStatus:   http.StatusConflict,
		},
		{
			name:           "unknown error remains internal",
			err:            errors.New("resume failed"),
			isSystemCaller: true,
			expectStatus:   http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiErr := mapConnectResumeError(tt.err, tt.isSystemCaller)
			require.NotNil(t, apiErr)
			assert.Equal(t, tt.expectStatus, apiErr.Code)
			assert.Contains(t, apiErr.Message, tt.err.Error())
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

// pickPlaceholderDeadline returns the spec field that buildSetTimeoutOptions
// populates for a (autoPause) sandbox: PauseTime when auto-pause is enabled,
// ShutdownTime otherwise.
func pickPlaceholderDeadline(sbx *agentsv1alpha1.Sandbox, autoPause bool) (*metav1.Time, string) {
	if autoPause {
		return sbx.Spec.PauseTime, "PauseTime"
	}
	return sbx.Spec.ShutdownTime, "ShutdownTime"
}

// placeholderAssertion returns the hook the Resume-wait observer fires when
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
