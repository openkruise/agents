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
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/servers/opensandbox/models"
)

// createTestSandbox creates one image-based sandbox through the HTTP API and
// returns its ID, reused by every pause/resume/metadata test below.
func createTestSandbox(t *testing.T, controller *Controller) string {
	t.Helper()
	body, err := json.Marshal(models.CreateSandboxRequest{
		Image:      &models.ImageSpec{URI: "python:3.11"},
		Extensions: map[string]string{"skipInitRuntime": "true"},
	})
	require.NoError(t, err)
	resp := doRequest(t, controller.mux, http.MethodPost, "/v1/sandboxes", nil, body)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	var created models.Sandbox
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	require.NotEmpty(t, created.ID)
	return created.ID
}

// enableWaitSim and updateSandboxWhen stand in for the agent-sandbox-controller,
// which normally drives a Sandbox's Paused/Ready conditions in response to
// Spec.Paused. Pause/Resume block on the cache's wait-reconcile mechanism for
// that confirmation, so a fake-client-only test must simulate it — the same
// pattern pkg/servers/e2b's own pause/resume tests use (EnableWaitSim /
// UpdateSandboxWhen / DoSetSandboxStatus in its core_test.go), reimplemented
// here because those are test-only helpers unexported from another package.
func enableWaitSim(t *testing.T, controller *Controller, sandboxID string) {
	t.Helper()
	mockMgr := controller.cache.(*infracache.Cache).GetMockManager()
	namespace, name, ok := strings.Cut(sandboxID, "--")
	require.True(t, ok, "expected legacy namespace--name sandboxID, got %q", sandboxID)
	sbx := &agentsv1alpha1.Sandbox{}
	require.NoError(t, mockMgr.GetClient().Get(t.Context(), ctrlclient.ObjectKey{Namespace: namespace, Name: name}, sbx))
	mockMgr.AddWaitReconcileKey(sbx)
}

// simulateControllerPauseOrResume watches for spec.Paused to reach want, then
// writes the status a real sandbox controller would after actuating it.
func simulateControllerPauseOrResume(t *testing.T, fc ctrlclient.Client, sandboxID string, want bool) {
	t.Helper()
	namespace, name, ok := strings.Cut(sandboxID, "--")
	require.True(t, ok, "expected legacy namespace--name sandboxID, got %q", sandboxID)
	require.Eventually(t, func() bool {
		sbx := &agentsv1alpha1.Sandbox{}
		if err := fc.Get(t.Context(), ctrlclient.ObjectKey{Namespace: namespace, Name: name}, sbx); err != nil {
			return false
		}
		return sbx.Spec.Paused == want
	}, 5*time.Second, 10*time.Millisecond)

	sbx := &agentsv1alpha1.Sandbox{}
	require.NoError(t, fc.Get(t.Context(), ctrlclient.ObjectKey{Namespace: namespace, Name: name}, sbx))
	pausedStatus, readyStatus := metav1.ConditionFalse, metav1.ConditionTrue
	phase := agentsv1alpha1.SandboxRunning
	if want {
		pausedStatus, readyStatus = metav1.ConditionTrue, metav1.ConditionFalse
		phase = agentsv1alpha1.SandboxPaused
	}
	sbx.Status.Phase = phase
	sbx.Status.Conditions = []metav1.Condition{
		{Type: string(agentsv1alpha1.SandboxConditionPaused), Status: pausedStatus},
		{Type: string(agentsv1alpha1.SandboxConditionReady), Status: readyStatus},
	}
	require.NoError(t, fc.Status().Update(t.Context(), sbx))
}

func TestPauseAndResumeSandbox(t *testing.T) {
	controller, fc := Setup(t)
	createDefaultTemplateFixture(t, fc, testNamespace)
	simulateInstantReadySandbox(t)
	sandboxID := createTestSandbox(t, controller)
	enableWaitSim(t, controller, sandboxID)

	go simulateControllerPauseOrResume(t, fc, sandboxID, true)
	pauseResp := doRequest(t, controller.mux, http.MethodPost, "/v1/sandboxes/"+sandboxID+"/pause", nil, nil)
	defer func() { _ = pauseResp.Body.Close() }()
	require.Equal(t, http.StatusAccepted, pauseResp.StatusCode)

	getResp := doRequest(t, controller.mux, http.MethodGet, "/v1/sandboxes/"+sandboxID, nil, nil)
	var afterPause models.Sandbox
	require.NoError(t, json.NewDecoder(getResp.Body).Decode(&afterPause))
	_ = getResp.Body.Close()
	assert.Equal(t, models.SandboxStatePaused, afterPause.Status.State)

	go simulateControllerPauseOrResume(t, fc, sandboxID, false)
	resumeResp := doRequest(t, controller.mux, http.MethodPost, "/v1/sandboxes/"+sandboxID+"/resume", nil, nil)
	defer func() { _ = resumeResp.Body.Close() }()
	require.Equal(t, http.StatusAccepted, resumeResp.StatusCode)

	getResp2 := doRequest(t, controller.mux, http.MethodGet, "/v1/sandboxes/"+sandboxID, nil, nil)
	defer func() { _ = getResp2.Body.Close() }()
	var afterResume models.Sandbox
	require.NoError(t, json.NewDecoder(getResp2.Body).Decode(&afterResume))
	assert.Equal(t, models.SandboxStateRunning, afterResume.Status.State)
}

func TestPauseSandbox_NotFound(t *testing.T) {
	controller, _ := Setup(t)
	resp := doRequest(t, controller.mux, http.MethodPost, "/v1/sandboxes/does-not-exist/pause", nil, nil)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
