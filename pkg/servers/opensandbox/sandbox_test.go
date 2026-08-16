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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/opensandbox/models"
)

// TestCreateSandbox_Image is the "demonstrate the work on pod/containers"
// test: it drives a real POST /v1/sandboxes request through the full stack
// (opensandbox handler -> SandboxManager.ClaimSandbox -> Infra -> a real
// Sandbox custom resource carrying a Kubernetes PodTemplateSpec), then reads
// back the persisted object directly through the fake API server to prove the
// caller's image actually landed in the pod's container spec — not just in
// the HTTP response.
func TestCreateSandbox_Image(t *testing.T) {
	controller, fc := Setup(t)
	createDefaultTemplateFixture(t, fc, testNamespace)
	simulateInstantReadySandbox(t)

	body, err := json.Marshal(models.CreateSandboxRequest{
		Image: &models.ImageSpec{URI: "python:3.11"},
		ResourceLimits: models.ResourceLimits{
			"cpu":    "250m",
			"memory": "256Mi",
		},
		Metadata: map[string]string{"team": "agents"},
		// This unit test runs against a fake Kubernetes client with no real
		// agent-runtime to answer the /init handshake, so it opts out via this
		// adapter's skipInitRuntime vendor extension (see sandbox.go).
		Extensions: map[string]string{"skipInitRuntime": "true"},
	})
	require.NoError(t, err)

	resp := doRequest(t, controller.mux, http.MethodPost, "/v1/sandboxes", nil, body)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	var created models.Sandbox
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	assert.Equal(t, models.SandboxStateRunning, created.Status.State)
	assert.Equal(t, "python:3.11", created.Image.URI)
	assert.Equal(t, "agents", created.Metadata["team"])
	require.NotEmpty(t, created.ID)

	// Read the underlying Sandbox CR directly to prove the image requested
	// through the OpenSandbox API actually landed on the pod template the
	// sandbox controller would schedule as a real Pod — the concrete
	// "demonstrated on a pod" artifact for this adapter.
	var sandboxList agentsv1alpha1.SandboxList
	require.NoError(t, fc.List(t.Context(), &sandboxList, client.InNamespace(testNamespace)))
	require.Len(t, sandboxList.Items, 1)
	pod := sandboxList.Items[0].Spec.Template
	require.NotNil(t, pod)
	require.Len(t, pod.Spec.Containers, 1)
	assert.Equal(t, "python:3.11", pod.Spec.Containers[0].Image)
}

func TestCreateSandbox_Validation(t *testing.T) {
	controller, _ := Setup(t)

	tests := []struct {
		name string
		body models.CreateSandboxRequest
	}{
		{
			name: "neither image nor snapshotId",
			body: models.CreateSandboxRequest{},
		},
		{
			name: "both image and snapshotId",
			body: models.CreateSandboxRequest{
				Image:      &models.ImageSpec{URI: "python:3.11"},
				SnapshotID: "snap-1",
			},
		},
		{
			name: "windows platform is rejected",
			body: models.CreateSandboxRequest{
				Image:    &models.ImageSpec{URI: "python:3.11"},
				Platform: &models.PlatformSpec{OS: "windows", Arch: "amd64"},
			},
		},
		{
			name: "timeout below the spec floor is rejected",
			body: models.CreateSandboxRequest{
				Image:   &models.ImageSpec{URI: "python:3.11"},
				Timeout: intPtr(10),
			},
		},
		{
			name: "reserved metadata prefix is rejected",
			body: models.CreateSandboxRequest{
				Image:    &models.ImageSpec{URI: "python:3.11"},
				Metadata: map[string]string{"opensandbox.io/x": "y"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := json.Marshal(tt.body)
			require.NoError(t, err)
			resp := doRequest(t, controller.mux, http.MethodPost, "/v1/sandboxes", nil, body)
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

func TestGetAndDeleteSandbox(t *testing.T) {
	controller, fc := Setup(t)
	createDefaultTemplateFixture(t, fc, testNamespace)
	simulateInstantReadySandbox(t)

	body, err := json.Marshal(models.CreateSandboxRequest{Image: &models.ImageSpec{URI: "python:3.11"}, Extensions: map[string]string{"skipInitRuntime": "true"}})
	require.NoError(t, err)
	createResp := doRequest(t, controller.mux, http.MethodPost, "/v1/sandboxes", nil, body)
	var created models.Sandbox
	require.NoError(t, json.NewDecoder(createResp.Body).Decode(&created))
	_ = createResp.Body.Close()

	getResp := doRequest(t, controller.mux, http.MethodGet, "/v1/sandboxes/"+created.ID, nil, nil)
	defer func() { _ = getResp.Body.Close() }()
	require.Equal(t, http.StatusOK, getResp.StatusCode)
	var fetched models.Sandbox
	require.NoError(t, json.NewDecoder(getResp.Body).Decode(&fetched))
	assert.Equal(t, created.ID, fetched.ID)

	delResp := doRequest(t, controller.mux, http.MethodDelete, "/v1/sandboxes/"+created.ID, nil, nil)
	defer func() { _ = delResp.Body.Close() }()
	assert.Equal(t, http.StatusNoContent, delResp.StatusCode)

	notFoundResp := doRequest(t, controller.mux, http.MethodGet, "/v1/sandboxes/"+created.ID, nil, nil)
	defer func() { _ = notFoundResp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, notFoundResp.StatusCode)
}

func TestListSandboxes_Pagination(t *testing.T) {
	controller, fc := Setup(t)
	createDefaultTemplateFixture(t, fc, testNamespace)
	simulateInstantReadySandbox(t)

	for range 3 {
		body, err := json.Marshal(models.CreateSandboxRequest{Image: &models.ImageSpec{URI: "python:3.11"}, Extensions: map[string]string{"skipInitRuntime": "true"}})
		require.NoError(t, err)
		resp := doRequest(t, controller.mux, http.MethodPost, "/v1/sandboxes", nil, body)
		require.Equal(t, http.StatusAccepted, resp.StatusCode)
		_ = resp.Body.Close()
	}

	resp := doRequest(t, controller.mux, http.MethodGet, "/v1/sandboxes?pageSize=2&page=1", nil, nil)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var page1 models.ListSandboxesResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&page1))
	assert.Len(t, page1.Items, 2)
	assert.Equal(t, 3, page1.Pagination.TotalItems)
	assert.True(t, page1.Pagination.HasNextPage)

	resp2 := doRequest(t, controller.mux, http.MethodGet, "/v1/sandboxes?pageSize=2&page=2", nil, nil)
	defer func() { _ = resp2.Body.Close() }()
	var page2 models.ListSandboxesResponse
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&page2))
	assert.Len(t, page2.Items, 1)
	assert.False(t, page2.Pagination.HasNextPage)
}

func intPtr(v int) *int { return &v }
