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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	quotaspec "github.com/openkruise/agents/pkg/sandbox-manager/quota/spec"
)

func TestNormalizeQuotaSpec(t *testing.T) {
	tests := []struct {
		name        string
		in          *quotaspec.QuotaSpec
		wantLimited bool
		wantLen     int
		expectError string
	}{
		{name: "nil is unlimited", in: nil},
		{name: "empty is unlimited", in: &quotaspec.QuotaSpec{}},
		{
			name: "count over all",
			in: &quotaspec.QuotaSpec{Limits: []quotaspec.QuotaLimit{{
				Dimension: quotaspec.DimSandboxCount,
				Scope:     quotaspec.ScopeAll,
				Limit:     50,
			}}},
			wantLimited: true,
			wantLen:     1,
		},
		{
			name: "limit zero is valid hard zero",
			in: &quotaspec.QuotaSpec{Limits: []quotaspec.QuotaLimit{{
				Dimension: quotaspec.DimSandboxCount,
				Scope:     quotaspec.ScopeRunning,
				Limit:     0,
			}}},
			wantLimited: true,
			wantLen:     1,
		},
		{
			name: "negative rejected",
			in: &quotaspec.QuotaSpec{Limits: []quotaspec.QuotaLimit{{
				Dimension: quotaspec.DimLimitsCPU,
				Scope:     quotaspec.ScopeAll,
				Limit:     -1,
			}}},
			expectError: "non-negative",
		},
		{
			name: "duplicate dimension scope rejected",
			in: &quotaspec.QuotaSpec{Limits: []quotaspec.QuotaLimit{
				{Dimension: quotaspec.DimSandboxCount, Scope: quotaspec.ScopeAll, Limit: 1},
				{Dimension: quotaspec.DimSandboxCount, Scope: quotaspec.ScopeAll, Limit: 2},
			}},
			expectError: "duplicate",
		},
		{
			name: "unknown dimension rejected",
			in: &quotaspec.QuotaSpec{Limits: []quotaspec.QuotaLimit{{
				Dimension: quotaspec.QuotaDimension("limits.gpu"),
				Scope:     quotaspec.ScopeAll,
				Limit:     1,
			}}},
			expectError: "unsupported quota dimension",
		},
		{
			name: "unknown scope rejected",
			in: &quotaspec.QuotaSpec{Limits: []quotaspec.QuotaLimit{{
				Dimension: quotaspec.DimSandboxCount,
				Scope:     quotaspec.QuotaScope("template:x"),
				Limit:     1,
			}}},
			expectError: "unsupported quota scope",
		},
		{
			name: "cpu memory count over running and all",
			in: &quotaspec.QuotaSpec{Limits: []quotaspec.QuotaLimit{
				{Dimension: quotaspec.DimLimitsCPU, Scope: quotaspec.ScopeRunning, Limit: 8000},
				{Dimension: quotaspec.DimLimitsMemory, Scope: quotaspec.ScopeRunning, Limit: 16384},
				{Dimension: quotaspec.DimSandboxCount, Scope: quotaspec.ScopeAll, Limit: 50},
			}},
			wantLimited: true,
			wantLen:     3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := quotaspec.NormalizeQuotaSpec(tt.in)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}

			require.NoError(t, err)
			if !tt.wantLimited {
				assert.Nil(t, got)
				return
			}

			require.NotNil(t, got)
			assert.True(t, got.IsLimited())
			assert.Len(t, got.Limits, tt.wantLen)
		})
	}
}

func TestQuotaSpecLimitsRoundTrip(t *testing.T) {
	tests := []struct {
		name        string
		raw         json.RawMessage
		want        *quotaspec.QuotaSpec
		expectError string
	}{
		{
			name: "full keys running and all round trip",
			raw:  json.RawMessage(`{"limits":[{"dimension":"sandbox.count","scope":"running","limit":10},{"dimension":"limits.cpu","scope":"running","limit":8000},{"dimension":"limits.memory","scope":"running","limit":16384},{"dimension":"sandbox.count","scope":"all","limit":50}]}`),
			want: &quotaspec.QuotaSpec{Limits: []quotaspec.QuotaLimit{
				{Dimension: quotaspec.DimSandboxCount, Scope: quotaspec.ScopeRunning, Limit: 10},
				{Dimension: quotaspec.DimLimitsCPU, Scope: quotaspec.ScopeRunning, Limit: 8000},
				{Dimension: quotaspec.DimLimitsMemory, Scope: quotaspec.ScopeRunning, Limit: 16384},
				{Dimension: quotaspec.DimSandboxCount, Scope: quotaspec.ScopeAll, Limit: 50},
			}},
		},
		{
			name: "full key sandbox.count running",
			raw:  json.RawMessage(`{"limits":[{"dimension":"sandbox.count","scope":"running","limit":2}]}`),
			want: &quotaspec.QuotaSpec{Limits: []quotaspec.QuotaLimit{
				{Dimension: quotaspec.DimSandboxCount, Scope: quotaspec.ScopeRunning, Limit: 2},
			}},
		},
		{name: "null is unlimited", raw: json.RawMessage(`null`)},
		{name: "empty object is unlimited", raw: json.RawMessage(`{}`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var spec quotaspec.QuotaSpec
			err := json.Unmarshal(tt.raw, &spec)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}

			require.NoError(t, err)
			if tt.want == nil {
				assert.Empty(t, spec.Limits)
				raw, err := json.Marshal(&spec)
				require.NoError(t, err)
				assert.JSONEq(t, `{}`, string(raw))
				return
			}

			assert.Equal(t, tt.want.Limits, spec.Limits)
			raw, err := json.Marshal(&spec)
			require.NoError(t, err)
			assert.JSONEq(t, string(tt.raw), string(raw))
		})
	}
}

func TestMarshalCreatedTeamAPIKeyQuotaSpecKeepsLimitsFormat(t *testing.T) {
	key := CreatedTeamAPIKey{
		Name: "limited",
		QuotaSpec: &quotaspec.QuotaSpec{Limits: []quotaspec.QuotaLimit{
			{Dimension: quotaspec.DimLimitsCPU, Scope: quotaspec.ScopeRunning, Limit: 8000},
			{Dimension: quotaspec.DimLimitsMemory, Scope: quotaspec.ScopeRunning, Limit: 16384},
			{Dimension: quotaspec.DimSandboxCount, Scope: quotaspec.ScopeAll, Limit: 50},
		}},
	}

	raw, err := json.Marshal(key)
	require.NoError(t, err)
	var payload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.Contains(t, payload, "quota")
	assert.JSONEq(t, `{"limits":[{"dimension":"limits.cpu","scope":"running","limit":8000},{"dimension":"limits.memory","scope":"running","limit":16384},{"dimension":"sandbox.count","scope":"all","limit":50}]}`, string(payload["quota"]))
}

func TestCreatedTeamAPIKeyQuotaOmittedWhenNil(t *testing.T) {
	key := CreatedTeamAPIKey{Name: "test"}
	raw, err := json.Marshal(key)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"quota"`)
}

// TestCreatedTeamAPIKeyEmptyQuotaSpecMarshalsAsObject documents the behavior boundary
// where a non-nil but empty QuotaSpec serializes as "quota":{} rather than being
// omitted. In practice, NormalizeQuotaSpec returns nil for empty specs, so this case
// should not occur in production code. The test records the expected JSON shape for
// defensiveness.
func TestCreatedTeamAPIKeyEmptyQuotaSpecMarshalsAsObject(t *testing.T) {
	key := CreatedTeamAPIKey{
		Name:      "test",
		QuotaSpec: &quotaspec.QuotaSpec{},
	}
	raw, err := json.Marshal(key)
	require.NoError(t, err)
	var payload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.Contains(t, payload, "quota")
	assert.JSONEq(t, `{}`, string(payload["quota"]))
}
