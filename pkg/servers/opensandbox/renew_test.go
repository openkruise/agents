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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
)

func newRenewRequest(t *testing.T, body string, sbx *sandboxcr.Sandbox) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, RoutePrefix+"/sandboxes/sbx-1/renew-expiration", strings.NewReader(body))
	ctx := r.Context()
	if sbx != nil {
		ctx = context.WithValue(ctx, sandboxContextKey, sbx)
	}
	return r.WithContext(ctx)
}

func TestParseRenewExpirationRequest(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
		want    string
	}{
		{name: "valid body", body: `{"expiresAt":"2030-01-01T00:00:00Z"}`, want: "2030-01-01T00:00:00Z"},
		{name: "missing expiresAt", body: `{}`, wantErr: true},
		{name: "empty expiresAt", body: `{"expiresAt":""}`, wantErr: true},
		{name: "unknown field rejected", body: `{"expiresAt":"2030-01-01T00:00:00Z","bogus":1}`, wantErr: true},
		{name: "malformed json rejected", body: `{"expiresAt":`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/renew-expiration", strings.NewReader(tt.body))
			got, apiErr := parseRenewExpirationRequest(r)
			if tt.wantErr {
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusBadRequest, apiErr.Code)
				return
			}
			require.Nil(t, apiErr)
			assert.Equal(t, tt.want, got.ExpiresAt)
		})
	}
}

// TestRenewSandboxExpiration_EarlyReturns covers every renew branch that returns
// before the timeout write, so it exercises the real handler with a nil manager
// and a bare sandbox object. The success path (SaveTimeoutWithPolicy) needs a
// live infra client and is covered by the kind-environment acceptance run.
func TestRenewSandboxExpiration_EarlyReturns(t *testing.T) {
	// The helper sandbox carries ShutdownTime = now+30m, so its current
	// expiresAt is 30 minutes out; renew requests are measured against that.
	newSbx := func(t *testing.T) *sandboxcr.Sandbox {
		return runningClaimedSandbox(t, "team-a", "sbx-1", "sbx-id-1", nil)
	}

	tests := []struct {
		name       string
		sbx        *sandboxcr.Sandbox
		maxTimeout int
		body       string
		wantCode   int
	}{
		{
			name:       "missing sandbox in context is internal error",
			sbx:        nil,
			maxTimeout: 3600,
			body:       `{"expiresAt":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`,
			wantCode:   http.StatusInternalServerError,
		},
		{
			name:       "malformed body is rejected",
			sbx:        newSbx(t),
			maxTimeout: 3600,
			body:       `{}`,
			wantCode:   http.StatusBadRequest,
		},
		{
			name:       "non RFC3339 expiresAt is rejected",
			sbx:        newSbx(t),
			maxTimeout: 3600,
			body:       `{"expiresAt":"not-a-time"}`,
			wantCode:   http.StatusBadRequest,
		},
		{
			name:       "past expiresAt is rejected",
			sbx:        newSbx(t),
			maxTimeout: 3600,
			body:       `{"expiresAt":"` + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) + `"}`,
			wantCode:   http.StatusBadRequest,
		},
		{
			name:       "non advancing expiresAt is rejected",
			sbx:        newSbx(t),
			maxTimeout: 3600,
			// current expiresAt is now+30m; request now+10m does not advance it.
			body:     `{"expiresAt":"` + time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339) + `"}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name: "expiresAt beyond max timeout is rejected",
			// A sandbox whose current expiration is near (now+10s) so the request
			// (now+120s) advances past it and the only failing guard is the
			// max-timeout ceiling (60s).
			sbx: runningClaimedSandbox(t, "team-a", "sbx-1", "sbx-id-1", func(s *agentsv1alpha1.Sandbox) {
				short := time.Now().UTC().Add(10 * time.Second).Truncate(time.Second)
				s.Spec.ShutdownTime = &metav1.Time{Time: short}
			}),
			maxTimeout: 60,
			body:       `{"expiresAt":"` + time.Now().Add(120*time.Second).UTC().Format(time.RFC3339) + `"}`,
			wantCode:   http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{manager: nil, maxTimeout: tt.maxTimeout}
			r := newRenewRequest(t, tt.body, tt.sbx)

			resp, apiErr := s.RenewSandboxExpiration(r)
			require.NotNil(t, apiErr)
			assert.Equal(t, tt.wantCode, apiErr.Code)
			assert.Equal(t, RenewSandboxExpirationResponse{}, resp.Body)
		})
	}
}
