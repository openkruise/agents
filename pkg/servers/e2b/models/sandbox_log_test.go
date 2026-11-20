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

package models

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// Credentials that must never reach a log line.
const (
	testEnvSecret    = "env-secret-must-not-leak"
	testHeaderSecret = "header-secret-must-not-leak"
)

// testNewSandboxRequestBody is a create request carrying both kinds of
// user-supplied credentials: an env var value and a network transform header.
const testNewSandboxRequestBody = `{
	"templateID": "code-interpreter",
	"metadata": {"owner": "team-a"},
	"envVars": {"API_KEY": "` + testEnvSecret + `"},
	"network": {
		"allowOut": ["api.example.com"],
		"rules": {"api.example.com": [{"transform": {"headers": {"Authorization": "` + testHeaderSecret + `"}}}]}
	}
}`

// TestNewSandboxRequestMarshalLogRedactsSecrets pins the log-safety contract of
// NewSandboxRequest across the renderings a create request goes through: the
// MarshalLog view and the zap logger sandbox-manager installs. Names stay visible
// for diagnostics, values never do.
func TestNewSandboxRequestMarshalLogRedactsSecrets(t *testing.T) {
	var request NewSandboxRequest
	require.NoError(t, json.Unmarshal([]byte(testNewSandboxRequestBody), &request))

	marshalLog, err := json.Marshal(request.MarshalLog())
	require.NoError(t, err)

	var zapOutput bytes.Buffer
	zap.New(zap.WriteTo(&zapOutput)).Info("create sandbox request received", "request", request)

	tests := []struct {
		name     string
		rendered string
	}{
		{name: "MarshalLog", rendered: string(marshalLog)},
		{name: "zap logger", rendered: zapOutput.String()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NotContains(t, tt.rendered, testEnvSecret, "env var values must never be rendered")
			assert.NotContains(t, tt.rendered, testHeaderSecret, "transform header values must never be rendered")
			for _, visible := range []string{"code-interpreter", "team-a", "API_KEY", "Authorization", "api.example.com", redactedLogValue} {
				assert.Contains(t, tt.rendered, visible)
			}
		})
	}

	// The redaction works on copies: the request used to create the sandbox
	// keeps its real values.
	assert.Equal(t, testEnvSecret, request.EnvVars["API_KEY"])
	assert.Equal(t, testHeaderSecret, request.Network.Rules["api.example.com"][0].Transform.Headers["Authorization"])
}

// TestNewSandboxRequestMarshalLogEmptyFields pins that MarshalLog neither panics
// on nor invents values for absent optional fields.
func TestNewSandboxRequestMarshalLogEmptyFields(t *testing.T) {
	tests := []struct {
		name    string
		request NewSandboxRequest
	}{
		{name: "no env vars and no network", request: NewSandboxRequest{TemplateID: "base"}},
		{name: "network without rules", request: NewSandboxRequest{TemplateID: "base", Network: &SandboxNetworkConfig{AllowOut: []string{"1.1.1.1"}}}},
		{name: "rule without transform", request: NewSandboxRequest{TemplateID: "base", Network: &SandboxNetworkConfig{
			Rules: map[string][]SandboxNetworkRule{"api.example.com": {{}}},
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want, err := json.Marshal(newSandboxRequestLogView(tt.request))
			require.NoError(t, err)
			got, err := json.Marshal(tt.request.MarshalLog())
			require.NoError(t, err)
			assert.JSONEq(t, string(want), string(got))
			assert.NotContains(t, string(got), redactedLogValue)
		})
	}
}
