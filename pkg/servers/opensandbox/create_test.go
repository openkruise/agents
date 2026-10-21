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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
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
		name         string
		body         string
		maximum      int
		wantTimeout  *int64
		wantError    string
		wantSource   createSource
		wantProxy    bool
		wantRequests *ResourceLimits
	}{
		{name: "omitted lifetime uses E2B default", body: imageRequest("")},
		{name: "null lifetime uses E2B default", body: imageRequest(`,"timeout":null`)},
		{name: "explicit lifetime", body: imageRequest(`,"timeout":60`), wantTimeout: ptr.To[int64](60)},
		{name: "configured maximum inclusive", body: imageRequest(`,"timeout":600`), maximum: 600, wantTimeout: ptr.To[int64](600)},
		{name: "zero lifetime uses E2B default", body: imageRequest(`,"timeout":0`)},
		{name: "E2B minimum accepted", body: imageRequest(`,"timeout":30`), wantTimeout: ptr.To[int64](models.DefaultMinTimeoutSeconds)},
		{name: "59 seconds accepted", body: imageRequest(`,"timeout":59`), wantTimeout: ptr.To[int64](59)},
		{name: "below E2B minimum", body: imageRequest(`,"timeout":29`), wantError: "timeout should between"},
		{name: "negative lifetime", body: imageRequest(`,"timeout":-1`), wantError: "timeout should between"},
		{name: "default exceeds configured maximum", body: imageRequest(""), maximum: 60, wantError: "timeout should between"},
		{name: "above maximum", body: imageRequest(`,"timeout":601`), maximum: 600, wantError: "timeout should between"},
		{name: "huge duration exceeds configured maximum", body: imageRequest(fmt.Sprintf(`,"timeout":%d`, int64(1<<63-1))), wantError: "timeout should between"},
		{name: "timeout is integer", body: imageRequest(`,"timeout":60.5`), wantError: "invalid request body"},
		{name: "timeout string rejected", body: imageRequest(`,"timeout":"60"`), wantError: "invalid request body"},
		{name: "missing source", body: `{}`, wantError: "image.uri"},
		{name: "empty URI", body: `{"image":{"uri":" "}}`, wantError: "image.uri"},
		{name: "missing entrypoint", body: `{"image":{"uri":"python"},"resourceLimits":{}}`},
		{name: "empty entrypoint", body: `{"image":{"uri":"python"},"resourceLimits":{},"entrypoint":[]}`},
		{name: "null entrypoint", body: `{"image":{"uri":"python"},"resourceLimits":{},"entrypoint":null}`},
		{name: "missing resources", body: `{"image":{"uri":"python"},"entrypoint":["sh"]}`},
		{name: "null resources", body: `{"image":{"uri":"python"},"entrypoint":["sh"],"resourceLimits":null}`},
		{name: "platform incomplete", body: imageRequest(`,"platform":{"os":"linux"}`)},
		{name: "platform complete", body: imageRequest(`,"platform":{"os":"linux","arch":"amd64"}`)},
		{name: "metadata value uses annotation rules", body: imageRequest(fmt.Sprintf(`,"metadata":{"name":"%s"}`, strings.Repeat("space value ", 10)))},
		{name: "OpenSandbox metadata prefix is not native-reserved", body: imageRequest(`,"metadata":{"opensandbox.io/owner":"value"}`)},
		{name: "template conflicts with image", body: imageRequest(`,"templateId":"template-v1"`), wantError: "competing workload sources"},
		{name: "template conflicts with snapshot", body: `{"templateId":"template-v1","snapshotId":"snap-1"}`, wantError: "competing workload sources"},
		{name: "pool cannot hide competing workload sources", body: imageRequest(`,"snapshotId":"snap-1","extensions":{"poolRef":"pool-a"}`), wantError: "competing workload sources"},
		{name: "invalid metadata key", body: imageRequest(`,"metadata":{"bad key!":"v"}`), wantError: "unqualified metadata"},
		{name: "reserved metadata", body: imageRequest(`,"metadata":{"agents.kruise.io/owner":"evil"}`), wantError: "Forbidden metadata"},
		{name: "unknown field", body: imageRequest(`,"bogusField":1`)},
		{name: "second JSON value", body: imageRequest("") + ` {}`},
		{name: "null suffix", body: imageRequest("") + ` null`},
		{name: "malformed suffix", body: imageRequest("") + ` garbage`},
		{name: "malformed body", body: `{"image":`, wantError: "invalid request body"},
		{name: "null credential proxy", body: imageRequest(`,"credentialProxy":null`)},
		{name: "empty credential proxy", body: imageRequest(`,"credentialProxy":{}`)},
		{name: "disabled credential proxy", body: imageRequest(`,"credentialProxy":{"enabled":false}`)},
		{name: "proxy enabled with policy parses independently of execution", body: imageRequest(`,"credentialProxy":{"enabled":true},"networkPolicy":{}`), wantProxy: true},
		{name: "proxy without policy parses independently of execution", body: imageRequest(`,"credentialProxy":{"enabled":true}`), wantProxy: true},
		{name: "proxy null enabled decodes as false", body: imageRequest(`,"credentialProxy":{"enabled":null}`)},
		{name: "proxy string enabled rejected", body: imageRequest(`,"credentialProxy":{"enabled":"false"}`), wantError: "invalid request body"},
		{name: "proxy numeric enabled rejected", body: imageRequest(`,"credentialProxy":{"enabled":0}`), wantError: "invalid request body"},
		{name: "proxy unknown option", body: imageRequest(`,"credentialProxy":{"bogus":false}`)},
		{name: "proxy is an object", body: imageRequest(`,"credentialProxy":false`), wantError: "invalid request body"},
		{name: "empty extensions", body: imageRequest(`,"extensions":{}`)},
		{name: "null extensions", body: imageRequest(`,"extensions":null`)},
		{name: "extensions is a map", body: imageRequest(`,"extensions":[]`), wantError: "invalid request body"},
		{name: "poolRef is a string", body: `{"extensions":{"poolRef":{"name":"pool-a"}}}`, wantError: "invalid request body"},
		{name: "null poolRef decodes as empty string", body: imageRequest(`,"extensions":{"poolRef":null}`)},
		{name: "numeric extension rejected", body: imageRequest(`,"extensions":{"other":3}`), wantError: "invalid request body"},
		{name: "snapshot without entrypoint", body: `{"snapshotId":"snap-1","resourceLimits":{}}`, wantSource: createSourceSnapshot},
		{name: "snapshot null entrypoint", body: `{"snapshotId":"snap-1","resourceLimits":{},"entrypoint":null}`, wantSource: createSourceSnapshot},
		{name: "snapshot explicit entrypoint", body: `{"snapshotId":"snap-1","resourceLimits":{},"entrypoint":["sh","-c","echo ok"]}`, wantSource: createSourceSnapshot},
		{name: "snapshot empty entrypoint accepted", body: `{"snapshotId":"snap-1","resourceLimits":{},"entrypoint":[]}`, wantSource: createSourceSnapshot},
		{name: "snapshot needs no limits", body: `{"snapshotId":"snap-1"}`, wantSource: createSourceSnapshot},
		{name: "image conflicts with snapshot", body: imageRequest(`,"snapshotId":"snap-1"`), wantError: "competing workload sources"},
		{name: "blank snapshot is ineffective", body: imageRequest(`,"snapshotId":"  "`)},
		{name: "null snapshot is ineffective", body: imageRequest(`,"snapshotId":null`)},
		{name: "snapshot is a string", body: imageRequest(`,"snapshotId":42`), wantError: "invalid request body"},
		{name: "snapshot with blank URI", body: `{"snapshotId":"snap-1","resourceLimits":{},"image":{"uri":"  "}}`, wantSource: createSourceSnapshot},
		{name: "empty image does not select a competing source", body: `{"snapshotId":"snap-1","resourceLimits":{},"image":{}}`, wantSource: createSourceSnapshot},
		{name: "nested null URI decodes as empty", body: `{"extensions":{"poolRef":"pool-a"},"image":{"uri":null}}`, wantSource: createSourcePool},
		{name: "pool needs no image limits or entrypoint", body: `{"extensions":{"poolRef":"pool-a"}}`, wantSource: createSourcePool},
		{name: "automatic pool recognized", body: `{"extensions":{"poolRef":"*"}}`, wantSource: createSourcePool},
		{name: "pool selects inventory with supplied image", body: imageRequest(`,"extensions":{"poolRef":"pool-a"}`), wantSource: createSourcePool},
		{name: "blank poolRef is ineffective", body: imageRequest(`,"extensions":{"poolRef":"  "}`)},
		{name: "blank poolRef needs another source", body: `{"extensions":{"poolRef":" "}}`, wantError: "one effective"},
		{name: "pool rejects snapshot", body: `{"extensions":{"poolRef":"pool-a"},"snapshotId":"snap-1"}`, wantError: "snapshotId cannot"},
		{name: "pool normalizes blank snapshot", body: `{"extensions":{"poolRef":"pool-a"},"snapshotId":"  "}`, wantSource: createSourcePool},
		{name: "pool allows nonnull lifecycle", body: `{"extensions":{"poolRef":"pool-a"},"lifecycle":{}}`, wantSource: createSourcePool},
		{name: "pool allows null lifecycle", body: `{"extensions":{"poolRef":"pool-a"},"lifecycle":null}`, wantSource: createSourcePool},
		{name: "standalone template is unsupported", body: `{"templateId":"template-v1","timeout":60}`, wantError: "templateId creation is not supported by this create delivery"},
		{name: "template plus named pool is unsupported", body: `{"templateId":"template-v1","timeout":60,"extensions":{"poolRef":"pool-a"}}`, wantError: "templateId with poolRef is not supported by this create delivery"},
		{name: "template plus automatic pool is unsupported", body: `{"templateId":"template-v1","timeout":60,"extensions":{"poolRef":"*"},"networkPolicy":{}}`, wantError: "templateId with poolRef is not supported by this create delivery"},
		{name: "template empty ID is ineffective", body: imageRequest(`,"templateId":""`)},
		{name: "template whitespace is ineffective", body: imageRequest(`,"templateId":"  "`)},
		{name: "template null ID is ineffective", body: imageRequest(`,"templateId":null`)},
		{name: "template ID is a string", body: imageRequest(`,"templateId":123`), wantError: "invalid request body"},
		{name: "template with null overrides is unsupported", body: `{"templateId":"template-v1","timeout":60,"image":null,"snapshotId":"  ","entrypoint":null,"env":null,"resourceLimits":null,"resourceRequests":null,"volumes":null,"platform":null,"credentialProxy":null,"lifecycle":null,"secureAccess":false}`, wantError: "templateId creation is not supported by this create delivery"},
		{name: "null requests", body: imageRequest(`,"resourceRequests":null`)},
		{name: "empty requests remains present", body: imageRequest(`,"resourceRequests":{}`), wantRequests: ptr.To(ResourceLimits{})},
		{name: "partial requests retained without merging", body: `{"image":{"uri":"python"},"entrypoint":["sh"],"resourceLimits":{"cpu":"1","memory":"512Mi"},"resourceRequests":{"cpu":"100m"}}`, wantRequests: ptr.To(ResourceLimits{"cpu": "100m"})},
		{name: "extended resource requests retained", body: imageRequest(`,"resourceRequests":{"example.com/device":"1"}`), wantRequests: ptr.To(ResourceLimits{"example.com/device": "1"})},
		{name: "requests value must be string", body: imageRequest(`,"resourceRequests":{"cpu":1}`), wantError: "invalid request body"},
		{name: "requests null value decodes as empty string", body: imageRequest(`,"resourceRequests":{"cpu":null}`), wantRequests: ptr.To(ResourceLimits{"cpu": ""})},
		{name: "limits null value decodes as empty string", body: `{"image":{"uri":"python"},"entrypoint":["sh"],"resourceLimits":{"cpu":null}}`},
		{name: "requests is a map", body: imageRequest(`,"resourceRequests":[]`), wantError: "invalid request body"},
		{name: "entrypoint null argument decodes as empty string", body: `{"extensions":{"poolRef":"pool-a"},"entrypoint":["sh",null]}`, wantSource: createSourcePool},
		{name: "OpenSandbox env name has no extra restriction", body: imageRequest(`,"env":{"OPENSANDBOX_LIFECYCLE":"x"}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			maximum := tt.maximum
			if maximum == 0 {
				maximum = models.DefaultMaxTimeout
			}
			got, apiErr := parseCreateSandboxRequest(newCreateRequest(t, tt.body), maximum)
			if tt.wantError != "" {
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusBadRequest, apiErr.Code)
				assert.Contains(t, apiErr.Message, tt.wantError)
				return
			}
			require.Nil(t, apiErr)
			wantTimeout := tt.wantTimeout
			if wantTimeout == nil {
				wantTimeout = ptr.To[int64](models.DefaultTimeoutSeconds)
			}
			assert.Equal(t, wantTimeout, got.Timeout)
			wantSource := tt.wantSource
			if wantSource == "" {
				wantSource = createSourceImage
			}
			assert.Equal(t, wantSource, got.source)
			assert.Equal(t, tt.wantProxy, got.CredentialProxy != nil && got.CredentialProxy.Enabled)
			assert.Equal(t, tt.wantRequests, got.ResourceRequests)
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

func TestApplyCreateModifier(t *testing.T) {
	for _, tt := range []struct {
		name     string
		extra    string
		lifetime int64
	}{
		{name: "omitted uses native lifetime", lifetime: models.DefaultTimeoutSeconds},
		{name: "null uses native lifetime", extra: `,"timeout":null`, lifetime: models.DefaultTimeoutSeconds},
		{name: "zero uses native lifetime", extra: `,"timeout":0`, lifetime: models.DefaultTimeoutSeconds},
		{name: "explicit lifetime", extra: `,"timeout":600`, lifetime: 600},
		{name: "stored extension wins over metadata", extra: `,"extensions":{"opensandbox.extensions.note":"extension-value"}`, lifetime: models.DefaultTimeoutSeconds},
	} {
		t.Run(tt.name, func(t *testing.T) {
			request, apiErr := parseCreateSandboxRequest(newCreateRequest(t, imageRequest(`,"metadata":{"user-key":"user-value"}`+tt.extra)), 3600)
			require.Nil(t, apiErr)
			request.Metadata["opensandbox.io/extensions.note"] = "metadata-value"
			metadataBefore := maps.Clone(request.Metadata)
			oldDeadline := metav1.NewTime(time.Now().Add(time.Hour))
			sbx := &sandboxcr.Sandbox{Sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "sbx-1", Annotations: map[string]string{"system": "keep"}},
				Spec:       agentsv1alpha1.SandboxSpec{PauseTime: &oldDeadline, ShutdownTime: &oldDeadline},
			}}
			before := time.Now()
			applyCreateModifier(sbx, request)
			assert.Nil(t, sbx.Spec.PauseTime, "clear inherited auto-pause deadline")
			require.NotNil(t, sbx.Spec.ShutdownTime)
			assert.WithinDuration(t, before.Add(time.Duration(tt.lifetime)*time.Second), sbx.Spec.ShutdownTime.Time, time.Second)
			assert.Equal(t, "keep", sbx.Annotations["system"])
			assert.Equal(t, "user-value", sbx.Annotations["user-key"])
			assert.Equal(t, metadataBefore, request.Metadata)
			wantValue := "metadata-value"
			if len(request.Extensions) != 0 {
				wantValue = "extension-value"
			}
			assert.Equal(t, wantValue, sbx.Annotations["opensandbox.io/extensions.note"])
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
		name     string
		ctx      context.Context
		body     string
		wantCode int
	}{
		{
			name:     "unimplemented source returns before manager call",
			ctx:      withUser(context.Background()),
			body:     `{"extensions":{"poolRef":"*"}}`,
			wantCode: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// manager is nil: every case above must return before touching it.
			s := &Server{manager: nil, maxTimeout: 3600}
			r := newCreateRequest(t, tt.body).WithContext(tt.ctx)

			resp, apiErr := s.CreateSandbox(r)
			require.NotNil(t, apiErr, "expected an error response")
			assert.Equal(t, tt.wantCode, apiErr.Code)
			assert.Equal(t, CreateSandboxResponse{}, resp.Body)
		})
	}
}

func TestCreateHTTPNetworkPublication(t *testing.T) {
	for _, source := range []string{"image", "pool", "snapshot"} {
		for _, tt := range []struct {
			name                  string
			policy                *NetworkPolicy
			wantPolicy, wantProxy bool
		}{
			{name: "no policy"},
			{name: "empty policy", policy: &NetworkPolicy{}},
			{name: "global deny", policy: &NetworkPolicy{DefaultAction: "deny"}, wantProxy: true},
			{name: "request rules", policy: &NetworkPolicy{Egress: []json.RawMessage{json.RawMessage(`{"action":"allow","target":"192.0.2.0/24"}`)}}, wantPolicy: true, wantProxy: true},
		} {
			t.Run(source+"/"+tt.name, func(t *testing.T) {
				provider, client, err := cachetest.NewTestCache(t)
				require.NoError(t, err)
				user := &models.CreatedTeamAPIKey{ID: uuid.New(), Key: "key", Team: &models.Team{Name: "team-a"}}
				box := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name: "delivery", Namespace: user.Team.Name, UID: "delivery-uid",
						Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: user.ID.String()},
					},
					Spec: agentsv1alpha1.SandboxSpec{EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sandbox", Image: "python:3.11"}}}},
					}},
				}
				probe := &createIdentityInfra{result: sandboxcr.AsSandbox(box, provider)}
				manager, err := sandboxmanager.NewSandboxManagerBuilder(config.SandboxManagerOptions{}).
					WithCustomInfra(func() (infra.Builder, error) { return probe, nil }).Build()
				require.NoError(t, err)
				mux := http.NewServeMux()
				require.NoError(t, RegisterRoutes(Deps{Mux: mux, Manager: manager, MaxTimeout: 3600, Keys: &fakeKeyStorage{byKey: map[string]*models.CreatedTeamAPIKey{user.Key: user}}}))
				body := map[string]any{"networkPolicy": tt.policy}
				switch source {
				case "image":
					body["image"] = Image{URI: "python:3.11"}
				case "pool":
					body["extensions"] = Extensions{"poolRef": "pool"}
				case "snapshot":
					body["snapshotId"] = "snapshot"
				}
				raw, err := json.Marshal(body)
				require.NoError(t, err)
				request := newCreateRequest(t, string(raw))
				request.Header.Set(HeaderOpenSandboxAPIKey, user.Key)
				response := httptest.NewRecorder()
				mux.ServeHTTP(response, request)
				require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
				var policies agentsv1alpha1.TrafficPolicyList
				require.NoError(t, client.List(t.Context(), &policies))
				if tt.wantPolicy {
					require.Len(t, policies.Items, 1)
					policy := policies.Items[0]
					assert.Equal(t, box.UID, policy.OwnerReferences[0].UID)
					assert.Equal(t, map[string]string{agentsv1alpha1.LabelSandboxName: box.Name}, policy.Spec.Selector.MatchLabels)
					require.Len(t, policy.Spec.Egress.Rules, 1)
					assert.Equal(t, agentsv1alpha1.RuleActionAllow, policy.Spec.Egress.Rules[0].Action)
					require.Len(t, policy.Spec.Egress.Rules[0].To, 1)
					assert.Equal(t, "192.0.2.0/24", policy.Spec.Egress.Rules[0].To[0].CIDR)
				} else {
					assert.Empty(t, policies.Items)
				}
				if source == "snapshot" {
					assert.Nil(t, probe.claim)
					require.NotNil(t, probe.clone)
				} else {
					require.NotNil(t, probe.claim)
				}
				if source == "image" {
					wantRuntimes := []agentsv1alpha1.RuntimeConfig{{Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime}}
					if tt.wantProxy {
						wantRuntimes = append(wantRuntimes, agentsv1alpha1.RuntimeConfig{Name: agentsv1alpha1.RuntimeConfigForInjectTrafficProxy})
					}
					assert.Equal(t, wantRuntimes, probe.claim.RuntimeConfig)
				}
				assert.Equal(t, allowInternetAccess(tt.policy), box.Labels[agentsv1alpha1.LabelAllowInternetAccess])
			})
		}
	}
}

func TestCreateHTTPContract(t *testing.T) {
	probe := &createIdentityInfra{}
	manager, err := sandboxmanager.NewSandboxManagerBuilder(config.SandboxManagerOptions{}).
		WithCustomInfra(func() (infra.Builder, error) { return probe, nil }).Build()
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, RegisterRoutes(Deps{Mux: mux, Manager: manager, MaxTimeout: 3600}))
	w := httptest.NewRecorder()
	r := newCreateRequest(t, imageRequest(`,"timeout":29`))
	mux.ServeHTTP(w, r)
	require.Equal(t, 400, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Len(t, body, 2)
	assert.Equal(t, "INVALID_REQUEST", body["code"])
	assert.NotEmpty(t, w.Header().Get("X-Request-ID"))

	for _, tt := range []struct {
		name        string
		body        string
		wantCode    int
		wantMessage string
	}{
		{"registry credentials must not be ignored", `{"image":{"uri":"private/image:v1","auth":{"username":"user","password":"test-only"}}}`, 400, "image.auth execution"},
		{"minimal image reaches cold allocation", `{"image":{"uri":"python:3.11"}}`, 400, allocationStopped},
		{"unknown fields use native decoding", imageRequest(`,"unknown":{"future":true},"platform":{}`), 400, allocationStopped},
		{"zero timeout reaches cold allocation", imageRequest(`,"timeout":0`), 400, allocationStopped},
		{"native minimum reaches cold allocation", imageRequest(`,"timeout":30`), 400, allocationStopped},
		{"null options reach cold allocation", imageRequest(`,"credentialProxy":{"enabled":null,"future":true}`), 400, allocationStopped},
		{"template creation remains unsupported with security settings", `{"templateId":"template-v1","env":{},"entrypoint":[],"secureAccess":true}`, 400, "templateId creation is not supported by this create delivery"},
		{"pool hooks reach execution gate", `{"extensions":{"poolRef":"pool-a"},"lifecycle":{}}`, 400, allocationStopped},
		{"disabled proxy reaches cold allocation", imageRequest(`,"credentialProxy":{"enabled":false}`), 400, allocationStopped},
		{"empty proxy reaches cold allocation", imageRequest(`,"credentialProxy":{}`), 400, allocationStopped},
		{"null proxy reaches cold allocation", imageRequest(`,"credentialProxy":null`), 400, allocationStopped},
		{"empty extensions reach cold allocation", imageRequest(`,"extensions":{}`), 400, allocationStopped},
		{"null extensions reach cold allocation", imageRequest(`,"extensions":null`), 400, allocationStopped},
		{"blank pool selector reaches cold allocation", imageRequest(`,"extensions":{"poolRef":"  "}`), 400, allocationStopped},
		{"null pool selector reaches cold allocation", imageRequest(`,"extensions":{"poolRef":null}`), 400, allocationStopped},
		{"null requests reach cold allocation", imageRequest(`,"resourceRequests":null`), 400, allocationStopped},
		{"snapshot network reaches clone allocation", `{"snapshotId":"snap-1","networkPolicy":{}}`, 400, allocationStopped},
		{"snapshot volume requires a supported source", `{"snapshotId":"snap-1","volumes":[{"name":"data"}]}`, 400, "existing PVC source"},
		{"snapshot PVC reaches clone allocation", `{"snapshotId":"snap-1","volumes":[{"name":"data","mountPath":"/mnt/data","pvc":{"claimName":"shared"}}]}`, 400, allocationStopped},
		{"snapshot empty volumes reach clone allocation", `{"snapshotId":"snap-1","volumes":[]}`, 400, allocationStopped},
		{"snapshot reaches clone allocation", `{"snapshotId":"snap-1","resourceLimits":{}}`, 400, allocationStopped},
		{"named pool cannot fall through to image cold creation", imageRequest(`,"extensions":{"poolRef":"pool-a"}`), 400, allocationStopped},
		{"pool without image cannot dereference image", `{"extensions":{"poolRef":"pool-a"}}`, 400, allocationStopped},
		{"automatic pool execution remains gated", `{"extensions":{"poolRef":"*"}}`, 400, "automatic pool selection"},
		{"standalone template remains unsupported", `{"templateId":"template-v1","timeout":60}`, 400, "templateId creation is not supported by this create delivery"},
		{"template plus pool remains unsupported", `{"templateId":"template-v1","timeout":60,"extensions":{"poolRef":"pool-a"}}`, 400, "templateId with poolRef is not supported by this create delivery"},
		{"requests must not be ignored", imageRequest(`,"resourceRequests":{"cpu":"100m"}`), 400, allocationStopped},
		{"empty requests need no resource override", imageRequest(`,"resourceRequests":{}`), 400, allocationStopped},
		{"extended limits must not be ignored", `{"image":{"uri":"python:3.11"},"entrypoint":["sh"],"resourceLimits":{"gpu":"1"}}`, 400, "extended resourceLimits execution"},
		{"image GPU requests must not be ignored", imageRequest(`,"resourceRequests":{"gpu":"1"}`), 400, "extended resourceRequests execution"},
		{"image vendor resource requests are rejected", imageRequest(`,"resourceRequests":{"nvidia.com/gpu":"1"}`), 400, "extended resourceRequests execution"},
		{"snapshot CPU and memory requests are native clone updates", `{"snapshotId":"snap-1","resourceRequests":{"cpu":"100m","memory":"128Mi"}}`, 400, "InplaceUpdate is not supported for clone"},
		{"snapshot empty requests reach clone allocation", `{"snapshotId":"snap-1","resourceRequests":{}}`, 400, allocationStopped},
		{"snapshot null requests reach clone allocation", `{"snapshotId":"snap-1","resourceRequests":null}`, 400, allocationStopped},
		{"snapshot vendor resource requests are rejected", `{"snapshotId":"snap-1","resourceRequests":{"nvidia.com/gpu":"1"}}`, 400, "extended resourceRequests execution"},
		{"snapshot other resource requests are rejected", `{"snapshotId":"snap-1","resourceRequests":{"ephemeral-storage":"1Gi"}}`, 400, "extended resourceRequests execution"},
		{"resource limits error precedes requests error", `{"image":{"uri":"python:3.11"},"resourceLimits":{"gpu":"1"},"resourceRequests":{"nvidia.com/gpu":"1"}}`, 400, "extended resourceLimits execution"},
		{"enabled proxy must not be ignored", imageRequest(`,"credentialProxy":{"enabled":true},"networkPolicy":{}`), 400, "credentialProxy.enabled=true execution"},
		{"other extension must not be ignored", imageRequest(`,"extensions":{"other":"value"}`), 400, "non-empty extensions execution"},
		{"stored extension reaches allocation", imageRequest(`,"extensions":{"opensandbox.extensions.note":"value"}`), 400, allocationStopped},
		{"invalid stored extension fails before allocation", imageRequest(`,"extensions":{"opensandbox.extensions.bad/key":"value"}`), 400, "invalid stored extension annotation key"},
		{"existing PVC reaches cold allocation", imageRequest(`,"volumes":[{"name":"data","pvc":{"claimName":"shared","createIfNotExists":false},"mountPath":"/mnt/data"}]`), 400, allocationStopped},
		{"empty volumes need no mount operation", imageRequest(`,"volumes":[]`), 400, allocationStopped},
		{"volume requires a supported source", imageRequest(`,"volumes":[{"name":"data"}]`), 400, "existing PVC source"},
		{"empty network policy reaches cold allocation", imageRequest(`,"networkPolicy":{}`), 400, allocationStopped},
		{"startup hook must not be ignored", imageRequest(`,"lifecycle":{"preStart":{"command":["true"]}}`), 400, "lifecycle execution"},
		{"secure endpoint must not be ignored", imageRequest(`,"secureAccess":true`), 400, "secureAccess execution"},
		{"source conflict precedes execution gap", imageRequest(`,"snapshotId":"snap-1"`), 400, "competing workload sources"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			probe.lookup, probe.checkpointLookup = nil, nil
			probe.claim, probe.clone = nil, nil
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, newCreateRequest(t, tt.body))
			require.Equal(t, tt.wantCode, response.Code)
			var result ErrorResponse
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
			assert.Contains(t, result.Message, tt.wantMessage)
			if probe.clone != nil && strings.Contains(tt.body, "claimName") {
				assert.Equal(t, []infra.ExistingVolumeMount{{Name: "data", VolumeName: "shared", MountPath: "/mnt/data"}}, probe.clone.Volumes)
			}
			if strings.HasPrefix(tt.wantMessage, "extended resource") || strings.Contains(tt.wantMessage, "deferred") {
				assert.Nil(t, probe.lookup, "unsupported inputs must fail before source lookup")
				assert.Nil(t, probe.checkpointLookup)
				assert.Nil(t, probe.claim, "unsupported inputs must fail before allocation")
				assert.Nil(t, probe.clone)
			}
			if tt.wantCode == http.StatusBadRequest {
				assert.Equal(t, "INVALID_REQUEST", result.Code)
			}
		})
	}

	// Snapshot lookup errors come from clone, without a separate boolean lookup.
	for _, tt := range []struct {
		name    string
		err     error
		status  int
		code    string
		message string
	}{
		{"missing checkpoint", managererrors.NewError(managererrors.ErrorNotFound, "checkpoint missing"), 400, "INVALID_REQUEST", "checkpoint missing"},
		{"lookup failure", managererrors.NewError(managererrors.ErrorInternal, "checkpoint lookup failed"), 500, "INTERNAL_ERROR", "checkpoint lookup failed"},
		{"canceled clone", context.Canceled, 500, "INTERNAL_ERROR", context.Canceled.Error()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			probe.lookup, probe.checkpointLookup = nil, nil
			probe.claim, probe.clone = nil, nil
			probe.cloneErr = tt.err
			defer func() { probe.cloneErr = nil }()
			response := httptest.NewRecorder()
			request := newCreateRequest(t, `{"snapshotId":"snapshot-1"}`)
			if tt.err == context.Canceled {
				ctx, cancel := context.WithCancel(request.Context())
				cancel()
				request = request.WithContext(ctx)
			}
			mux.ServeHTTP(response, request)
			require.Equal(t, tt.status, response.Code)
			var result ErrorResponse
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
			assert.Equal(t, tt.code, result.Code)
			assert.Contains(t, result.Message, tt.message)
			assert.Nil(t, probe.lookup)
			assert.Nil(t, probe.checkpointLookup)
			assert.Nil(t, probe.claim)
			require.NotNil(t, probe.clone)
			assert.Equal(t, "snapshot-1", probe.clone.CheckPointID)
			assert.False(t, probe.clone.SkipWaitCheckpoint)
		})
	}

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

// Exercise the real HTTP and Manager boundary so defaults are checked before
// backend conversion, including the whole-map resource requests behavior.
func TestCreateHTTPColdConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name, extra  string
		wantRequests map[string]string
	}{
		{"absent requests use limits", "", map[string]string{"cpu": "750m", "memory": "384Mi"}},
		{"empty requests use limits", `,"resourceRequests":{}`, map[string]string{"cpu": "750m", "memory": "384Mi"}},
		{"null requests use limits", `,"resourceRequests":null`, map[string]string{"cpu": "750m", "memory": "384Mi"}},
		{"nonempty requests replace whole map", `,"resourceRequests":{"cpu":"150m"}`, map[string]string{"cpu": "150m"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			probe := &createIdentityInfra{}
			manager, err := sandboxmanager.NewSandboxManagerBuilder(config.SandboxManagerOptions{}).
				WithCustomInfra(func() (infra.Builder, error) { return probe, nil }).Build()
			require.NoError(t, err)
			mux := http.NewServeMux()
			require.NoError(t, RegisterRoutes(Deps{Mux: mux, Manager: manager, MaxTimeout: 3600}))
			body := `{"image":{"uri":"unaliased/image:v1"},"entrypoint":["python3","-c","print('hello world')","two words"],"env":{"STARTUP":"value"},"resourceLimits":{"cpu":"750m","memory":"384Mi"},"platform":{"os":"linux","arch":"arm64"}` + tt.extra + `}`
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, newCreateRequest(t, body))
			require.Equal(t, http.StatusBadRequest, w.Code)
			require.Contains(t, w.Body.String(), allocationStopped)
			require.NotNil(t, probe.claim)
			require.NotNil(t, probe.claim.ColdStart)
			assert.Equal(t, &infra.ColdStartOptions{
				Image: "unaliased/image:v1", Command: []string{"python3", "-c", "print('hello world')", "two words"},
				EnvVars: map[string]string{"STARTUP": "value"}, ResourceLimits: map[string]string{"cpu": "750m", "memory": "384Mi"},
				ResourceRequests: tt.wantRequests, OS: "linux", Architecture: "arm64",
			}, probe.claim.ColdStart)
			assert.Equal(t, probe.claim.ColdStart.EnvVars, probe.claim.InitRuntime.EnvVars)
			assert.Equal(t, []agentsv1alpha1.RuntimeConfig{{Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime}}, probe.claim.RuntimeConfig)
			assert.Empty(t, probe.claim.Template)
			assert.Nil(t, probe.lookup)
		})
	}
}

func TestCreateHTTPPoolConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name, extra              string
		wantImage                string
		wantRequests, wantLimits map[string]string
		wantError                string
	}{
		{name: "pool defaults retain configuration"},
		{name: "pool network policy", extra: `,"networkPolicy":{}`},
		{name: "pool PVC mount", extra: `,"volumes":[{"name":"data","mountPath":"/mnt/data","pvc":{"claimName":"shared"}}]`},
		{name: "empty resource maps retain configuration", extra: `,"resourceLimits":{},"resourceRequests":{}`},
		{name: "blank image retains configuration", extra: `,"image":{"uri":" "}`},
		{name: "image override", extra: `,"image":{"uri":"custom/image:v2"}`, wantImage: "custom/image:v2"},
		{name: "limits leave requests unchanged", extra: `,"resourceLimits":{"cpu":"750m","memory":"384Mi"}`, wantLimits: map[string]string{"cpu": "750m", "memory": "384Mi"}},
		{name: "empty requests leave source requests unchanged", extra: `,"resourceLimits":{"cpu":"750m"},"resourceRequests":{}`, wantLimits: map[string]string{"cpu": "750m"}},
		{name: "null requests leave source requests unchanged", extra: `,"resourceLimits":{"cpu":"750m"},"resourceRequests":null`, wantLimits: map[string]string{"cpu": "750m"}},
		{name: "image and resources with whole map requests", extra: `,"image":{"uri":"custom/image:v2"},"resourceLimits":{"cpu":"750m","memory":"384Mi"},"resourceRequests":{"cpu":"150m"}`, wantImage: "custom/image:v2", wantRequests: map[string]string{"cpu": "150m"}, wantLimits: map[string]string{"cpu": "750m", "memory": "384Mi"}},
		{name: "requests only", extra: `,"resourceRequests":{"cpu":"150m"}`, wantRequests: map[string]string{"cpu": "150m"}},
		{name: "invalid request quantity", extra: `,"resourceRequests":{"cpu":"invalid"}`, wantError: "invalid resourceRequests quantity for cpu"},
		{name: "invalid limit quantity", extra: `,"resourceRequests":{"cpu":"150m"},"resourceLimits":{"cpu":"invalid"}`, wantError: "invalid resourceLimits quantity for cpu"},
		{name: "extended requests rejected", extra: `,"resourceRequests":{"nvidia.com/gpu":"1"}`, wantError: "extended resourceRequests execution is not implemented"},
		{name: "extended limits rejected", extra: `,"resourceLimits":{"gpu":"1"}`, wantError: "extended resourceLimits execution is not implemented"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			probe := &createIdentityInfra{}
			manager, err := sandboxmanager.NewSandboxManagerBuilder(config.SandboxManagerOptions{}).
				WithCustomInfra(func() (infra.Builder, error) { return probe, nil }).Build()
			require.NoError(t, err)
			mux := http.NewServeMux()
			require.NoError(t, RegisterRoutes(Deps{Mux: mux, Manager: manager, MaxTimeout: 3600}))
			body := `{"extensions":{"poolRef":" named-pool "},"env":{"STARTUP":"value"}` + tt.extra + `}`
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, newCreateRequest(t, body))
			require.Equal(t, http.StatusBadRequest, w.Code)
			if tt.wantError != "" {
				require.Contains(t, w.Body.String(), tt.wantError)
				assert.Nil(t, probe.claim, "invalid overrides must fail before allocation")
				assert.Nil(t, probe.lookup)
				return
			}
			require.Contains(t, w.Body.String(), allocationStopped)
			require.NotNil(t, probe.claim)
			assert.Nil(t, probe.claim.ColdStart)
			assert.Equal(t, "named-pool", probe.claim.Template)
			if strings.Contains(tt.extra, "claimName") {
				assert.Equal(t, []infra.ExistingVolumeMount{{Name: "data", VolumeName: "shared", MountPath: "/mnt/data"}}, probe.claim.Volumes)
			}
			assert.True(t, probe.claim.CreateOnNoStock, "named pools use the native E2B no-stock creation default")
			assert.Equal(t, map[string]string{"STARTUP": "value"}, probe.claim.InitRuntime.EnvVars)
			if tt.wantImage == "" && len(tt.wantRequests) == 0 && len(tt.wantLimits) == 0 {
				assert.Nil(t, probe.claim.InplaceUpdate)
				return
			}
			require.NotNil(t, probe.claim.InplaceUpdate)
			assert.Equal(t, tt.wantImage, probe.claim.InplaceUpdate.Image)
			if len(tt.wantRequests) == 0 && len(tt.wantLimits) == 0 {
				assert.Nil(t, probe.claim.InplaceUpdate.Resources)
				return
			}
			require.NotNil(t, probe.claim.InplaceUpdate.Resources)
			quantities := func(values map[string]string) corev1.ResourceList {
				if values == nil {
					return nil
				}
				result := corev1.ResourceList{}
				for name, value := range values {
					result[corev1.ResourceName(name)] = resource.MustParse(value)
				}
				return result
			}
			assert.Equal(t, quantities(tt.wantRequests), probe.claim.InplaceUpdate.Resources.Requests)
			assert.Equal(t, quantities(tt.wantLimits), probe.claim.InplaceUpdate.Resources.Limits)
		})
	}
}

func TestResolveExistingVolumes(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		wantError  bool
	}{
		{name: "existing PVC", body: `{"name":"data","pvc":{"claimName":"shared","createIfNotExists":false},"mountPath":"/mnt/data","subPath":"poc","readOnly":true}`},
		{name: "missing source", body: `{"name":"data"}`, wantError: true},
		{name: "host source", body: `{"name":"data","host":{"path":"/tmp"}}`, wantError: true},
		{name: "competing sources", body: `{"pvc":{"claimName":"shared"},"host":{"path":"/tmp"}}`, wantError: true},
		{name: "invalid shape", body: `{"pvc":"wrong"}`, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, apiErr := resolveExistingVolumes([]json.RawMessage{json.RawMessage(tt.body)})
			if tt.wantError {
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusBadRequest, apiErr.Code)
				return
			}
			require.Nil(t, apiErr)
			assert.Equal(t, []infra.ExistingVolumeMount{{Name: "data", VolumeName: "shared", MountPath: "/mnt/data", SubPath: "poc", ReadOnly: true}}, got)
		})
	}
}

// TestCreateHTTPNativeMetadata exercises the real HTTP adapter with the existing
// allocation probe, including source-specific native extension restrictions.
func TestCreateHTTPNativeMetadata(t *testing.T) {
	for _, source := range []string{"pool", "snapshot", "image"} {
		for _, tt := range []struct {
			name      string
			metadata  map[string]string
			fields    map[string]any
			wantError string
		}{
			{name: "label and ordinary metadata", metadata: map[string]string{"label:app": "demo", "note": "value"}},
			{name: "default deny uses native labels", fields: map[string]any{"networkPolicy": NetworkPolicy{DefaultAction: "deny"}}},
			{name: "native operation deadlines", metadata: map[string]string{models.ExtensionKeyClaimTimeout: "12", models.ExtensionKeyWaitReadyTimeout: "7"}},
			{name: "native inventory preference", metadata: map[string]string{models.ExtensionKeyCreateOnNoStock: "false"}},
			{name: "skip runtime initialization", metadata: map[string]string{models.ExtensionKeySkipInitRuntime: "true"}},
			{name: "never timeout", metadata: map[string]string{models.ExtensionKeyNeverTimeout: "true"}},
			{name: "failed sandbox retention", metadata: map[string]string{models.ExtensionKeyReserveFailedSandboxFor: "10m", models.ExtensionKeyReserveFailedSandbox: "true"}},
			{name: "paused retention", metadata: map[string]string{models.ExtensionKeyReservePausedSandboxDuration: "10m"}},
			{name: "return pod IP", metadata: map[string]string{models.ExtensionKeyReturnPodIP: "true"}},
			{name: "native resource precedence", metadata: map[string]string{models.ExtensionKeyClaimWithCPURequest: "200m", models.ExtensionKeyClaimWithMemoryLimit: "512Mi"}, fields: map[string]any{"resourceRequests": map[string]string{"cpu": "100m"}, "resourceLimits": map[string]string{"memory": "256Mi"}}},
			{name: "native image precedence", metadata: map[string]string{models.ExtensionKeyClaimWithImage: "native:v2"}},
			{name: "native single CSI", metadata: map[string]string{models.ExtensionKeyClaimWithCSIMount_VolumeName: "pv", models.ExtensionKeyClaimWithCSIMount_MountPoint: "/data", models.ExtensionKeyClaimWithCSIMount_SubPath: "child"}},
			{name: "native multi CSI", metadata: map[string]string{models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"pv","mountPath":"/data","readOnly":true}]`}},
			{name: "ambiguous CSI inputs", metadata: map[string]string{models.ExtensionKeyClaimWithCSIMount_VolumeName: "pv", models.ExtensionKeyClaimWithCSIMount_MountPoint: "/data"}, fields: map[string]any{"volumes": []map[string]any{{"name": "data", "mountPath": "/data", "pvc": map[string]string{"claimName": "claim"}}}}, wantError: "volumes and native CSI metadata cannot be combined"},
			{name: "clone naming", metadata: map[string]string{models.ExtensionKeySandboxGenerateName: "custom-"}},
			{name: "inline security rule", metadata: map[string]string{models.ExtensionKeySecurityRules: `[{"name":"block-example","match":[{"domains":["example.com"]}],"actions":{"block":{}}}]`}},
			{name: "inline bypass rejected", metadata: map[string]string{models.ExtensionKeySecurityRules: `[{"name":"unsafe","match":[{"domains":["example.com"]}],"actions":{"bypass":true}}]`}, wantError: "bypass is not allowed"},
			{name: "system annotation rejected", metadata: map[string]string{agentsv1alpha1.AnnotationOwner: "forged"}, wantError: "Forbidden metadata"},
			{name: "reserved label rejected", metadata: map[string]string{"label:" + agentsv1alpha1.LabelSandboxName: "forged"}, wantError: "reserved"},
			{name: "invalid label value", metadata: map[string]string{"label:app": "has spaces"}, wantError: "invalid label value"},
			{name: "invalid native duration", metadata: map[string]string{models.ExtensionKeyReserveFailedSandboxFor: "bad"}, wantError: "invalid reserve failed sandbox duration"},
		} {
			t.Run(source+"/"+tt.name, func(t *testing.T) {
				clone := source == "snapshot"
				user := &models.CreatedTeamAPIKey{ID: uuid.New(), Team: &models.Team{Name: "team-a"}, Key: "key"}
				box := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "delivery", Namespace: user.Team.Name, Annotations: map[string]string{
					agentsv1alpha1.AnnotationOwner: user.ID.String(), agentsv1alpha1.AnnotationRuntimeAccessToken: "inherited-token", agentsv1alpha1.AnnotationSecurityRules: "old-rules",
				}}, Spec: agentsv1alpha1.SandboxSpec{EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "old:v1", Command: []string{"original"}, Env: []corev1.EnvVar{{Name: "ORIGINAL", Value: "value"}}}}}}}}, Status: agentsv1alpha1.SandboxStatus{PodInfo: agentsv1alpha1.PodInfo{PodIP: "10.0.0.1"}}}
				sbx := sandboxcr.AsSandbox(box, nil)
				sbx.EnableWakeOnIngressTraffic(time.Minute)
				originalSpec := box.Spec.Template.Spec.DeepCopy()
				probe := &createIdentityInfra{result: sbx}
				manager, err := sandboxmanager.NewSandboxManagerBuilder(config.SandboxManagerOptions{}).WithCustomInfra(func() (infra.Builder, error) { return probe, nil }).Build()
				require.NoError(t, err)
				mux := http.NewServeMux()
				require.NoError(t, RegisterRoutes(Deps{Mux: mux, Manager: manager, MaxTimeout: 3600, Keys: &fakeKeyStorage{byKey: map[string]*models.CreatedTeamAPIKey{user.Key: user}}}))
				body := map[string]any{"metadata": tt.metadata, "env": map[string]string{"REQUEST": "value"}}
				switch source {
				case "pool":
					body["extensions"] = Extensions{"poolRef": "pool"}
				case "snapshot":
					body["snapshotId"] = "snapshot"
					body["entrypoint"] = []string{"ignored"}
					body["platform"] = Platform{Arch: "arm64"}
				case "image":
					body["image"] = Image{URI: "python:3.11"}
				}
				maps.Copy(body, tt.fields)
				raw, err := json.Marshal(body)
				require.NoError(t, err)
				r := newCreateRequest(t, string(raw))
				r.Header.Set(HeaderOpenSandboxAPIKey, user.Key)
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)
				wantError := tt.wantError
				if clone && (tt.name == "native resource precedence" || tt.name == "native image precedence") {
					wantError = "InplaceUpdate is not supported for clone"
				}
				if !clone && tt.name == "clone naming" {
					wantError = "only supported for clone"
				}
				if wantError != "" {
					require.Equal(t, 400, w.Code, w.Body.String())
					assert.Contains(t, w.Body.String(), wantError)
					assert.Nil(t, probe.claim)
					assert.Nil(t, probe.clone)
					return
				}
				require.Equal(t, 202, w.Code, w.Body.String())
				if source == "snapshot" {
					assert.Nil(t, probe.lookup, "an explicit snapshot must not query the same-named SandboxSet")
					assert.Nil(t, probe.checkpointLookup, "snapshot must use clone lookup directly")
				}
				assert.NotContains(t, box.Spec.Template.Labels, agentsv1alpha1.LabelSandboxName, "Pod identity is assigned by the controller")
				assert.NotContains(t, box.Spec.Template.Labels, agentsv1alpha1.LabelSandboxUID, "Pod identity is assigned by the controller")
				assert.Equal(t, *originalSpec, box.Spec.Template.Spec, "the modifier must not rewrite the workload")
				assert.Equal(t, "inherited-token", box.Annotations[agentsv1alpha1.AnnotationRuntimeAccessToken])
				assert.True(t, box.Spec.AutoPausePolicy == nil || box.Spec.AutoPausePolicy.Resume == nil || box.Spec.AutoPausePolicy.Resume.OnIngressTraffic == nil)
				internet := "true"
				if tt.name == "default deny uses native labels" {
					internet = "false"
				}
				assert.Equal(t, internet, box.Labels[agentsv1alpha1.LabelAllowInternetAccess])
				assert.Equal(t, internet, box.Spec.Template.Labels[agentsv1alpha1.LabelAllowInternetAccess])
				if tt.name == "inline security rule" {
					assert.Contains(t, box.Annotations[agentsv1alpha1.AnnotationSecurityRules], `"statusCode":403`)
				} else {
					assert.NotContains(t, box.Annotations, agentsv1alpha1.AnnotationSecurityRules)
				}
				if tt.name == "never timeout" {
					assert.Nil(t, box.Spec.ShutdownTime)
				} else {
					assert.NotNil(t, box.Spec.ShutdownTime)
				}
				retention := "forever"
				if tt.name == "paused retention" {
					retention = "10m"
				}
				assert.Equal(t, retention, box.Annotations[agentsv1alpha1.AnnotationReservePausedSandboxDuration])
				var response CreateSandboxResponse
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				for key := range tt.metadata {
					if strings.HasPrefix(key, agentsv1alpha1.E2BPrefix) {
						assert.NotContains(t, response.Metadata, key)
						if key != models.ExtensionKeyReturnPodIP {
							assert.NotContains(t, box.Annotations, key)
						}
					}
				}
				if tt.name == "label and ordinary metadata" {
					assert.Equal(t, "demo", box.Labels["app"])
					assert.Equal(t, "demo", box.Spec.Template.Labels["app"])
					assert.Equal(t, "demo", response.Metadata["app"])
					assert.Equal(t, "value", response.Metadata["note"])
				}
				if tt.name == "return pod IP" {
					assert.Equal(t, "10.0.0.1", response.Metadata[models.MetadataKeyPodIP])
				}
				var csi *config.CSIMountOptions
				var reserve *time.Duration
				if clone {
					require.NotNil(t, probe.clone)
					assert.False(t, probe.clone.SkipWaitCheckpoint)
					csi, reserve = probe.clone.CSIMount, probe.clone.ReserveFailedSandboxFor
					if tt.name == "clone naming" {
						assert.Equal(t, "custom-", probe.clone.GenerateName)
					}
					if tt.name == "native operation deadlines" {
						assert.Equal(t, 12*time.Second, probe.clone.CloneTimeout)
						assert.Equal(t, 7*time.Second, probe.clone.WaitReadyTimeout)
					}
				} else {
					require.NotNil(t, probe.claim)
					csi, reserve = probe.claim.CSIMount, probe.claim.ReserveFailedSandboxFor
					if tt.name == "native inventory preference" {
						assert.False(t, probe.claim.CreateOnNoStock)
					}
					if tt.name == "skip runtime initialization" {
						assert.Nil(t, probe.claim.InitRuntime)
					} else {
						assert.Equal(t, map[string]string{"REQUEST": "value"}, probe.claim.InitRuntime.EnvVars)
					}
					if tt.name == "native operation deadlines" {
						assert.Equal(t, 12*time.Second, probe.claim.ClaimTimeout)
						assert.Equal(t, 7*time.Second, probe.claim.WaitReadyTimeout)
					}
					if tt.name == "label and ordinary metadata" {
						assert.Equal(t, []string{"app"}, probe.claim.UserMetadataKeys.Labels)
						assert.Equal(t, []string{"note"}, probe.claim.UserMetadataKeys.Annotations)
					}
					if tt.name == "native resource precedence" {
						if source == "image" {
							assert.Equal(t, "200m", probe.claim.ColdStart.ResourceRequests["cpu"])
							assert.Equal(t, "512Mi", probe.claim.ColdStart.ResourceLimits["memory"])
						} else {
							q := probe.claim.InplaceUpdate.Resources.Requests[corev1.ResourceCPU]
							assert.Zero(t, q.Cmp(resource.MustParse("200m")))
							q = probe.claim.InplaceUpdate.Resources.Limits[corev1.ResourceMemory]
							assert.Zero(t, q.Cmp(resource.MustParse("512Mi")))
						}
					}
					if tt.name == "native image precedence" {
						if source == "image" {
							assert.Equal(t, "native:v2", probe.claim.ColdStart.Image)
						} else {
							assert.Equal(t, "native:v2", probe.claim.InplaceUpdate.Image)
						}
					}
				}
				if tt.name == "failed sandbox retention" {
					require.NotNil(t, reserve)
					assert.Equal(t, 10*time.Minute, *reserve)
				}
				if strings.HasPrefix(tt.name, "native single CSI") || strings.HasPrefix(tt.name, "native multi CSI") {
					require.NotNil(t, csi)
					assert.Empty(t, csi.MountOptionList)
					assert.Contains(t, csi.MountOptionListRaw, `"pvName":"pv"`)
				}
			})
		}
	}
}
