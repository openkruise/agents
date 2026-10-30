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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/proxy"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

func TestPauseResumeSandboxAcceptance(t *testing.T) {
	tests := []struct {
		name, operation string
		paused          bool
		phase           agentsv1alpha1.SandboxPhase
		condition       agentsv1alpha1.SandboxConditionType
		conditionTrue   bool
		want            int
	}{
		{"pause running", "pause", false, agentsv1alpha1.SandboxRunning, agentsv1alpha1.SandboxConditionReady, true, 202},
		{"repeat pause", "pause", true, agentsv1alpha1.SandboxRunning, agentsv1alpha1.SandboxConditionReady, true, 202},
		{"resume paused", "resume", true, agentsv1alpha1.SandboxPaused, agentsv1alpha1.SandboxConditionPaused, true, 202},
		{"repeat resume before phase catches up", "resume", false, agentsv1alpha1.SandboxPaused, agentsv1alpha1.SandboxConditionPaused, true, 202},
		{"repeat resume during initialization", "resume", false, agentsv1alpha1.SandboxRunning, agentsv1alpha1.SandboxConditionReady, false, 202},
		{"resume failed sandbox", "resume", false, agentsv1alpha1.SandboxFailed, agentsv1alpha1.SandboxConditionReady, false, 409},
		{"pause during initialization", "pause", false, agentsv1alpha1.SandboxRunning, agentsv1alpha1.SandboxConditionReady, false, 409},
		{"pause conflicts with accepted resume", "pause", false, agentsv1alpha1.SandboxPaused, agentsv1alpha1.SandboxConditionPaused, true, 409},
		{"resume conflicts with accepted pause", "resume", true, agentsv1alpha1.SandboxRunning, agentsv1alpha1.SandboxConditionReady, true, 409},
		{"resume conflicts with incomplete suspension", "resume", true, agentsv1alpha1.SandboxPaused, agentsv1alpha1.SandboxConditionPaused, false, 409},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &models.CreatedTeamAPIKey{ID: uuid.New(), Team: &models.Team{Name: "team-a"}}
			status := metav1.ConditionFalse
			if tt.conditionTrue {
				status = metav1.ConditionTrue
			}
			sbx := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "team-a", UID: "uid-test",
					Labels:      map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True, agentsv1alpha1.LabelSandboxID: "sbx-test"},
					Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: user.ID.String()},
				},
				Spec:   agentsv1alpha1.SandboxSpec{Paused: tt.paused, ShutdownTime: &metav1.Time{Time: time.Now().Add(time.Hour).Truncate(time.Second)}},
				Status: agentsv1alpha1.SandboxStatus{Phase: tt.phase, Conditions: []metav1.Condition{{Type: string(tt.condition), Status: status}}},
			}
			cache, fc, err := cachetest.NewTestCache(t, sbx)
			require.NoError(t, err)
			opts := config.InitOptions(config.SandboxManagerOptions{DisableEnvoyExtProc: true})
			mgr, err := sandboxmanager.NewSandboxManagerBuilder(opts).WithCustomInfra(func() (infra.Builder, error) {
				return sandboxcr.NewInfraBuilder(opts).WithCache(cache).WithAPIReader(fc).WithRouteReader(proxy.NewServer(opts)), nil
			}).Build()
			require.NoError(t, err)
			mux := http.NewServeMux()
			require.NoError(t, RegisterRoutes(Deps{Mux: mux, Manager: mgr, Keys: &fakeKeyStorage{byKey: map[string]*models.CreatedTeamAPIKey{"key": user}}}))
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			r := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/sbx-test/"+tt.operation, nil).WithContext(ctx)
			r.Header.Set(HeaderOpenSandboxAPIKey, "key")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			require.Equal(t, tt.want, w.Code, w.Body.String())
			var actual agentsv1alpha1.Sandbox
			require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(sbx), &actual))
			assert.Equal(t, sbx.Status, actual.Status, "HTTP acceptance must not require controller completion")
			assert.True(t, actual.Spec.ShutdownTime.Equal(sbx.Spec.ShutdownTime), "timeout remains unchanged")
			if tt.want == 202 {
				assert.JSONEq(t, `{}`, w.Body.String())
				assert.Equal(t, tt.operation == "pause", actual.Spec.Paused)
			} else {
				assert.Equal(t, tt.paused, actual.Spec.Paused)
			}
		})
	}
}
