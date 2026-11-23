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

package opensandbox

import (
	"net/http"
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
	"github.com/openkruise/agents/pkg/sandboxstate"
)

func TestMapState(t *testing.T) {
	tests := []struct {
		name        string
		agentsState string
		reason      string
		want        SandboxState
	}{
		{
			name:        "running maps to Running",
			agentsState: sandboxstate.Running,
			reason:      "RunningResourceClaimedAndReady",
			want:        SandboxStateRunning,
		},
		{
			name:        "claimed but not ready remains Pending",
			agentsState: sandboxstate.Provisioning,
			reason:      "RunningResourceClaimedButNotReady",
			want:        SandboxStatePending,
		},
		{
			name:        "paused maps to Paused",
			agentsState: sandboxstate.Paused,
			reason:      "RunningResourceClaimedAndPaused",
			want:        SandboxStatePaused,
		},
		{
			name:        "creating maps to Pending",
			agentsState: sandboxstate.Creating,
			reason:      "ResourcePending",
			want:        SandboxStatePending,
		},
		{
			name:        "dead with terminal reason maps to Terminated",
			agentsState: sandboxstate.Dead,
			reason:      "ResourceTerminating",
			want:        SandboxStateTerminated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MapState(tt.agentsState, tt.reason))
		})
	}
}

func TestMapStatePhase(t *testing.T) {
	tests := []struct {
		name     string
		state    string
		deleting bool
		phase    string
		want     SandboxState
		wantErr  bool
	}{
		{name: "creating is Pending", state: sandboxstate.Creating, phase: string(agentsv1alpha1.SandboxPending), want: SandboxStatePending},
		{name: "available pool is Pending", state: sandboxstate.Available, phase: string(agentsv1alpha1.SandboxRunning), want: SandboxStatePending},
		{name: "running is Running", state: sandboxstate.Running, phase: string(agentsv1alpha1.SandboxRunning), want: SandboxStateRunning},
		{name: "pause accepted while running is Pausing", state: sandboxstate.Paused, phase: string(agentsv1alpha1.SandboxRunning), want: SandboxStatePausing},
		{name: "paused phase is Paused", state: sandboxstate.Paused, phase: string(agentsv1alpha1.SandboxPaused), want: SandboxStatePaused},
		{name: "resuming phase is Resuming", state: sandboxstate.Paused, phase: string(agentsv1alpha1.SandboxResuming), want: SandboxStateResuming},
		{name: "upgrading phase is Running", state: sandboxstate.Paused, phase: string(agentsv1alpha1.SandboxUpgrading), want: SandboxStateRunning},
		{name: "claimed but not ready is Pending", state: sandboxstate.Provisioning, phase: string(agentsv1alpha1.SandboxRunning), want: SandboxStatePending},
		{name: "terminating is Stopping", state: sandboxstate.Dead, phase: string(agentsv1alpha1.SandboxTerminating), want: SandboxStateStopping},
		{name: "failed is Failed", state: sandboxstate.Dead, phase: string(agentsv1alpha1.SandboxFailed), want: SandboxStateFailed},
		{name: "deleted is Terminated", state: sandboxstate.Dead, deleting: true, phase: string(agentsv1alpha1.SandboxTerminating), want: SandboxStateTerminated},
		{name: "succeeded is Terminated", state: sandboxstate.Dead, phase: string(agentsv1alpha1.SandboxSucceeded), want: SandboxStateTerminated},
		{name: "shutdown time reached is Terminated", state: sandboxstate.Expired, phase: string(agentsv1alpha1.SandboxRunning), want: SandboxStateTerminated},
		{name: "unknown state is an error", state: "some-future-state", phase: "Running", wantErr: true},
		{name: "dead without a terminal phase or deletion is an error", state: sandboxstate.Dead, phase: string(agentsv1alpha1.SandboxRunning), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mapStatePhase(tt.state, tt.phase, tt.deleting)
			if tt.wantErr {
				require.Error(t, err)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestClaimedSandboxStateProjection(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*agentsv1alpha1.Sandbox)
		state  SandboxState
	}{
		{"provisioning", func(s *agentsv1alpha1.Sandbox) { s.Status.Conditions = nil }, SandboxStatePending},
		{"expired", func(s *agentsv1alpha1.Sandbox) {
			deadline := metav1.NewTime(time.Now().Add(-time.Minute))
			s.Spec.ShutdownTime = &deadline
		}, SandboxStateTerminated},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sbx := runningClaimedSandbox(t, "default", "sbx-1", "id", tt.mutate)
			state, reason := sbx.GetState()
			require.Contains(t, claimedSandboxStates, state)
			require.NotContains(t, liveSandboxStates, state)

			created := convertToOpenSandboxResponse(sbx, CreateSandboxRequest{})
			require.Equal(t, tt.state, created.Status.State)
			require.Equal(t, reason, created.Status.Reason)
			s := &Server{}
			described, apiErr := s.DescribeSandbox(newSandboxScopedRequest(t, http.MethodGet, "", sbx))
			require.Nil(t, apiErr)
			require.Equal(t, created.Status, described.Body.Status)
			require.True(t, buildListFilter([]SandboxState{tt.state}, nil)(sbx))
			require.False(t, buildListFilter([]SandboxState{SandboxStateRunning}, nil)(sbx))
		})
	}
}

func TestMapLifecycleErrorToApiError(t *testing.T) {
	notFoundK8s := apierrors.NewNotFound(schema.GroupResource{Group: "apps.kruise.io", Resource: "sandboxes"}, "sbx-1")

	tests := []struct {
		name     string
		err      error
		wantCode int
	}{
		{name: "k8s not found maps to 404", err: notFoundK8s, wantCode: http.StatusNotFound},
		{name: "manager not found maps to 404", err: managererrors.NewError(managererrors.ErrorNotFound, "missing"), wantCode: http.StatusNotFound},
		{name: "bad request maps to 400", err: managererrors.NewError(managererrors.ErrorBadRequest, "bad"), wantCode: http.StatusBadRequest},
		{name: "conflict maps to 409", err: managererrors.NewError(managererrors.ErrorConflict, "conflict"), wantCode: http.StatusConflict},
		{name: "wait task conflict maps to 409", err: cacheutils.ErrWaitTaskConflict, wantCode: http.StatusConflict},
		{name: "quota exceeded maps to 403", err: managererrors.NewError(managererrors.ErrorQuotaExceeded, "quota"), wantCode: http.StatusForbidden},
		{name: "internal maps to 500", err: managererrors.NewError(managererrors.ErrorInternal, "boom"), wantCode: http.StatusInternalServerError},
		{name: "untyped error maps to 500", err: assert.AnError, wantCode: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapLifecycleErrorToApiError(tt.err)
			require.NotNil(t, got)
			assert.Equal(t, tt.wantCode, got.Code)
			assert.Equal(t, tt.err.Error(), got.Message)
		})
	}
}

func TestGetSandboxErrorCode(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
	}{
		{name: "not found is 404", err: managererrors.NewError(managererrors.ErrorNotFound, "missing"), wantCode: http.StatusNotFound},
		{name: "not allowed is 404 (anti-enumeration)", err: managererrors.NewError(managererrors.ErrorNotAllowed, "foreign"), wantCode: http.StatusNotFound},
		{name: "bad request state is 404 (anti-enumeration)", err: managererrors.NewError(managererrors.ErrorBadRequest, "wrong state"), wantCode: http.StatusNotFound},
		{name: "internal is 500", err: managererrors.NewError(managererrors.ErrorInternal, "cache outage"), wantCode: http.StatusInternalServerError},
		{name: "untyped is 404", err: assert.AnError, wantCode: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantCode, getSandboxErrorCode(tt.err))
		})
	}
}
