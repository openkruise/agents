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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

// The spy exposes native policy results and permits a stale or failed readback.
// Its embedded interface causes unexpected lifecycle calls to fail the test.
type networkPolicySandboxSpy struct {
	infra.Sandbox
	owner        string
	state        string
	labels       map[string]string
	policy       *infra.SandboxNetworkConfig
	readErr      error
	writeErr     error
	staleRead    bool
	beforeWrite  func()
	reads        int
	replacements []infra.SandboxNetworkConfig
}

func (s *networkPolicySandboxSpy) GetAnnotations() map[string]string {
	return map[string]string{agentsv1alpha1.AnnotationOwner: s.owner}
}
func (s *networkPolicySandboxSpy) GetLabels() map[string]string { return s.labels }
func (s *networkPolicySandboxSpy) GetState() (string, string)   { return s.state, "" }
func (s *networkPolicySandboxSpy) SelectNetworkPolicy(ctx context.Context) (*infra.SandboxNetworkConfig, error) {
	s.reads++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.policy, s.readErr
}
func (s *networkPolicySandboxSpy) UpdateNetworkPolicy(_ context.Context, cfg infra.SandboxNetworkConfig) error {
	if s.beforeWrite != nil {
		hook := s.beforeWrite
		s.beforeWrite = nil
		hook()
	}
	s.replacements = append(s.replacements, cfg)
	if s.writeErr == nil && !s.staleRead {
		s.policy = &cfg
	}
	return s.writeErr
}

func newNetworkPolicyHTTPFixture(t *testing.T) (*http.ServeMux, *networkPolicySandboxSpy) {
	t.Helper()
	user := &models.CreatedTeamAPIKey{
		ID: uuid.New(), Team: &models.Team{Name: "network-policy-team"},
	}
	sandbox := &networkPolicySandboxSpy{
		owner: user.ID.String(),
		state: agentsv1alpha1.SandboxStateRunning,
		labels: map[string]string{
			agentsv1alpha1.LabelAllowInternetAccess: agentsv1alpha1.False,
		},
	}
	store := &fakeKeyStorage{byKey: map[string]*models.CreatedTeamAPIKey{"network-policy-key": user}}
	return newControlPlaneTestMux(t, store, &createIdentityInfra{result: sandbox}), sandbox
}

func newNetworkPolicyRequest(method, body string) *http.Request {
	request := httptest.NewRequest(method, "/v1/sandboxes/sbx-test/networkpolicy", strings.NewReader(body))
	request.Header.Set(HeaderOpenSandboxAPIKey, "network-policy-key")
	return request
}

func requestNetworkPolicy(mux *http.ServeMux, method, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, newNetworkPolicyRequest(method, body))
	return response
}

func assertNetworkPolicy(t *testing.T, response *httptest.ResponseRecorder, mode, action, rules string) {
	t.Helper()
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.JSONEq(t, fmt.Sprintf(`{"status":"ok","mode":%q,"policy":{"defaultAction":%q,"egress":%s}}`, mode, action, rules),
		response.Body.String())
}

func TestRuntimeNetworkPolicyRoundTrip(t *testing.T) {
	mux, spy := newNetworkPolicyHTTPFixture(t)
	assertNetworkPolicy(t, requestNetworkPolicy(mux, http.MethodGet, ""), "deny_all", "deny", "[]")

	const untouched = `[{"action":"allow","target":"keep.example.com"}]`
	tests := []struct {
		name, method, body, egress string
		writes                     int
	}{
		{
			name: "put preserves an explicit matching default", method: http.MethodPut, writes: 1,
			body:   `{"defaultAction":"deny","egress":[{"action":"deny","target":"192.0.2.1/32"},{"action":"allow","target":"keep.example.com"},{"action":"allow","target":"192.0.2.1"}]}`,
			egress: `[{"action":"allow","target":"keep.example.com"},{"action":"allow","target":"192.0.2.1/32"},{"action":"deny","target":"192.0.2.1/32"}]`,
		},
		{
			name: "patch changes both old actions and keeps the first match", method: http.MethodPatch, writes: 1,
			body:   `[{"action":" DENY ","target":"192.0.2.1"},{"action":"allow","target":"192.0.2.1/32"},{"action":"allow","target":"2001:db8::1"}]`,
			egress: `[{"action":"allow","target":"keep.example.com"},{"action":"allow","target":"2001:db8::1/128"},{"action":"deny","target":"192.0.2.1/32"}]`,
		},
		{
			name: "delete normalizes targets", method: http.MethodDelete, writes: 1,
			body: `["192.0.2.1","2001:db8::1/128"]`, egress: untouched,
		},
		{
			name: "unknown targets are a no-op", method: http.MethodDelete,
			body: `["different.example.com","*.example.com"]`, egress: untouched,
		},
		{name: "same action patch is a no-op", method: http.MethodPatch, body: untouched, egress: untouched},
		{
			name: "null patch is a no-op", method: http.MethodPatch,
			body: `null`, egress: untouched,
		},
		{
			name: "empty delete is a no-op", method: http.MethodDelete,
			body: `[]`, egress: untouched,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(spy.replacements)
			assertNetworkPolicy(t, requestNetworkPolicy(mux, tt.method, tt.body), "enforcing", "deny", tt.egress)
			assert.Len(t, spy.replacements, before+tt.writes)
		})
	}

	assertNetworkPolicy(t, requestNetworkPolicy(mux, http.MethodPut, "null"), "deny_all", "deny", "[]")
	assert.Equal(t, agentsv1alpha1.False, spy.labels[agentsv1alpha1.LabelAllowInternetAccess])

	spy.labels[agentsv1alpha1.LabelAllowInternetAccess] = agentsv1alpha1.True
	assertNetworkPolicy(t, requestNetworkPolicy(mux, http.MethodGet, ""), "allow_all", "allow", "[]")
}

func TestRuntimeNetworkPolicyErrors(t *testing.T) {
	tests := []struct {
		name, method, body string
		status             int
		setup              func(*networkPolicySandboxSpy)
	}{
		{
			name: "dead sandbox hidden", method: http.MethodGet, status: http.StatusNotFound,
			setup: func(s *networkPolicySandboxSpy) { s.state = agentsv1alpha1.SandboxStateDead },
		},
		{
			name: "reserved failed sandbox hidden", method: http.MethodGet, status: http.StatusNotFound,
			setup: func(s *networkPolicySandboxSpy) {
				s.labels[agentsv1alpha1.LabelSandboxReservedFailed] = agentsv1alpha1.True
			},
		},
		{
			name: "missing label", method: http.MethodGet, status: http.StatusNotImplemented,
			setup: func(s *networkPolicySandboxSpy) { delete(s.labels, agentsv1alpha1.LabelAllowInternetAccess) },
		},
		{
			name: "invalid label before body", method: http.MethodPut, status: http.StatusNotImplemented,
			setup: func(s *networkPolicySandboxSpy) { s.labels[agentsv1alpha1.LabelAllowInternetAccess] = "deny" },
		},
		{name: "cannot change default", method: http.MethodPut, body: `{"defaultAction":"allow"}`, status: http.StatusBadRequest},
		{name: "empty body", method: http.MethodPut, status: http.StatusBadRequest},
		{name: "wrong put type", method: http.MethodPut, body: `[]`, status: http.StatusBadRequest},
		{name: "malformed patch", method: http.MethodPatch, body: `[`, status: http.StatusBadRequest},
		{name: "trailing value", method: http.MethodPatch, body: `[] null`, status: http.StatusBadRequest},
		{
			name: "invalid shadowed rule", method: http.MethodPatch, status: http.StatusBadRequest,
			body: `[{"action":"allow","target":"192.0.2.1"},{"action":"invalid","target":"192.0.2.1/32"}]`,
		},
		{name: "null delete target", method: http.MethodDelete, body: `[null]`, status: http.StatusBadRequest},
	}
	codes := map[int]string{
		http.StatusNotFound:       "NOT_FOUND",
		http.StatusNotImplemented: "NOT_IMPLEMENTED",
		http.StatusBadRequest:     "INVALID_REQUEST",
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, spy := newNetworkPolicyHTTPFixture(t)
			if tt.setup != nil {
				tt.setup(spy)
			}
			response := requestNetworkPolicy(mux, tt.method, tt.body)
			require.Equal(t, tt.status, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), fmt.Sprintf(`"code":%q`, codes[tt.status]))
			assert.Zero(t, spy.reads)
			assert.Empty(t, spy.replacements)
		})
	}
}

func TestRuntimeNetworkPolicyNativeReadback(t *testing.T) {
	tests := []struct {
		name              string
		readErr, writeErr error
		staleRead, cancel bool
		status            int
	}{
		{name: "stale readback supplies the response", staleRead: true, status: http.StatusOK},
		{name: "failed readback retains write", readErr: errors.New("watch not ready"), status: http.StatusInternalServerError},
		{name: "canceled readback retains write", cancel: true, status: http.StatusInternalServerError},
		{
			name: "typed write failure skips readback", status: http.StatusConflict,
			writeErr: managererrors.NewError(managererrors.ErrorConflict, "sandbox changed"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, spy := newNetworkPolicyHTTPFixture(t)
			spy.readErr, spy.writeErr, spy.staleRead = tt.readErr, tt.writeErr, tt.staleRead
			spy.state = agentsv1alpha1.SandboxStatePaused
			spy.policy = &infra.SandboxNetworkConfig{AllowOut: []string{"old.example.com"}}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.cancel {
				spy.beforeWrite = cancel
			}
			response := httptest.NewRecorder()
			request := newNetworkPolicyRequest(http.MethodPut, `{"egress":[{"action":"allow","target":"new.example.com"}]}`)
			mux.ServeHTTP(response, request.WithContext(ctx))
			require.Equal(t, tt.status, response.Code, response.Body.String())
			require.Len(t, spy.replacements, 1)
			if tt.writeErr != nil {
				assert.Contains(t, response.Body.String(), `"code":"CONFLICT"`)
				assert.Zero(t, spy.reads)
				return
			}
			assert.Equal(t, 1, spy.reads)
			if tt.staleRead {
				assertNetworkPolicy(t, response, "enforcing", "deny", `[{"action":"allow","target":"old.example.com"}]`)
				return
			}
			assert.Contains(t, response.Body.String(), `"code":"INTERNAL_ERROR"`)
			wantErr := tt.readErr
			if tt.cancel {
				wantErr = context.Canceled
			}
			assert.Contains(t, response.Body.String(), wantErr.Error())
			spy.readErr = nil
			assertNetworkPolicy(t, requestNetworkPolicy(mux, http.MethodGet, ""), "enforcing", "deny",
				`[{"action":"allow","target":"new.example.com"}]`)
			assert.Len(t, spy.replacements, 1)
		})
	}
}

func TestRuntimeNetworkPolicyRuleLimit(t *testing.T) {
	mux, spy := newNetworkPolicyHTTPFixture(t)

	rules := make([]networkRule, 0, 100)
	for i := range 100 {
		rules = append(rules, networkRule{Action: "allow", Target: fmt.Sprintf("host%d.example.com", i)})
	}
	body, err := json.Marshal(runtimeNetworkPolicy{Egress: rules})
	require.NoError(t, err)
	response := requestNetworkPolicy(mux, http.MethodPut, string(body))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	response = requestNetworkPolicy(mux, http.MethodPatch, `[{"action":"allow","target":"extra.example.com"}]`)
	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "maximum of 100")
	assert.Len(t, spy.replacements, 1)
}

func TestRuntimeNetworkPolicyOverlappingPatches(t *testing.T) {
	mux, spy := newNetworkPolicyHTTPFixture(t)
	spy.policy = &infra.SandboxNetworkConfig{AllowOut: []string{"original.example.com"}}
	spy.beforeWrite = func() {
		response := requestNetworkPolicy(mux, http.MethodPatch, `[{"action":"allow","target":"intervening.example.com"}]`)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	}

	response := requestNetworkPolicy(mux, http.MethodPatch, `[{"action":"allow","target":"last.example.com"}]`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Len(t, spy.replacements, 2)
	assert.Equal(t, []string{"original.example.com", "intervening.example.com"}, spy.replacements[0].AllowOut)
	assert.Equal(t, []string{"original.example.com", "last.example.com"}, spy.replacements[1].AllowOut)
	assert.Equal(t, &spy.replacements[1], spy.policy)
}
