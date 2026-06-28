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

package spec

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
		want        *QuotaSpec
		expectError string
	}{
		{name: "nil is unlimited", in: nil, want: nil},
		{name: "empty is unlimited", in: &QuotaSpec{}, want: nil},
		{
			name: "zero limit is valid",
			in:   &QuotaSpec{Limits: []QuotaLimit{{Dimension: DimSandboxCount, Scope: ScopeRunning, Limit: 0}}},
			want: &QuotaSpec{Limits: []QuotaLimit{{Dimension: DimSandboxCount, Scope: ScopeRunning, Limit: 0}}},
		},
		{
			name:        "negative limit rejected",
			in:          &QuotaSpec{Limits: []QuotaLimit{{Dimension: DimSandboxCount, Scope: ScopeRunning, Limit: -1}}},
			expectError: "quota limit must be non-negative",
		},
		{
			name: "duplicate pair rejected",
			in: &QuotaSpec{Limits: []QuotaLimit{
				{Dimension: DimSandboxCount, Scope: ScopeRunning, Limit: 1},
				{Dimension: DimSandboxCount, Scope: ScopeRunning, Limit: 2},
			}},
			expectError: "duplicate quota limit",
		},
		{
			name:        "unsupported dimension rejected",
			in:          &QuotaSpec{Limits: []QuotaLimit{{Dimension: QuotaDimension("cpu"), Scope: ScopeRunning, Limit: 1}}},
			expectError: "unsupported quota dimension",
		},
		{
			name:        "unsupported scope rejected",
			in:          &QuotaSpec{Limits: []QuotaLimit{{Dimension: DimSandboxCount, Scope: QuotaScope("template:python"), Limit: 1}}},
			expectError: "unsupported quota scope",
		},
		{
			name: "count over all",
			in: &QuotaSpec{Limits: []QuotaLimit{{
				Dimension: DimSandboxCount,
				Scope:     ScopeAll,
				Limit:     50,
			}}},
			want: &QuotaSpec{Limits: []QuotaLimit{{
				Dimension: DimSandboxCount,
				Scope:     ScopeAll,
				Limit:     50,
			}}},
		},
		{
			name: "cpu memory count over running and all",
			in: &QuotaSpec{Limits: []QuotaLimit{
				{Dimension: DimLimitsCPU, Scope: ScopeRunning, Limit: 8000},
				{Dimension: DimLimitsMemory, Scope: ScopeRunning, Limit: 16384},
				{Dimension: DimSandboxCount, Scope: ScopeAll, Limit: 50},
			}},
			want: &QuotaSpec{Limits: []QuotaLimit{
				{Dimension: DimLimitsCPU, Scope: ScopeRunning, Limit: 8000},
				{Dimension: DimLimitsMemory, Scope: ScopeRunning, Limit: 16384},
				{Dimension: DimSandboxCount, Scope: ScopeAll, Limit: 50},
			}},
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
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDecodeQuotaSpec(t *testing.T) {
	tests := []struct {
		name        string
		raw         []byte
		want        *QuotaSpec
		expectError string
	}{
		{name: "nil is unlimited", raw: nil, want: nil},
		{name: "empty is unlimited", raw: []byte(""), want: nil},
		{name: "null is unlimited", raw: []byte("null"), want: nil},
		{
			name: "valid stored spec",
			raw:  []byte(`{"limits":[{"dimension":"sandbox.count","scope":"all","limit":50}]}`),
			want: &QuotaSpec{Limits: []QuotaLimit{{Dimension: DimSandboxCount, Scope: ScopeAll, Limit: 50}}},
		},
		{
			name:        "missing limits field",
			raw:         []byte(`{}`),
			expectError: "stored quota missing limits",
		},
		{
			name:        "extra fields rejected",
			raw:         []byte(`{"limits":[],"extra":true}`),
			expectError: "stored quota contains unsupported fields",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeQuotaSpec(tt.raw)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMarshalQuotaSpec(t *testing.T) {
	tests := []struct {
		name string
		spec *QuotaSpec
		want string
	}{
		{name: "nil produces nil", spec: nil, want: ""},
		{name: "empty produces nil", spec: &QuotaSpec{}, want: ""},
		{
			name: "valid spec round trips",
			spec: &QuotaSpec{Limits: []QuotaLimit{{Dimension: DimSandboxCount, Scope: ScopeAll, Limit: 50}}},
			want: `{"limits":[{"dimension":"sandbox.count","scope":"all","limit":50}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MarshalQuotaSpec(tt.spec)
			require.NoError(t, err)
			if tt.want == "" {
				assert.Nil(t, got)
				return
			}
			assert.JSONEq(t, tt.want, string(got))
		})
	}
}

func TestQuotaSpecMarshalRoundTrip(t *testing.T) {
	original := &QuotaSpec{Limits: []QuotaLimit{
		{Dimension: DimLimitsCPU, Scope: ScopeRunning, Limit: 8000},
		{Dimension: DimLimitsMemory, Scope: ScopeRunning, Limit: 16384},
		{Dimension: DimSandboxCount, Scope: ScopeAll, Limit: 50},
	}}

	raw, err := MarshalQuotaSpec(original)
	require.NoError(t, err)

	decoded, err := DecodeQuotaSpec(raw)
	require.NoError(t, err)
	assert.Equal(t, original, decoded)
}

func TestQuotaSpecIsLimited(t *testing.T) {
	var nilSpec *QuotaSpec
	assert.False(t, nilSpec.IsLimited())
	assert.False(t, (&QuotaSpec{}).IsLimited())
	assert.True(t, (&QuotaSpec{Limits: []QuotaLimit{{Dimension: DimSandboxCount, Scope: ScopeAll, Limit: 1}}}).IsLimited())
}

func TestQuotaSpecDeepCopy(t *testing.T) {
	var nilSpec *QuotaSpec
	assert.Nil(t, nilSpec.DeepCopy())

	original := &QuotaSpec{Limits: []QuotaLimit{{Dimension: DimSandboxCount, Scope: ScopeAll, Limit: 50}}}
	copied := original.DeepCopy()
	assert.Equal(t, original, copied)
	copied.Limits[0].Limit = 100
	assert.NotEqual(t, original.Limits[0].Limit, copied.Limits[0].Limit)
}

func TestQuotaSpecLimitedPairs(t *testing.T) {
	var nilSpec *QuotaSpec
	assert.Nil(t, nilSpec.LimitedPairs())

	spec := &QuotaSpec{Limits: []QuotaLimit{
		{Dimension: DimSandboxCount, Scope: ScopeAll, Limit: 50},
		{Dimension: DimLimitsCPU, Scope: ScopeRunning, Limit: 8000},
	}}
	pairs := spec.LimitedPairs()
	assert.Equal(t, int64(50), pairs[DimSandboxCount][ScopeAll])
	assert.Equal(t, int64(8000), pairs[DimLimitsCPU][ScopeRunning])
}

func TestMarshalQuotaSpecJSON(t *testing.T) {
	spec := &QuotaSpec{Limits: []QuotaLimit{{Dimension: DimSandboxCount, Scope: ScopeAll, Limit: 50}}}
	raw, err := json.Marshal(spec)
	require.NoError(t, err)
	assert.JSONEq(t, `{"limits":[{"dimension":"sandbox.count","scope":"all","limit":50}]}`, string(raw))
}
