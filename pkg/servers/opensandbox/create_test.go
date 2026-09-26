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
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

func newCreateRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, RoutePrefix+"/sandboxes", strings.NewReader(body))
}

// Valid image shape from the pinned OpenAPI; extra contains comma-prefixed fields.
func imageRequest(extra string) string {
	return `{"image":{"uri":"python:3.11"},"entrypoint":["sleep","600"],"resourceLimits":{}` + extra + `}`
}

func TestParseCreateSandboxRequest(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		maximum     int
		wantTimeout *int64
		wantError   string
	}{
		{name: "omitted lifetime is unlimited", body: imageRequest("")},
		{name: "null lifetime is unlimited", body: imageRequest(`,"timeout":null`)},
		{name: "minimum lifetime", body: imageRequest(`,"timeout":60`), wantTimeout: ptr.To[int64](60)},
		{name: "configured maximum inclusive", body: imageRequest(`,"timeout":600`), maximum: 600, wantTimeout: ptr.To[int64](600)},
		{name: "zero is not omission", body: imageRequest(`,"timeout":0`), wantError: "at least 60"},
		{name: "old E2B minimum rejected", body: imageRequest(`,"timeout":30`), wantError: "at least 60"},
		{name: "59 seconds rejected", body: imageRequest(`,"timeout":59`), wantError: "at least 60"},
		{name: "above maximum", body: imageRequest(`,"timeout":601`), maximum: 600, wantError: "server maximum"},
		{name: "duration overflow rejected", body: imageRequest(fmt.Sprintf(`,"timeout":%d`, maxTimeoutSeconds+1)), wantError: "server maximum"},
		{name: "timeout is integer", body: imageRequest(`,"timeout":60.5`), wantError: "invalid request body"},
		{name: "timeout string rejected", body: imageRequest(`,"timeout":"60"`), wantError: "invalid request body"},
		{name: "missing source", body: `{}`, wantError: "image.uri"},
		{name: "empty URI", body: `{"image":{"uri":" "}}`, wantError: "image.uri"},
		{name: "missing entrypoint", body: `{"image":{"uri":"python"},"resourceLimits":{}}`, wantError: "entrypoint"},
		{name: "empty entrypoint", body: `{"image":{"uri":"python"},"resourceLimits":{},"entrypoint":[]}`, wantError: "entrypoint"},
		{name: "null entrypoint", body: `{"image":{"uri":"python"},"resourceLimits":{},"entrypoint":null}`, wantError: "entrypoint"},
		{name: "missing resources", body: `{"image":{"uri":"python"},"entrypoint":["sh"]}`, wantError: "resourceLimits"},
		{name: "null resources", body: `{"image":{"uri":"python"},"entrypoint":["sh"],"resourceLimits":null}`, wantError: "resourceLimits"},
		{name: "platform incomplete", body: imageRequest(`,"platform":{"os":"linux"}`), wantError: "platform.os and platform.arch"},
		{name: "platform complete", body: imageRequest(`,"platform":{"os":"linux","arch":"amd64"}`)},
		{name: "invalid metadata key", body: imageRequest(`,"metadata":{"bad key!":"v"}`), wantError: "unqualified metadata"},
		{name: "reserved metadata", body: imageRequest(`,"metadata":{"agents.kruise.io/owner":"evil"}`), wantError: "Forbidden metadata"},
		{name: "unknown field", body: imageRequest(`,"bogusField":1`), wantError: "unknown field"},
		{name: "second JSON value", body: imageRequest("") + ` {}`, wantError: "exactly one JSON"},
		{name: "null suffix", body: imageRequest("") + ` null`, wantError: "exactly one JSON"},
		{name: "malformed suffix", body: imageRequest("") + ` garbage`, wantError: "exactly one JSON"},
		{name: "malformed body", body: `{"image":`, wantError: "invalid request body"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, apiErr := parseCreateSandboxRequest(newCreateRequest(t, tt.body), tt.maximum)
			if tt.wantError != "" {
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusBadRequest, apiErr.Code)
				assert.Contains(t, apiErr.Message, tt.wantError)
				return
			}
			require.Nil(t, apiErr)
			assert.Equal(t, tt.wantTimeout, got.Timeout)
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
			name: "claimed but not ready is surfaced as Pending",
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
			wantState:      SandboxStatePending,
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
			metadataBefore := maps.Clone(tt.request.Metadata)
			got := convertToOpenSandboxResponse(sbx, tt.request)
			assert.Equal(t, metadataBefore, tt.request.Metadata, "response construction must not mutate caller metadata")

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
	for _, tt := range []struct {
		name     string
		lifetime *int64
	}{
		{name: "unlimited"},
		{name: "finite", lifetime: ptr.To[int64](600)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lifetime := tt.lifetime
			oldDeadline := metav1.NewTime(time.Now().Add(time.Hour))
			sbx := &sandboxcr.Sandbox{Sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "sbx-1", Annotations: map[string]string{"system": "keep"}},
				Spec:       agentsv1alpha1.SandboxSpec{PauseTime: &oldDeadline, ShutdownTime: &oldDeadline},
			}}
			before := time.Now()
			applyCreateModifier(sbx, CreateSandboxRequest{Timeout: lifetime, Metadata: map[string]string{"user-key": "user-value"}})
			assert.Nil(t, sbx.Spec.PauseTime, "clear inherited auto-pause deadline")
			if lifetime == nil {
				assert.Nil(t, sbx.Spec.ShutdownTime, "clear inherited shutdown deadline")
			} else {
				require.NotNil(t, sbx.Spec.ShutdownTime)
				assert.WithinDuration(t, before.Add(time.Duration(*lifetime)*time.Second), sbx.Spec.ShutdownTime.Time, time.Second)
			}
			assert.Equal(t, "keep", sbx.Annotations["system"])
			assert.Equal(t, "user-value", sbx.Annotations["user-key"])
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
			body:    `{"image":{"uri":"ruby:3.3"},"entrypoint":["sh"],"resourceLimits":{}}`,
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

func TestCreateHTTPContract(t *testing.T) {
	mux := http.NewServeMux()
	require.NoError(t, RegisterRoutes(Deps{Mux: mux, Manager: &sandboxmanager.SandboxManager{}, MaxTimeout: 3600}))
	w := httptest.NewRecorder()
	r := newCreateRequest(t, imageRequest(`,"timeout":0`))
	mux.ServeHTTP(w, r)
	require.Equal(t, 400, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Len(t, body, 2)
	assert.Equal(t, "INVALID_REQUEST", body["code"])
	assert.NotEmpty(t, w.Header().Get("X-Request-ID"))

	// Authentication failures go through the same formatter as handler errors.
	authMux := http.NewServeMux()
	require.NoError(t, RegisterRoutes(Deps{Mux: authMux, Manager: &sandboxmanager.SandboxManager{}, Keys: &fakeKeyStorage{}}))
	w = httptest.NewRecorder()
	authMux.ServeHTTP(w, newCreateRequest(t, imageRequest("")))
	require.Equal(t, 401, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Len(t, body, 2)
	assert.Equal(t, "UNAUTHORIZED", body["code"])
}
