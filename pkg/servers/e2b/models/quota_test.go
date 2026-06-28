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
)

func TestNormalizeQuotaSpec(t *testing.T) {
	tests := []struct {
		name        string
		in          *QuotaSpec
		wantLimited bool
		wantLen     int
		expectError string
	}{
		{name: "nil is unlimited", in: nil},
		{name: "empty is unlimited", in: &QuotaSpec{}},
		{
			name: "count over all",
			in: &QuotaSpec{Limits: []QuotaLimit{{
				Dimension: DimSandboxCount,
				Scope:     ScopeAll,
				Limit:     50,
			}}},
			wantLimited: true,
			wantLen:     1,
		},
		{
			name: "limit zero is valid hard zero",
			in: &QuotaSpec{Limits: []QuotaLimit{{
				Dimension: DimSandboxCount,
				Scope:     ScopeRunning,
				Limit:     0,
			}}},
			wantLimited: true,
			wantLen:     1,
		},
		{
			name: "negative rejected",
			in: &QuotaSpec{Limits: []QuotaLimit{{
				Dimension: DimLimitsCPU,
				Scope:     ScopeAll,
				Limit:     -1,
			}}},
			expectError: "non-negative",
		},
		{
			name: "duplicate dimension scope rejected",
			in: &QuotaSpec{Limits: []QuotaLimit{
				{Dimension: DimSandboxCount, Scope: ScopeAll, Limit: 1},
				{Dimension: DimSandboxCount, Scope: ScopeAll, Limit: 2},
			}},
			expectError: "duplicate",
		},
		{
			name: "unknown dimension rejected",
			in: &QuotaSpec{Limits: []QuotaLimit{{
				Dimension: QuotaDimension("limits.gpu"),
				Scope:     ScopeAll,
				Limit:     1,
			}}},
			expectError: "unsupported quota dimension",
		},
		{
			name: "unknown scope rejected",
			in: &QuotaSpec{Limits: []QuotaLimit{{
				Dimension: DimSandboxCount,
				Scope:     QuotaScope("template:x"),
				Limit:     1,
			}}},
			expectError: "unsupported quota scope",
		},
		{
			name: "cpu memory count over running and all",
			in: &QuotaSpec{Limits: []QuotaLimit{
				{Dimension: DimLimitsCPU, Scope: ScopeRunning, Limit: 8000},
				{Dimension: DimLimitsMemory, Scope: ScopeRunning, Limit: 16384},
				{Dimension: DimSandboxCount, Scope: ScopeAll, Limit: 50},
			}},
			wantLimited: true,
			wantLen:     3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeQuotaSpec(tt.in)
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

func TestQuotaSpecWireRoundTrip(t *testing.T) {
	tests := []struct {
		name        string
		raw         json.RawMessage
		want        *QuotaSpec
		expectError string
	}{
		{
			name: "full keys running and all round trip",
			raw:  json.RawMessage(`{"running":{"limits.cpu":8000,"limits.memory":16384,"sandbox.count":10},"all":{"sandbox.count":50}}`),
			want: &QuotaSpec{Limits: []QuotaLimit{
				{Dimension: DimSandboxCount, Scope: ScopeRunning, Limit: 10},
				{Dimension: DimLimitsCPU, Scope: ScopeRunning, Limit: 8000},
				{Dimension: DimLimitsMemory, Scope: ScopeRunning, Limit: 16384},
				{Dimension: DimSandboxCount, Scope: ScopeAll, Limit: 50},
			}},
		},
		{
			name: "full key sandbox.count running",
			raw:  json.RawMessage(`{"running":{"sandbox.count":2}}`),
			want: &QuotaSpec{Limits: []QuotaLimit{
				{Dimension: DimSandboxCount, Scope: ScopeRunning, Limit: 2},
			}},
		},
		{name: "null is unlimited", raw: json.RawMessage(`null`)},
		{name: "empty object is unlimited", raw: json.RawMessage(`{}`)},
		{
			name:        "unknown scope rejected",
			raw:         json.RawMessage(`{"template:x":{"sandbox.count":1}}`),
			expectError: "unsupported quota scope",
		},
		{
			name:        "unknown dimension rejected",
			raw:         json.RawMessage(`{"running":{"gpu":1}}`),
			expectError: "unsupported quota dimension",
		},
		{
			name:        "short key count rejected",
			raw:         json.RawMessage(`{"running":{"count":1}}`),
			expectError: "use full key",
		},
		{
			name:        "short key cpu rejected",
			raw:         json.RawMessage(`{"running":{"cpu":8000}}`),
			expectError: "use full key",
		},
		{
			name:        "short key memory rejected",
			raw:         json.RawMessage(`{"running":{"memory":16384}}`),
			expectError: "use full key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := QuotaSpecFromWire(tt.raw)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}

			require.NoError(t, err)
			if tt.want == nil {
				assert.Nil(t, got)
				assert.Nil(t, WireFromQuotaSpec(got))
				return
			}

			require.Equal(t, tt.want, got)
			assert.JSONEq(t, string(tt.raw), string(WireFromQuotaSpec(got)))
		})
	}
}

func TestWireFromQuotaSpecEmitsFullKeys(t *testing.T) {
	spec := &QuotaSpec{Limits: []QuotaLimit{
		{Dimension: DimSandboxCount, Scope: ScopeRunning, Limit: 2},
		{Dimension: DimLimitsCPU, Scope: ScopeRunning, Limit: 8000},
		{Dimension: DimLimitsMemory, Scope: ScopeRunning, Limit: 16384},
		{Dimension: DimSandboxCount, Scope: ScopeAll, Limit: 50},
	}}
	raw := WireFromQuotaSpec(spec)
	assert.JSONEq(t,
		`{"running":{"limits.cpu":8000,"limits.memory":16384,"sandbox.count":2},"all":{"sandbox.count":50}}`,
		string(raw))
	assert.NotContains(t, string(raw), `"count"`)
	assert.NotContains(t, string(raw), `"cpu"`)
	assert.NotContains(t, string(raw), `"memory"`)
}

func TestMarshalCreatedTeamAPIKeyQuotaKeepsExistingRawMessage(t *testing.T) {
	key := CreatedTeamAPIKey{
		Quota: json.RawMessage(`{"running":{"limits.cpu":8000,"limits.memory":16384},"all":{"sandbox.count":50}}`),
		QuotaSpec: &QuotaSpec{Limits: []QuotaLimit{{
			Dimension: QuotaDimension("limits.gpu"),
			Scope:     ScopeRunning,
			Limit:     1,
		}}},
	}

	raw, err := json.Marshal(key)
	require.NoError(t, err)
	var payload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.Contains(t, payload, "quota")
	assert.JSONEq(t, `{"running":{"limits.cpu":8000,"limits.memory":16384},"all":{"sandbox.count":50}}`, string(payload["quota"]))
	assert.NotContains(t, string(raw), `"limits"`)
}
