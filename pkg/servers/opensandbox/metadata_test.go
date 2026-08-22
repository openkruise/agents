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

	"github.com/openkruise/agents/pkg/servers/opensandbox/models"
)

func TestPatchSandboxMetadata(t *testing.T) {
	controller, fc := Setup(t)
	createDefaultTemplateFixture(t, fc, testNamespace)
	simulateInstantReadySandbox(t)

	body, err := json.Marshal(models.CreateSandboxRequest{
		Image:      &models.ImageSpec{URI: "python:3.11"},
		Metadata:   map[string]string{"a": "1", "b": "2"},
		Extensions: map[string]string{"skipInitRuntime": "true"},
	})
	require.NoError(t, err)
	createResp := doRequest(t, controller.mux, http.MethodPost, "/v1/sandboxes", nil, body)
	var created models.Sandbox
	require.NoError(t, json.NewDecoder(createResp.Body).Decode(&created))
	_ = createResp.Body.Close()

	// JSON Merge Patch: overwrite "a", delete "b" (null), leave others untouched.
	patchBody := []byte(`{"a":"one","b":null,"c":"three"}`)
	patchResp := doRequest(t, controller.mux, http.MethodPatch, "/v1/sandboxes/"+created.ID+"/metadata", nil, patchBody)
	defer func() { _ = patchResp.Body.Close() }()
	require.Equal(t, http.StatusOK, patchResp.StatusCode)

	var patched models.Sandbox
	require.NoError(t, json.NewDecoder(patchResp.Body).Decode(&patched))
	assert.Equal(t, "one", patched.Metadata["a"])
	assert.Equal(t, "three", patched.Metadata["c"])
	_, hasB := patched.Metadata["b"]
	assert.False(t, hasB)

	// The patch must be persisted, not just reflected in the response.
	getResp := doRequest(t, controller.mux, http.MethodGet, "/v1/sandboxes/"+created.ID, nil, nil)
	defer func() { _ = getResp.Body.Close() }()
	var fetched models.Sandbox
	require.NoError(t, json.NewDecoder(getResp.Body).Decode(&fetched))
	assert.Equal(t, "one", fetched.Metadata["a"])
	assert.Equal(t, "three", fetched.Metadata["c"])
}

func TestPatchSandboxMetadata_ReservedPrefixRejected(t *testing.T) {
	controller, fc := Setup(t)
	createDefaultTemplateFixture(t, fc, testNamespace)
	simulateInstantReadySandbox(t)
	sandboxID := createTestSandbox(t, controller)

	patchBody := []byte(`{"opensandbox.io/x":"y"}`)
	resp := doRequest(t, controller.mux, http.MethodPatch, "/v1/sandboxes/"+sandboxID+"/metadata", nil, patchBody)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
