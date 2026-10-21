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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

// Captured from e2b Python SDK 2.56.0 Template.to_json(). It pins the actual
// SDK wire shape: context setters are separate steps, list-form RUN commands
// are joined with &&, skip_cache sets step force, and start/ready are strings.
const sdk256ImageRecipe = `{
  "steps": [
    {"type":"WORKDIR","args":["/app"],"force":false},
    {"type":"USER","args":["root"],"force":false},
    {"type":"ENV","args":["MODE","build","PORT","8080"],"force":false},
    {"type":"RUN","args":["python -m pip install numpy && python -m pip install pandas","agent"],"force":false},
    {"type":"RUN","args":["echo forced"],"force":true}
  ],
  "force": false,
  "fromImage": "python:3.12-slim",
  "startCmd": "python server.py",
  "readyCmd": "curl -f localhost:8080/health"
}`

// Captured from e2b Python SDK 2.56.0 Template.to_json() for
// Template().from_template("python-base").run_cmd("echo derived").
const sdk256FromTemplateRecipe = `{
  "steps": [{"type":"RUN","args":["echo derived"],"force":false}],
  "force": false,
  "fromTemplate": "python-base"
}`

// Captured from e2b Python SDK 2.56.0 after calling skip_cache before
// from_image. Both the whole-build force and the generated step force are
// accepted, but neither is persisted because this implementation has no
// build-step cache.
const sdk256GlobalForceRecipe = `{
  "steps": [{"type":"RUN","args":["echo hello"],"force":true}],
  "force": true,
  "fromImage": "python:3.12-slim"
}`

func TestTemplateBuildExtensionHeaderNames(t *testing.T) {
	assert.Equal(t, "x-e2b-kruise-auto-warm", models.ExtensionHeaderAutoWarm)
	assert.Equal(t, "x-e2b-kruise-warmpool-size", models.ExtensionHeaderWarmPoolSize)
}

func TestConvertBuildRecipeSDK256WireFixture(t *testing.T) {
	tests := []struct {
		name     string
		fixture  string
		expected *convertedBuild
		check    func(t *testing.T, request models.TemplateBuildStartV2)
	}{
		{
			name:    "from image recipe",
			fixture: sdk256ImageRecipe,
			expected: &convertedBuild{
				Image: "python:3.12-slim",
				BuildOptions: agentsv1alpha1.SandboxTemplateBuildOptions{
					Checkpoint: true,
					Steps: []agentsv1alpha1.TemplateBuildCommand{
						{
							Command: "python -m pip install numpy && python -m pip install pandas",
							User:    "agent",
							Cwd:     "/app",
							Env:     map[string]string{"MODE": "build", "PORT": "8080"},
						},
						{
							Command: "echo forced",
							User:    "root",
							Cwd:     "/app",
							Env:     map[string]string{"MODE": "build", "PORT": "8080"},
						},
					},
					StartCommand: &agentsv1alpha1.TemplateBuildCommand{
						Command: "python server.py",
						User:    "root",
						Cwd:     "/app",
						Env:     map[string]string{"MODE": "build", "PORT": "8080"},
					},
					ReadyCommand: &agentsv1alpha1.TemplateBuildCommand{
						Command: "curl -f localhost:8080/health",
						User:    "root",
						Cwd:     "/app",
						Env:     map[string]string{"MODE": "build", "PORT": "8080"},
					},
				},
			},
			check: func(t *testing.T, request models.TemplateBuildStartV2) {
				require.Len(t, request.Steps, 5)
				assert.False(t, request.Steps[3].Force)
				assert.True(t, request.Steps[4].Force)
			},
		},
		{
			name:    "from template recipe",
			fixture: sdk256FromTemplateRecipe,
			expected: &convertedBuild{
				BuildOptions: agentsv1alpha1.SandboxTemplateBuildOptions{
					FromTemplate: "python-base",
					Checkpoint:   true,
					Steps:        []agentsv1alpha1.TemplateBuildCommand{{Command: "echo derived"}},
				},
			},
		},
		{
			name:    "global force recipe",
			fixture: sdk256GlobalForceRecipe,
			expected: &convertedBuild{
				Image: "python:3.12-slim",
				BuildOptions: agentsv1alpha1.SandboxTemplateBuildOptions{
					Checkpoint: true,
					Steps:      []agentsv1alpha1.TemplateBuildCommand{{Command: "echo hello"}},
				},
			},
			check: func(t *testing.T, request models.TemplateBuildStartV2) {
				assert.True(t, request.Force)
				require.Len(t, request.Steps, 1)
				assert.True(t, request.Steps[0].Force)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var request models.TemplateBuildStartV2
			require.NoError(t, json.Unmarshal([]byte(tt.fixture), &request))
			if tt.check != nil {
				tt.check(t, request)
			}

			converted, apiErr := convertBuildRecipe(request)
			require.Nil(t, apiErr)
			assert.Equal(t, tt.expected, converted)
		})
	}
}

func TestTemplateBuildStartV2PreservesRegistryPresence(t *testing.T) {
	var request models.TemplateBuildStartV2
	require.NoError(t, json.Unmarshal([]byte(`{"fromImage":"image","fromImageRegistry":null}`), &request))
	assert.Equal(t, json.RawMessage("null"), request.FromImageRegistry)

	converted, apiErr := convertBuildRecipe(request)
	require.NotNil(t, apiErr)
	assert.Contains(t, apiErr.Message, "fromImageRegistry")
	assert.Nil(t, converted)
}

func TestConvertBuildRecipeFolding(t *testing.T) {
	tests := []struct {
		name     string
		request  models.TemplateBuildStartV2
		expected *convertedBuild
	}{
		{
			name: "RUN snapshots the current context and locally overrides user",
			request: models.TemplateBuildStartV2{
				FromImage: strPtr("example/runtime@sha256:abc"),
				Steps: []models.TemplateStep{
					{Type: "WORKDIR", Args: []string{"/workspace"}},
					{Type: "USER", Args: []string{"agent"}},
					{Type: "ENV", Args: []string{"A", "1", "B", "2"}},
					runStep("echo default"),
					{Type: "RUN", Args: []string{"apt-get update", "root"}},
				},
			},
			expected: &convertedBuild{
				Image: "example/runtime@sha256:abc",
				BuildOptions: agentsv1alpha1.SandboxTemplateBuildOptions{
					Checkpoint: true,
					Steps: []agentsv1alpha1.TemplateBuildCommand{
						{Command: "echo default", User: "agent", Cwd: "/workspace", Env: map[string]string{"A": "1", "B": "2"}},
						{Command: "apt-get update", User: "root", Cwd: "/workspace", Env: map[string]string{"A": "1", "B": "2"}},
					},
				},
			},
		},
		{
			name: "ENV overwrites same-name keys and snapshots each command",
			request: models.TemplateBuildStartV2{
				FromImage: strPtr("example/runtime@sha256:abc"),
				Steps: []models.TemplateStep{
					{Type: "ENV", Args: []string{"MODE", "build"}},
					runStep("echo build"),
					{Type: "ENV", Args: []string{"MODE", "run"}},
					runStep("echo run"),
				},
			},
			expected: &convertedBuild{
				Image: "example/runtime@sha256:abc",
				BuildOptions: agentsv1alpha1.SandboxTemplateBuildOptions{
					Checkpoint: true,
					Steps: []agentsv1alpha1.TemplateBuildCommand{
						{Command: "echo build", Env: map[string]string{"MODE": "build"}},
						{Command: "echo run", Env: map[string]string{"MODE": "run"}},
					},
				},
			},
		},
		{
			name: "context after the last RUN shapes start and ready",
			request: models.TemplateBuildStartV2{
				FromTemplate: strPtr("base"),
				Steps: []models.TemplateStep{
					runStep("prepare"),
					{Type: "WORKDIR", Args: []string{"/srv"}},
					{Type: "USER", Args: []string{"agent"}},
					{Type: "ENV", Args: []string{"PORT", "8080"}},
				},
				StartCmd: strPtr("python server.py"),
				ReadyCmd: strPtr("curl -f localhost:8080/health"),
			},
			expected: &convertedBuild{
				BuildOptions: agentsv1alpha1.SandboxTemplateBuildOptions{
					FromTemplate: "base",
					Checkpoint:   true,
					Steps:        []agentsv1alpha1.TemplateBuildCommand{{Command: "prepare"}},
					StartCommand: &agentsv1alpha1.TemplateBuildCommand{
						Command: "python server.py", User: "agent", Cwd: "/srv", Env: map[string]string{"PORT": "8080"},
					},
					ReadyCommand: &agentsv1alpha1.TemplateBuildCommand{
						Command: "curl -f localhost:8080/health", User: "agent", Cwd: "/srv", Env: map[string]string{"PORT": "8080"},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			converted, apiErr := convertBuildRecipe(tt.request)
			require.Nil(t, apiErr)
			assert.Equal(t, tt.expected, converted)
		})
	}
}

func TestConvertBuildRecipeCopiesCommandEnvironment(t *testing.T) {
	request := models.TemplateBuildStartV2{
		FromImage: strPtr("image"),
		Steps: []models.TemplateStep{
			{Type: "ENV", Args: []string{"MODE", "build"}},
			runStep("first"),
			runStep("second"),
		},
		StartCmd: strPtr("start"),
		ReadyCmd: strPtr("ready"),
	}
	converted, apiErr := convertBuildRecipe(request)
	require.Nil(t, apiErr)

	converted.BuildOptions.Steps[0].Env["MODE"] = "changed"
	assert.Equal(t, "build", converted.BuildOptions.Steps[1].Env["MODE"])
	assert.Equal(t, "build", converted.BuildOptions.StartCommand.Env["MODE"])
	assert.Equal(t, "build", converted.BuildOptions.ReadyCommand.Env["MODE"])
}

func TestConvertBuildRecipeRejects(t *testing.T) {
	valid := func(mutate func(*models.TemplateBuildStartV2)) models.TemplateBuildStartV2 {
		request := models.TemplateBuildStartV2{
			FromImage: strPtr("example/runtime@sha256:abc"),
			Steps:     []models.TemplateStep{runStep("echo hello")},
		}
		mutate(&request)
		return request
	}

	tests := []struct {
		name        string
		request     models.TemplateBuildStartV2
		wantMessage string
	}{
		{
			name:        "a build base is required",
			request:     models.TemplateBuildStartV2{},
			wantMessage: "exactly one of fromImage or fromTemplate",
		},
		{
			name:        "fromImage must not be empty",
			request:     models.TemplateBuildStartV2{FromImage: strPtr(" ")},
			wantMessage: "fromImage must not be empty",
		},
		{
			name:        "fromTemplate must not be empty",
			request:     models.TemplateBuildStartV2{FromTemplate: strPtr("")},
			wantMessage: "fromTemplate must not be empty",
		},
		{
			name: "fromImage and fromTemplate are mutually exclusive",
			request: valid(func(r *models.TemplateBuildStartV2) {
				r.FromTemplate = strPtr("base")
			}),
			wantMessage: "mutually exclusive",
		},
		{
			name: "fromImageRegistry is rejected",
			request: valid(func(r *models.TemplateBuildStartV2) {
				r.FromImageRegistry = json.RawMessage(`{"type":"general"}`)
			}),
			wantMessage: "fromImageRegistry",
		},
		{
			name: "COPY is rejected",
			request: valid(func(r *models.TemplateBuildStartV2) {
				r.Steps = []models.TemplateStep{{Type: "COPY", Args: []string{"src", "dst"}}}
			}),
			wantMessage: `type "COPY" is not supported`,
		},
		{
			name: "ADD is rejected",
			request: valid(func(r *models.TemplateBuildStartV2) {
				r.Steps = []models.TemplateStep{{Type: "ADD", Args: []string{"src", "dst"}}}
			}),
			wantMessage: `type "ADD" is not supported`,
		},
		{
			name: "unknown step type is rejected",
			request: valid(func(r *models.TemplateBuildStartV2) {
				r.Steps = []models.TemplateStep{{Type: "ARG", Args: []string{"x"}}}
			}),
			wantMessage: `type "ARG" is not supported`,
		},
		{
			name: "relative WORKDIR is rejected",
			request: valid(func(r *models.TemplateBuildStartV2) {
				r.Steps = []models.TemplateStep{{Type: "WORKDIR", Args: []string{"app"}}}
			}),
			wantMessage: "absolute path",
		},
		{
			name: "WORKDIR without args is rejected",
			request: valid(func(r *models.TemplateBuildStartV2) {
				r.Steps = []models.TemplateStep{{Type: "WORKDIR"}}
			}),
			wantMessage: "absolute path",
		},
		{
			name: "USER without a value is rejected",
			request: valid(func(r *models.TemplateBuildStartV2) {
				r.Steps = []models.TemplateStep{{Type: "USER", Args: []string{" "}}}
			}),
			wantMessage: "non-empty user",
		},
		{
			name: "odd ENV args are rejected",
			request: valid(func(r *models.TemplateBuildStartV2) {
				r.Steps = []models.TemplateStep{{Type: "ENV", Args: []string{"KEY"}}}
			}),
			wantMessage: "key/value pairs",
		},
		{
			name: "RUN without a command is rejected",
			request: valid(func(r *models.TemplateBuildStartV2) {
				r.Steps = []models.TemplateStep{{Type: "RUN"}}
			}),
			wantMessage: "non-empty command",
		},
		{
			name: "RUN with a blank command is rejected",
			request: valid(func(r *models.TemplateBuildStartV2) {
				r.Steps = []models.TemplateStep{{Type: "RUN", Args: []string{" "}}}
			}),
			wantMessage: "non-empty command",
		},
		{
			name: "RUN with extra args is rejected",
			request: valid(func(r *models.TemplateBuildStartV2) {
				r.Steps = []models.TemplateStep{{Type: "RUN", Args: []string{"echo", "user", "extra"}}}
			}),
			wantMessage: "at most",
		},
		{
			name: "empty startCmd is rejected",
			request: valid(func(r *models.TemplateBuildStartV2) {
				r.StartCmd = strPtr(" ")
			}),
			wantMessage: "startCmd must not be empty",
		},
		{
			name: "empty readyCmd is rejected",
			request: valid(func(r *models.TemplateBuildStartV2) {
				r.ReadyCmd = strPtr("")
			}),
			wantMessage: "readyCmd must not be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			converted, apiErr := convertBuildRecipe(tt.request)
			require.NotNil(t, apiErr)
			assert.Equal(t, http.StatusBadRequest, apiErr.Code)
			assert.Contains(t, apiErr.Message, tt.wantMessage)
			assert.Nil(t, converted)
		})
	}
}

func runStep(command string) models.TemplateStep {
	return models.TemplateStep{Type: "RUN", Args: []string{command}}
}

func strPtr(value string) *string { return &value }
