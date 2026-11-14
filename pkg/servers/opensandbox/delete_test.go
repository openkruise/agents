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
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

func TestDeleteSandboxAuthorizationAndCleanup(t *testing.T) {
	user := &models.CreatedTeamAPIKey{ID: uuid.New(), Team: &models.Team{Name: "team-a"}}
	tests := []struct {
		name                            string
		exists, foreign, otherNamespace bool
		phase                           agentsv1alpha1.SandboxPhase
		want                            int
	}{
		{name: "owned running", exists: true, phase: agentsv1alpha1.SandboxRunning, want: 204},
		{name: "owned failed", exists: true, phase: agentsv1alpha1.SandboxFailed, want: 204},
		{name: "owned pending", exists: true, phase: agentsv1alpha1.SandboxPending, want: 204},
		{name: "foreign key", exists: true, foreign: true, phase: agentsv1alpha1.SandboxRunning, want: 404},
		{name: "foreign namespace", exists: true, otherNamespace: true, phase: agentsv1alpha1.SandboxRunning, want: 404},
		{name: "missing", want: 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
				Name: "sandbox", Namespace: "team-a",
				Labels:      map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True, agentsv1alpha1.LabelSandboxID: "sbx-test"},
				Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: user.ID.String()},
			}, Status: agentsv1alpha1.SandboxStatus{Phase: tt.phase}}
			if tt.foreign {
				sbx.Annotations[agentsv1alpha1.AnnotationOwner] = uuid.NewString()
			}
			if tt.otherNamespace {
				sbx.Namespace = "team-b"
			}
			var objs []client.Object
			if tt.exists {
				objs = append(objs, sbx)
			}
			cache, fc, err := cachetest.NewTestCache(t, objs...)
			require.NoError(t, err)
			opts := config.InitOptions(config.SandboxManagerOptions{DisableEnvoyExtProc: true})
			mgr, err := sandboxmanager.NewSandboxManagerBuilder(opts).WithCustomInfra(func() (infra.Builder, error) {
				return sandboxcr.NewInfraBuilder(opts).WithCache(cache).WithRouteReader(proxy.NewServer(opts)), nil
			}).Build()
			require.NoError(t, err)
			mux := http.NewServeMux()
			require.NoError(t, RegisterRoutes(Deps{Mux: mux, Manager: mgr, Keys: &fakeKeyStorage{byKey: map[string]*models.CreatedTeamAPIKey{"test-key": user}}}))
			r := httptest.NewRequest(http.MethodDelete, "/v1/sandboxes/sbx-test", nil)
			r.Header.Set(HeaderOpenSandboxAPIKey, "test-key")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			require.Equal(t, tt.want, w.Code, w.Body.String())
			if tt.want == 204 {
				assert.Empty(t, w.Body.String())
				err := fc.Get(t.Context(), client.ObjectKeyFromObject(sbx), &agentsv1alpha1.Sandbox{})
				assert.True(t, apierrors.IsNotFound(err), "sandbox must actually be removed: %v", err)
				again := httptest.NewRecorder()
				mux.ServeHTTP(again, r)
				assert.Equal(t, 404, again.Code)
				assert.JSONEq(t, `{"code":"NOT_FOUND","message":"sandbox not found"}`, again.Body.String())
			} else {
				assert.JSONEq(t, `{"code":"NOT_FOUND","message":"sandbox not found"}`, w.Body.String())
				if tt.exists {
					require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(sbx), &agentsv1alpha1.Sandbox{}))
				}
			}
		})
	}
}
