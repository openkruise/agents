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
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

func TestRecoveryE2BLifecycle(t *testing.T) {
	for _, phase := range []v1alpha1.SandboxPhase{v1alpha1.SandboxRecovering, v1alpha1.SandboxFailed, v1alpha1.SandboxSucceeded} {
		t.Run(string(phase), func(t *testing.T) {
			controller, cli, teardown := Setup(t)
			defer teardown()
			user := &models.CreatedTeamAPIKey{ID: keys.AdminKeyID, Key: InitKey, Name: "admin", Team: models.AdminTeam()}
			box := CreateClaimedSandboxCR(t, controller, Namespace, "recovery-lifecycle", "test-template", user.ID.String(), nil)
			id := fmt.Sprintf("%s--%s", box.Namespace, box.Name)
			UpdateSandboxWhen(t, cli, id, Immediately, DoSetSandboxStatus(phase, "", metav1.ConditionFalse))
			req := NewRequest(t, nil, models.SetTimeoutRequest{TimeoutSeconds: 30}, map[string]string{"sandboxID": id}, user)
			require.Eventually(t, func() bool {
				sbx, err := controller.getSandboxOfUser(req.Context(), id, claimedSandboxStates)
				if err != nil {
					return false
				}
				state, _ := sbx.GetState()
				if phase == v1alpha1.SandboxRecovering {
					return state == v1alpha1.SandboxStateRecovering
				}
				return state == v1alpha1.SandboxStateDead
			}, 5*time.Second, 10*time.Millisecond)
			if phase == v1alpha1.SandboxRecovering {
				before := GetSandbox(t, id, cli).DeepCopy()
				_, apiErr := controller.ConnectSandbox(req)
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusServiceUnavailable, apiErr.Code)
				v2Req := NewRequest(t, nil, nil, map[string]string{"sandboxID": id}, user)
				_, apiErr = controller.ConnectSandboxV2(v2Req)
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusServiceUnavailable, apiErr.Code)
				after := GetSandbox(t, id, cli)
				assert.Equal(t, before.Spec, after.Spec, "connect during recovery must not resume or extend TTL")
				describe, apiErr := controller.DescribeSandbox(v2Req)
				require.Nil(t, apiErr)
				assert.Equal(t, models.SandboxStateRunning, describe.Body.State)
				sbx, apiErr := controller.getSandboxOfUser(v2Req.Context(), id, claimedSandboxStates)
				require.Nil(t, apiErr)
				assert.True(t, getListFilter(ListSandboxesRequest{States: []string{v1alpha1.SandboxStateRunning}})(sbx))
				assert.False(t, getListFilter(ListSandboxesRequest{States: []string{v1alpha1.SandboxStatePaused}})(sbx))
			}
			response, apiErr := controller.DeleteSandbox(NewRequest(t, nil, nil, map[string]string{"sandboxID": id}, user))
			require.Nil(t, apiErr)
			assert.Equal(t, http.StatusNoContent, response.Code)
			require.Eventually(t, func() bool {
				return apierrors.IsNotFound(cli.Get(t.Context(), client.ObjectKeyFromObject(box), &v1alpha1.Sandbox{}))
			}, time.Second, 10*time.Millisecond)
		})
	}
}
