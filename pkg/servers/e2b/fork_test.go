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
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

func TestParseForkSandboxRequest(t *testing.T) {
	controller := &Controller{maxTimeout: 60}
	tests := []struct {
		name        string
		body        string
		expect      forkSandboxRequest
		expectError string
	}{
		{
			name:   "empty body uses E2B defaults",
			body:   "",
			expect: forkSandboxRequest{timeout: defaultForkTimeoutSeconds, count: 1},
		},
		{
			name:   "empty object uses E2B defaults",
			body:   "{}",
			expect: forkSandboxRequest{timeout: defaultForkTimeoutSeconds, count: 1},
		},
		{
			name:   "explicit values",
			body:   `{"timeout": 42, "count": 3}`,
			expect: forkSandboxRequest{timeout: 42, count: 3},
		},
		{
			name:        "zero timeout is rejected",
			body:        `{"timeout": 0}`,
			expectError: "timeout should be between 1 and 60",
		},
		{
			name:        "timeout above server limit is rejected",
			body:        `{"timeout": 61}`,
			expectError: "timeout should be between 1 and 60",
		},
		{
			name:        "zero count is rejected",
			body:        `{"count": 0}`,
			expectError: "count should be between 1 and 100",
		},
		{
			name:        "count above E2B limit is rejected",
			body:        `{"count": 101}`,
			expectError: "count should be between 1 and 100",
		},
		{
			name:        "unknown field is rejected",
			body:        `{"unknown": true}`,
			expectError: "failed to decode fork request",
		},
		{
			name:        "trailing JSON is rejected",
			body:        `{} {}`,
			expectError: "fork request must contain a single JSON object",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/sandboxes/source/fork", bytes.NewBufferString(tt.body))
			got, apiErr := controller.parseForkSandboxRequest(request)
			if tt.expectError != "" {
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusBadRequest, apiErr.Code)
				assert.Contains(t, apiErr.Message, tt.expectError)
				return
			}
			require.Nil(t, apiErr)
			assert.Equal(t, tt.expect, got)
		})
	}
}

func TestForkSandboxCreatesMultipleChildren(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	const checkpointID = "fork-checkpoint-id"
	origCreateCheckpoint := sandboxcr.DefaultCreateCheckpoint
	sandboxcr.DefaultCreateCheckpoint = func(ctx context.Context, client ctrlclient.Client, checkpoint *v1alpha1.Checkpoint) (*v1alpha1.Checkpoint, error) {
		created, err := origCreateCheckpoint(ctx, client, checkpoint)
		if err != nil {
			return nil, err
		}
		created.Status = v1alpha1.CheckpointStatus{
			Phase:        v1alpha1.CheckpointSucceeded,
			CheckpointId: checkpointID,
		}
		if err := client.Status().Update(ctx, created); err != nil {
			return nil, err
		}
		return created, nil
	}
	t.Cleanup(func() { sandboxcr.DefaultCreateCheckpoint = origCreateCheckpoint })

	var childNumber atomic.Int32
	origCreateSandbox := sandboxcr.DefaultCreateSandbox
	sandboxcr.DefaultCreateSandbox = func(ctx context.Context, sandbox *v1alpha1.Sandbox, client ctrlclient.Client) (*v1alpha1.Sandbox, error) {
		if sandbox.Name == "" && sandbox.GenerateName != "" {
			sandbox.Name = sandbox.GenerateName + strconv.Itoa(int(childNumber.Add(1)))
		}
		created, err := origCreateSandbox(ctx, sandbox, client)
		if err != nil {
			return nil, err
		}
		created.Status = v1alpha1.SandboxStatus{
			Phase:              v1alpha1.SandboxRunning,
			ObservedGeneration: created.Generation,
			Conditions: []metav1.Condition{{
				Type:   string(v1alpha1.SandboxConditionReady),
				Status: metav1.ConditionTrue,
				Reason: v1alpha1.SandboxReadyReasonPodReady,
			}},
			PodInfo: v1alpha1.PodInfo{PodIP: "1.2.3.4"},
		}
		if err := client.Status().Update(ctx, created); err != nil {
			return nil, err
		}
		return created, nil
	}
	t.Cleanup(func() { sandboxcr.DefaultCreateSandbox = origCreateSandbox })

	user := adminTestUser()
	source := CreateClaimedSandboxCR(t, controller, Namespace, "fork-success-source", "fork-template", user.ID.String(), nil)
	sandboxID := source.Namespace + "--" + source.Name
	count := 2
	req := NewRequest(t, nil, models.ForkSandboxRequest{Count: &count}, map[string]string{"sandboxID": sandboxID}, user)

	resp, apiErr := controller.ForkSandbox(req)

	require.Nil(t, apiErr)
	require.Equal(t, http.StatusCreated, resp.Code)
	require.Len(t, resp.Body, count)
	for _, result := range resp.Body {
		require.NotNil(t, result.Sandbox)
		assert.Nil(t, result.Error)
		assert.NotEmpty(t, result.Sandbox.SandboxID)
	}
	assert.NotEqual(t, resp.Body[0].Sandbox.SandboxID, resp.Body[1].Sandbox.SandboxID)
}

func TestForkSandboxRejectsNonRunningClaimedSource(t *testing.T) {
	controller, client, teardown := Setup(t)
	defer teardown()

	user := adminTestUser()
	tests := []struct {
		name          string
		sandboxName   string
		phase         v1alpha1.SandboxPhase
		pausedStatus  metav1.ConditionStatus
		readyStatus   metav1.ConditionStatus
		expectedState string
		expectedCode  int
	}{
		{
			name:          "paused source returns conflict",
			sandboxName:   "paused-fork-source",
			phase:         v1alpha1.SandboxPaused,
			pausedStatus:  metav1.ConditionTrue,
			readyStatus:   metav1.ConditionFalse,
			expectedState: v1alpha1.SandboxStatePaused,
			expectedCode:  http.StatusConflict,
		},
		{
			name:          "terminal dead source returns not found",
			sandboxName:   "dead-fork-source",
			phase:         v1alpha1.SandboxTerminating,
			readyStatus:   metav1.ConditionFalse,
			expectedState: v1alpha1.SandboxStateDead,
			expectedCode:  http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := CreateClaimedSandboxCR(t, controller, Namespace, tt.sandboxName, "fork-template", user.ID.String(), nil)
			sandboxID := source.Namespace + "--" + source.Name
			UpdateSandboxWhen(t, client, sandboxID, Immediately, DoSetSandboxStatus(tt.phase, tt.pausedStatus, tt.readyStatus))

			req := NewRequest(t, nil, models.ForkSandboxRequest{}, map[string]string{"sandboxID": sandboxID}, user)
			require.Eventually(t, func() bool {
				got, apiErr := controller.getSandboxOfUser(req.Context(), sandboxID, claimedSandboxStates)
				if apiErr != nil {
					return false
				}
				state, _ := got.GetState()
				return state == tt.expectedState
			}, time.Second, 10*time.Millisecond, "cache should observe the updated source state")

			resp, apiErr := controller.ForkSandbox(req)
			assert.Empty(t, resp.Body)
			require.NotNil(t, apiErr)
			assert.Equal(t, tt.expectedCode, apiErr.Code)
			assert.Contains(t, apiErr.Message, "sandboxResource=")
		})
	}
}

func TestForkSandboxRejectsMissingUser(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	resp, apiErr := controller.ForkSandbox(NewRequest(t, nil, models.ForkSandboxRequest{}, map[string]string{"sandboxID": "source"}, nil))

	assert.Empty(t, resp.Body)
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusUnauthorized, apiErr.Code)
}

func TestForkSandboxMapsCheckpointFailure(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	user := adminTestUser()
	source := CreateClaimedSandboxCR(t, controller, Namespace, "fork-checkpoint-failure", "fork-template", user.ID.String(), nil)
	origCreateCheckpoint := sandboxcr.DefaultCreateCheckpoint
	sandboxcr.DefaultCreateCheckpoint = func(context.Context, ctrlclient.Client, *v1alpha1.Checkpoint) (*v1alpha1.Checkpoint, error) {
		return nil, errors.New("checkpoint backend failed")
	}
	t.Cleanup(func() { sandboxcr.DefaultCreateCheckpoint = origCreateCheckpoint })

	resp, apiErr := controller.ForkSandbox(NewRequest(t, nil, models.ForkSandboxRequest{}, map[string]string{"sandboxID": source.Namespace + "--" + source.Name}, user))

	assert.Empty(t, resp.Body)
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusInternalServerError, apiErr.Code)
	assert.Contains(t, apiErr.Message, "failed while creating checkpoint")
}

func TestForkErrorCodeMappings(t *testing.T) {
	requestTests := []struct {
		name string
		err  error
		want int
	}{
		{name: "bad request", err: managererrors.NewError(managererrors.ErrorBadRequest, "bad request"), want: http.StatusBadRequest},
		{name: "not found", err: managererrors.NewError(managererrors.ErrorNotFound, "not found"), want: http.StatusNotFound},
		{name: "not allowed", err: managererrors.NewError(managererrors.ErrorNotAllowed, "not allowed"), want: http.StatusNotFound},
		{name: "conflict", err: managererrors.NewError(managererrors.ErrorConflict, "conflict"), want: http.StatusConflict},
		{name: "unavailable", err: managererrors.NewError(managererrors.ErrorUnavailable, "unavailable"), want: http.StatusServiceUnavailable},
		{name: "unknown", err: errors.New("unknown"), want: http.StatusInternalServerError},
	}
	for _, tt := range requestTests {
		t.Run("request/"+tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, forkRequestErrorCode(tt.err))
		})
	}

	itemTests := []struct {
		name string
		err  error
		want int32
	}{
		{name: "quota exceeded", err: managererrors.NewError(managererrors.ErrorQuotaExceeded, "quota exceeded"), want: http.StatusTooManyRequests},
		{name: "conflict", err: managererrors.NewError(managererrors.ErrorConflict, "conflict"), want: http.StatusConflict},
		{name: "unavailable", err: managererrors.NewError(managererrors.ErrorUnavailable, "unavailable"), want: http.StatusServiceUnavailable},
		{name: "other manager error", err: managererrors.NewError(managererrors.ErrorBadRequest, "bad request"), want: http.StatusInternalServerError},
		{name: "unknown", err: errors.New("unknown"), want: http.StatusInternalServerError},
	}
	for _, tt := range itemTests {
		t.Run("item/"+tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, forkItemErrorCode(tt.err))
		})
	}
}

func TestForkRoutesRegistered(t *testing.T) {
	for _, path := range []string{
		"/sandboxes/source/fork",
		adapters.CustomPrefix + "/api/sandboxes/source/fork",
	} {
		t.Run(path, func(t *testing.T) {
			storage := &lookupKeyStorage{}
			controller := &Controller{mux: http.NewServeMux(), keys: storage}
			controller.registerRoutes()

			req := httptest.NewRequest(http.MethodPost, path, nil)
			req.Header.Set(models.HeaderApiKey, "invalid-key")
			rec := httptest.NewRecorder()
			controller.mux.ServeHTTP(rec, req)

			require.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Equal(t, []string{"invalid-key"}, storage.calls)
			assert.Equal(t, []string{traceOpFork}, storage.operations)
		})
	}
}
