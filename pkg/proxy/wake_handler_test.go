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

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/servers/web"
)

func TestHandleWakeActionStatus(t *testing.T) {
	tests := []struct {
		name       string
		action     WakeAction
		expectCode int
	}{
		{
			name:       "resumed returns ok",
			action:     WakeActionResumed,
			expectCode: http.StatusOK,
		},
		{
			name:       "already running returns ok",
			action:     WakeActionAlreadyRunning,
			expectCode: http.StatusOK,
		},
		{
			name:       "auto resume disabled returns unprocessable entity",
			action:     WakeActionAutoResumeDisabled,
			expectCode: http.StatusUnprocessableEntity,
		},
		{
			name:       "invalid auto resume policy returns unprocessable entity",
			action:     WakeActionInvalidAutoResumePolicy,
			expectCode: http.StatusUnprocessableEntity,
		},
		{
			name:       "pausing returns conflict",
			action:     WakeActionPausing,
			expectCode: http.StatusConflict,
		},
		{
			name:       "bad state returns conflict",
			action:     WakeActionBadState,
			expectCode: http.StatusConflict,
		},
		{
			name:       "gone returns gone",
			action:     WakeActionGone,
			expectCode: http.StatusGone,
		},
		{
			name:       "not found returns not found",
			action:     WakeActionNotFound,
			expectCode: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewServer(config.SandboxManagerOptions{})
			s.SetWakeFunc(func(ctx context.Context, id string) (WakeResult, error) {
				assert.Equal(t, "sandbox-1", id)
				return WakeResult{
					Action:          tt.action,
					Message:         "wake result",
					State:           "paused",
					ResourceVersion: "10",
				}, nil
			})
			mux := http.NewServeMux()
			web.RegisterRoute(mux, http.MethodPost, WakeAPI, s.handleWake)

			req := httptest.NewRequest(http.MethodPost, "/wake/sandbox-1", nil)
			rr := httptest.NewRecorder()

			mux.ServeHTTP(rr, req)

			assert.Equal(t, tt.expectCode, rr.Code)
			var body WakeResult
			require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
			assert.Equal(t, tt.action, body.Action)
			assert.Equal(t, "wake result", body.Message)
			assert.Equal(t, "paused", body.State)
			assert.Equal(t, "10", body.ResourceVersion)
		})
	}
}

func TestHandleWakeJSONIncludesEmptyFields(t *testing.T) {
	s := NewServer(config.SandboxManagerOptions{})
	s.SetWakeFunc(func(ctx context.Context, id string) (WakeResult, error) {
		assert.Equal(t, "sandbox-empty-fields", id)
		return WakeResult{Action: WakeActionAlreadyRunning}, nil
	})
	mux := http.NewServeMux()
	web.RegisterRoute(mux, http.MethodPost, WakeAPI, s.handleWake)

	req := httptest.NewRequest(http.MethodPost, "/wake/sandbox-empty-fields", nil)
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	var body map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal(t, string(WakeActionAlreadyRunning), body["action"])
	assert.Contains(t, body, "message")
	assert.Contains(t, body, "state")
	assert.Contains(t, body, "resourceVersion")
	assert.Equal(t, "", body["message"])
	assert.Equal(t, "", body["state"])
	assert.Equal(t, "", body["resourceVersion"])
}

func TestHandleWakeUnsupportedAction(t *testing.T) {
	s := NewServer(config.SandboxManagerOptions{})
	s.SetWakeFunc(func(ctx context.Context, id string) (WakeResult, error) {
		assert.Equal(t, "sandbox-unknown", id)
		return WakeResult{Action: WakeAction("Unknown")}, nil
	})
	mux := http.NewServeMux()
	web.RegisterRoute(mux, http.MethodPost, WakeAPI, s.handleWake)

	req := httptest.NewRequest(http.MethodPost, "/wake/sandbox-unknown", nil)
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
	var body web.ApiError
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	assert.Contains(t, body.Message, "unsupported wake action")
}

func TestHandleWakeEmptyBodyWorksThroughMux(t *testing.T) {
	s := NewServer(config.SandboxManagerOptions{})
	s.SetWakeFunc(func(ctx context.Context, id string) (WakeResult, error) {
		assert.Equal(t, "sandbox-empty-body", id)
		return WakeResult{
			Action:          WakeActionResumed,
			Message:         "resumed",
			State:           "running",
			ResourceVersion: "11",
		}, nil
	})
	mux := http.NewServeMux()
	web.RegisterRoute(mux, http.MethodPost, "/wake/{sandboxID}", s.handleWake)

	req := httptest.NewRequest(http.MethodPost, "/wake/sandbox-empty-body", nil)
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	var body WakeResult
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal(t, WakeActionResumed, body.Action)
	assert.Equal(t, "resumed", body.Message)
	assert.Equal(t, "running", body.State)
	assert.Equal(t, "11", body.ResourceVersion)
}

func TestHandleWakeError(t *testing.T) {
	s := NewServer(config.SandboxManagerOptions{})
	s.SetWakeFunc(func(ctx context.Context, id string) (WakeResult, error) {
		return WakeResult{}, errors.New("wake failed")
	})

	req := httptest.NewRequest(http.MethodPost, "/wake/sandbox-1", strings.NewReader(""))
	req.SetPathValue("sandboxID", "sandbox-1")

	resp, apiErr := s.handleWake(req)

	assert.Empty(t, resp.Code)
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusInternalServerError, apiErr.Code)
	assert.Contains(t, apiErr.Message, "wake failed")
}

func TestHandleWakeMissingWakeFunc(t *testing.T) {
	s := NewServer(config.SandboxManagerOptions{})
	req := httptest.NewRequest(http.MethodPost, "/wake/sandbox-1", nil)
	req.SetPathValue("sandboxID", "sandbox-1")

	resp, apiErr := s.handleWake(req)

	assert.Empty(t, resp.Code)
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusInternalServerError, apiErr.Code)
	assert.Contains(t, apiErr.Message, "wake function is not configured")
}
