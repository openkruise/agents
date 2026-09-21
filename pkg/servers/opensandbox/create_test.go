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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

func newCreateRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, RoutePrefix+"/sandboxes", strings.NewReader(body))
}

func TestParseCreateSandboxRequest(t *testing.T) {
	const maxTimeout = 3600

	tests := []struct {
		name         string
		body         string
		maxTimeout   int
		wantErr      bool
		wantCode     int
		wantTimeout  int
		wantEnv      map[string]string
		wantPlatform bool
	}{
		{
			name:        "minimal image request defaults timeout",
			body:        `{"image":{"uri":"python:3.11"}}`,
			maxTimeout:  maxTimeout,
			wantTimeout: defaultTimeoutSeconds,
		},
		{
			name:        "explicit timeout and env are parsed",
			body:        `{"image":{"uri":"python:3.11"},"timeout":600,"env":{"A":"b"}}`,
			maxTimeout:  maxTimeout,
			wantTimeout: 600,
			wantEnv:     map[string]string{"A": "b"},
		},
		{
			name:         "top-level platform is parsed",
			body:         `{"image":{"uri":"python:3.11"},"platform":{"os":"linux","arch":"amd64"}}`,
			maxTimeout:   maxTimeout,
			wantTimeout:  defaultTimeoutSeconds,
			wantPlatform: true,
		},
		{
			name:       "snapshotId is rejected in Phase 1",
			body:       `{"snapshotId":"snap-1"}`,
			maxTimeout: maxTimeout,
			wantErr:    true,
			wantCode:   http.StatusBadRequest,
		},
		{
			name:       "missing image is rejected",
			body:       `{"timeout":600}`,
			maxTimeout: maxTimeout,
			wantErr:    true,
			wantCode:   http.StatusBadRequest,
		},
		{
			name:       "empty image uri is rejected",
			body:       `{"image":{"uri":"  "}}`,
			maxTimeout: maxTimeout,
			wantErr:    true,
			wantCode:   http.StatusBadRequest,
		},
		{
			name:       "timeout below minimum is rejected",
			body:       `{"image":{"uri":"python:3.11"},"timeout":5}`,
			maxTimeout: maxTimeout,
			wantErr:    true,
			wantCode:   http.StatusBadRequest,
		},
		{
			name:       "timeout above maximum is rejected",
			body:       `{"image":{"uri":"python:3.11"},"timeout":120}`,
			maxTimeout: 60,
			wantErr:    true,
			wantCode:   http.StatusBadRequest,
		},
		{
			name:       "unqualified metadata key is rejected",
			body:       `{"image":{"uri":"python:3.11"},"metadata":{"bad key!":"v"}}`,
			maxTimeout: maxTimeout,
			wantErr:    true,
			wantCode:   http.StatusBadRequest,
		},
		{
			name:       "reserved metadata prefix is rejected",
			body:       `{"image":{"uri":"python:3.11"},"metadata":{"agents.kruise.io/owner":"evil"}}`,
			maxTimeout: maxTimeout,
			wantErr:    true,
			wantCode:   http.StatusBadRequest,
		},
		{
			name:       "unknown field is rejected",
			body:       `{"image":{"uri":"python:3.11"},"bogusField":1}`,
			maxTimeout: maxTimeout,
			wantErr:    true,
			wantCode:   http.StatusBadRequest,
		},
		{
			name:       "malformed json is rejected",
			body:       `{"image":`,
			maxTimeout: maxTimeout,
			wantErr:    true,
			wantCode:   http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, apiErr := parseCreateSandboxRequest(newCreateRequest(t, tt.body), tt.maxTimeout)
			if tt.wantErr {
				require.NotNil(t, apiErr)
				assert.Equal(t, tt.wantCode, apiErr.Code)
				return
			}
			require.Nil(t, apiErr)
			assert.Equal(t, tt.wantTimeout, got.Timeout)
			if tt.wantEnv != nil {
				assert.Equal(t, tt.wantEnv, got.Env)
			}
			if tt.wantPlatform {
				require.NotNil(t, got.Platform)
				assert.Equal(t, "linux", got.Platform.OS)
				assert.Equal(t, "amd64", got.Platform.Arch)
			}
		})
	}
}

func TestConvertToOpenSandboxResponse(t *testing.T) {
	// Times are relative to now: a fixed date would age into the past and
	// flip GetState to dead once wall-clock passes ShutdownTime. Truncate
	// to seconds to match RFC3339 annotation round-trip precision.
	claimTime := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
	shutdownTime := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
	creationTime := time.Now().UTC().Add(-4 * time.Minute).Truncate(time.Second)

	tests := []struct {
		name           string
		sandbox        *agentsv1alpha1.Sandbox
		request        CreateSandboxRequest
		wantID         string
		wantState      SandboxState
		wantEntrypoint []string
		wantExpires    string
		wantCreated    string
		wantResource   string
	}{
		{
			name: "running and ready surfaces Running with timestamps",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "team-a",
					Name:      "sbx-1",
					Labels:    map[string]string{agentsv1alpha1.LabelSandboxID: "sbx-id-1"},
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationClaimTime: claimTime.Format(time.RFC3339),
					},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					ShutdownTime: &metav1.Time{Time: shutdownTime},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue},
					},
				},
			},
			request: CreateSandboxRequest{
				Entrypoint: []string{"python", "/app/main.py"},
				Metadata:   map[string]string{"user-key": "user-val"},
			},
			wantID:         "sbx-id-1",
			wantState:      SandboxStateRunning,
			wantEntrypoint: []string{"python", "/app/main.py"},
			wantExpires:    shutdownTime.Format(time.RFC3339),
			wantCreated:    claimTime.Format(time.RFC3339),
			wantResource:   "team-a/sbx-1",
		},
		{
			name: "claimed but not ready is surfaced as Running",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:         "team-a",
					Name:              "sbx-2",
					Labels:            map[string]string{agentsv1alpha1.LabelSandboxID: "sbx-id-2"},
					CreationTimestamp: metav1.Time{Time: creationTime},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionFalse},
					},
				},
			},
			wantID:         "sbx-id-2",
			wantState:      SandboxStateRunning,
			wantEntrypoint: []string{},
			wantCreated:    creationTime.Format(time.RFC3339),
			wantResource:   "team-a/sbx-2",
		},
		{
			name: "legacy id fallback when no sandbox-id label",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:         "team-b",
					Name:              "sbx-3",
					CreationTimestamp: metav1.Time{Time: creationTime},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue},
					},
				},
			},
			wantID:         "team-b--sbx-3",
			wantState:      SandboxStateRunning,
			wantEntrypoint: []string{},
			wantCreated:    creationTime.Format(time.RFC3339),
			wantResource:   "team-b/sbx-3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &sandboxcr.Sandbox{Sandbox: tt.sandbox}
			got := convertToOpenSandboxResponse(sbx, tt.request)

			assert.Equal(t, tt.wantID, got.ID)
			assert.Equal(t, tt.wantState, got.Status.State)
			assert.Equal(t, tt.wantEntrypoint, got.Entrypoint)
			assert.Equal(t, tt.wantExpires, got.ExpiresAt)
			assert.Equal(t, tt.wantCreated, got.CreatedAt)
			// The SDK response model pops entrypoint and createdAt
			// unconditionally, so both keys must be present in the
			// serialized body regardless of their values.
			raw, err := json.Marshal(got)
			require.NoError(t, err)
			var body map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &body))
			assert.Contains(t, body, "entrypoint")
			assert.Contains(t, body, "createdAt")
			// The protected resource-context key is always written last so
			// user metadata cannot spoof it.
			assert.Equal(t, tt.wantResource, got.Metadata[MetadataKeySandboxResource])
			// User metadata is echoed back when provided.
			for k, v := range tt.request.Metadata {
				assert.Equal(t, v, got.Metadata[k])
			}
		})
	}
}

func TestConvertToOpenSandboxResponseCreatedAtFallback(t *testing.T) {
	// Neither a claim-time annotation nor a creation timestamp: createdAt
	// falls back to now. The exact value is wall-clock dependent, so assert
	// presence and RFC3339 parseability instead of equality.
	sbx := &sandboxcr.Sandbox{Sandbox: &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "sbx-4"},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue},
			},
		},
	}}

	got := convertToOpenSandboxResponse(sbx, CreateSandboxRequest{})

	require.NotEmpty(t, got.CreatedAt)
	_, err := time.Parse(time.RFC3339, got.CreatedAt)
	assert.NoError(t, err)
}

func TestApplyCreateModifier(t *testing.T) {
	tests := []struct {
		name           string
		timeout        int
		metadata       map[string]string
		wantPauseZero  bool
		wantAnnotation map[string]string
	}{
		{
			name:          "timeout sets shutdown without pause",
			timeout:       600,
			wantPauseZero: true,
		},
		{
			name:           "metadata is persisted as annotations",
			timeout:        600,
			metadata:       map[string]string{"user-key": "user-val"},
			wantPauseZero:  true,
			wantAnnotation: map[string]string{"user-key": "user-val"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &sandboxcr.Sandbox{Sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "sbx-1"},
			}}
			before := time.Now()
			applyCreateModifier(sbx, CreateSandboxRequest{Timeout: tt.timeout, Metadata: tt.metadata})

			opts := sbx.GetTimeout()
			assert.True(t, opts.PauseTime.IsZero() == tt.wantPauseZero,
				"PauseTime zero mismatch: %v", opts.PauseTime)
			// ShutdownTime is normalized to whole seconds UTC; allow a small
			// window around the requested lifetime.
			wantShutdown := before.Add(time.Duration(tt.timeout) * time.Second)
			assert.WithinDuration(t, wantShutdown, opts.ShutdownTime, 5*time.Second)

			annotations := sbx.GetAnnotations()
			for k, v := range tt.wantAnnotation {
				assert.Equal(t, v, annotations[k])
			}
		})
	}
}

func TestMapInfraErrorToApiError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
	}{
		{name: "bad request maps to 400", err: managererrors.NewError(managererrors.ErrorBadRequest, "bad"), wantCode: http.StatusBadRequest},
		{name: "not found maps to 400", err: managererrors.NewError(managererrors.ErrorNotFound, "missing"), wantCode: http.StatusBadRequest},
		{name: "conflict maps to 409", err: managererrors.NewError(managererrors.ErrorConflict, "conflict"), wantCode: http.StatusConflict},
		{name: "quota exceeded maps to 403", err: managererrors.NewError(managererrors.ErrorQuotaExceeded, "quota"), wantCode: http.StatusForbidden},
		{name: "internal maps to 500", err: managererrors.NewError(managererrors.ErrorInternal, "boom"), wantCode: http.StatusInternalServerError},
		{name: "untyped error maps to 500", err: assert.AnError, wantCode: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapInfraErrorToApiError(tt.err)
			require.NotNil(t, got)
			assert.Equal(t, tt.wantCode, got.Code)
			assert.Equal(t, tt.err.Error(), got.Message)
		})
	}
}

// TestCreateSandbox_EarlyReturns covers the CreateSandbox branches that return
// before the manager is called, so they exercise the real handler with a nil
// manager. The claim orchestration itself is covered by the sandbox-manager
// test suite; standing up a warm pool here would duplicate that coverage.
func TestCreateSandbox_EarlyReturns(t *testing.T) {
	user := &models.CreatedTeamAPIKey{ID: uuid.New(), Name: "tester", Team: models.AdminTeam()}
	withUser := func(ctx context.Context) context.Context {
		return context.WithValue(ctx, userContextKey, user)
	}

	tests := []struct {
		name       string
		ctx        context.Context
		aliases    map[string]string
		body       string
		wantCode   int
		wantNilMgr bool
	}{
		{
			name:     "missing user in context is an internal error",
			ctx:      context.Background(),
			aliases:  map[string]string{"python:3.11": "python-tpl"},
			body:     `{"image":{"uri":"python:3.11"}}`,
			wantCode: http.StatusInternalServerError,
		},
		{
			name:     "invalid body is rejected before manager call",
			ctx:      withUser(context.Background()),
			aliases:  map[string]string{"python:3.11": "python-tpl"},
			body:     `{"snapshotId":"snap-1"}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:    "unmapped image is rejected before manager call",
			ctx:     withUser(context.Background()),
			aliases: map[string]string{"python:3.11": "python-tpl"},
			body:    `{"image":{"uri":"ruby:3.3"}}`,
			// Mirrors the reference server, which reports an
			// unavailable creation source as 500.
			wantCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// manager is nil: every case above must return before touching it.
			s := &Server{manager: nil, imageAliases: tt.aliases, maxTimeout: 3600}
			r := newCreateRequest(t, tt.body).WithContext(tt.ctx)

			resp, apiErr := s.CreateSandbox(r)
			require.NotNil(t, apiErr, "expected an error response")
			assert.Equal(t, tt.wantCode, apiErr.Code)
			assert.Equal(t, CreateSandboxResponse{}, resp.Body)
		})
	}
}
