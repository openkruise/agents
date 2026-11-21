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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/utils"
	runtimeconfig "github.com/openkruise/agents/pkg/utils/runtime/config"
)

func TestCreateSandboxWithClaim_AccessToken(t *testing.T) {
	for _, skipInit := range []bool{false, true} {
		t.Run("skip-init="+strconv.FormatBool(skipInit), func(t *testing.T) {
			// Given a pool whose runtime records every initialization request.
			initRequests := make(chan runtimeconfig.InitRuntimeOptions, 2)
			runtimeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/init" {
					http.Error(w, "unexpected runtime request", http.StatusBadRequest)
					return
				}
				var opts runtimeconfig.InitRuntimeOptions
				if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				initRequests <- opts
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(runtimeServer.Close)
			controller, client, teardown := Setup(t)
			t.Cleanup(teardown)
			cleanup := CreateSandboxPool(t, controller, "token-template", 2, CreateSandboxPoolOptions{RuntimeURL: runtimeServer.URL})
			t.Cleanup(cleanup)
			var previousToken string

			for range 2 {
				// When a sandbox is delivered through the create API.
				resp, apiErr := controller.CreateSandbox(NewRequest(t, nil, map[string]any{
					"templateID": "token-template",
					"metadata": map[string]string{
						models.ExtensionKeySkipInitRuntime: strconv.FormatBool(skipInit),
					},
				}, nil, adminTestUser()))
				require.Nil(t, apiErr)
				require.Equal(t, http.StatusCreated, resp.Code)

				// Then the response and gateway route carry the same unique UUID.
				token := resp.Body.EnvdAccessToken
				_, err := uuid.Parse(token)
				require.NoError(t, err)
				require.NotEqual(t, previousToken, token)
				previousToken = token
				sbx := GetSandbox(t, resp.Body.SandboxID, client)
				require.Equal(t, token, utils.GetAccessToken(sbx))
				route, err := sandboxroute.RouteFromSandbox(sbx)
				require.NoError(t, err)
				require.Equal(t, token, route.AccessToken)
				require.Equal(t, agentsv1alpha1.SandboxStateRunning, route.State)
				if skipInit {
					require.NotContains(t, sbx.Annotations, agentsv1alpha1.AnnotationInitRuntimeRequest)
					require.Empty(t, initRequests)
				} else {
					require.NotEmpty(t, initRequests)
					require.Equal(t, token, (<-initRequests).AccessToken)
				}
			}
		})
	}
}
