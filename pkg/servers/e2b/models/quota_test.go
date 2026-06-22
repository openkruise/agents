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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeQuotaSpec(t *testing.T) {
	tests := []struct {
		name           string
		spec           *QuotaSpec
		expectError    string
		expectLimited  bool
		expectCount    int64
		expectHasLimit bool
	}{
		{
			name:        "nil quota is unlimited",
			spec:        nil,
			expectError: "",
		},
		{
			name:        "empty quota is unlimited",
			spec:        &QuotaSpec{},
			expectError: "",
		},
		{
			name:        "nil sandbox count is unlimited",
			spec:        &QuotaSpec{Limits: []QuotaLimit{{Dimension: DimSandboxCount}}},
			expectError: "",
		},
		{
			name:           "zero sandbox count is hard zero",
			spec:           &QuotaSpec{Limits: []QuotaLimit{{Dimension: DimSandboxCount, Limit: int64Ptr(0)}}},
			expectError:    "",
			expectLimited:  true,
			expectCount:    0,
			expectHasLimit: true,
		},
		{
			name:           "positive sandbox count is limited",
			spec:           &QuotaSpec{Limits: []QuotaLimit{{Dimension: DimSandboxCount, Limit: int64Ptr(50)}}},
			expectError:    "",
			expectLimited:  true,
			expectCount:    50,
			expectHasLimit: true,
		},
		{
			name:        "negative sandbox count is rejected",
			spec:        &QuotaSpec{Limits: []QuotaLimit{{Dimension: DimSandboxCount, Limit: int64Ptr(-1)}}},
			expectError: "limit must be non-negative",
		},
		{
			name:        "duplicate sandbox count is rejected",
			spec:        &QuotaSpec{Limits: []QuotaLimit{{Dimension: DimSandboxCount}, {Dimension: DimSandboxCount}}},
			expectError: "duplicate quota limit",
		},
		{
			name:        "unsupported dimension is rejected",
			spec:        &QuotaSpec{Limits: []QuotaLimit{{Dimension: QuotaDimension("sandbox.memory")}}},
			expectError: "unsupported quota dimension",
		},
		{
			name:        "non-empty scope is rejected",
			spec:        &QuotaSpec{Limits: []QuotaLimit{{Dimension: DimSandboxCount, Scope: QuotaScope{Template: "tmpl"}}}},
			expectError: "quota scope is not supported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeQuotaSpec(tt.spec)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				if tt.expectError == "limit must be non-negative" {
					assert.ErrorIs(t, err, ErrQuotaLimitNegative)
				}
				return
			}

			require.NoError(t, err)
			if tt.spec == nil || len(tt.spec.Limits) == 0 || (len(tt.spec.Limits) == 1 && tt.spec.Limits[0].Limit == nil) {
				assert.Nil(t, got)
				assert.False(t, tt.expectLimited)
				assert.False(t, tt.expectHasLimit)
				return
			}

			require.NotNil(t, got)
			assert.Equal(t, tt.expectLimited, got.IsLimited())

			count, ok := got.SandboxCountLimit()
			assert.Equal(t, tt.expectHasLimit, ok)
			assert.Equal(t, tt.expectCount, count)
		})
	}
}

func TestQuotaSpecJSONHelpers(t *testing.T) {
	t.Run("marshal and decode normalized quota", func(t *testing.T) {
		raw, err := MarshalQuotaSpec(&QuotaSpec{Limits: []QuotaLimit{{
			Dimension: DimSandboxCount,
			Limit:     int64Ptr(4),
		}}})
		require.NoError(t, err)
		require.NotEmpty(t, raw)
		assert.Contains(t, string(raw), `"dimension":"sandbox.count"`)

		got, err := DecodeQuotaSpec(raw)
		require.NoError(t, err)
		require.NotNil(t, got)
		count, limited := got.SandboxCountLimit()
		require.True(t, limited)
		assert.EqualValues(t, 4, count)
	})

	t.Run("unlimited quota marshals and decodes nil", func(t *testing.T) {
		raw, err := MarshalQuotaSpec(nil)
		require.NoError(t, err)
		assert.Nil(t, raw)

		got, err := DecodeQuotaSpec(nil)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("decode rejects invalid internal quota", func(t *testing.T) {
		got, err := DecodeQuotaSpec([]byte(`{"limits":[{"dimension":"sandbox.count","limit":-1}]}`))
		require.Error(t, err)
		assert.Nil(t, got)
		assert.True(t, errors.Is(err, ErrQuotaLimitNegative))
	})
}

func TestAPIKeyQuotaJSONToQuotaSpec(t *testing.T) {
	tests := []struct {
		name        string
		rawJSON     string
		expectError string
		expectNil   bool
		expectCount int64
	}{
		{
			name:      "absent quota is unlimited",
			rawJSON:   `{"name":"demo"}`,
			expectNil: true,
		},
		{
			name:      "null quota is unlimited",
			rawJSON:   `{"name":"demo","quota":null}`,
			expectNil: true,
		},
		{
			name:        "nested sandbox count is limited",
			rawJSON:     `{"name":"demo","quota":{"sandbox":{"count":2}}}`,
			expectCount: 2,
		},
		{
			name:        "nested sandbox count zero is limited",
			rawJSON:     `{"name":"demo","quota":{"sandbox":{"count":0}}}`,
			expectCount: 0,
		},
		{
			name:        "negative quota limit is rejected",
			rawJSON:     `{"name":"demo","quota":{"sandbox":{"count":-1}}}`,
			expectError: "quota limit must be non-negative",
		},
		{
			name:        "unsupported top-level quota field is rejected",
			rawJSON:     `{"name":"demo","quota":{"cpu":1}}`,
			expectError: `unsupported quota field "cpu"`,
		},
		{
			name:        "unsupported sandbox dimension is rejected",
			rawJSON:     `{"name":"demo","quota":{"sandbox":{"memory":1}}}`,
			expectError: `unsupported quota field "sandbox.memory"`,
		},
		{
			name:        "internal limits shape is rejected",
			rawJSON:     `{"name":"demo","quota":{"limits":[{"dimension":"sandbox.count","limit":1}]}}`,
			expectError: `unsupported quota field "limits"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req NewTeamAPIKey
			err := json.Unmarshal([]byte(tt.rawJSON), &req)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}

			require.NoError(t, err)
			if tt.expectNil {
				assert.Nil(t, req.QuotaSpec)
				return
			}

			require.NotNil(t, req.QuotaSpec)
			count, ok := req.QuotaSpec.SandboxCountLimit()
			require.True(t, ok)
			assert.Equal(t, tt.expectCount, count)
		})
	}
}

func TestAPIKeyQuotaFromSpecJSON(t *testing.T) {
	tests := []struct {
		name          string
		key           CreatedTeamAPIKey
		expectedQuota string
	}{
		{
			name: "internal quota spec marshals to public nested quota",
			key: CreatedTeamAPIKey{
				QuotaSpec: &QuotaSpec{
					Limits: []QuotaLimit{{
						Dimension: DimSandboxCount,
						Limit:     int64Ptr(50),
					}},
				},
			},
			expectedQuota: `"quota":{"sandbox":{"count":50}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.key)
			require.NoError(t, err)

			assert.Contains(t, string(raw), tt.expectedQuota)
			assert.NotContains(t, string(raw), `"limits"`)
			assert.NotContains(t, string(raw), "QuotaSpec")
		})
	}
}

func TestQuotaSpecJSONShape(t *testing.T) {
	tests := []struct {
		name             string
		spec             *QuotaSpec
		expectContains   []string
		expectNotContain []string
	}{
		{
			name: "sandbox count is stored in internal lower-case shape",
			spec: &QuotaSpec{
				Limits: []QuotaLimit{{
					Dimension: DimSandboxCount,
					Limit:     int64Ptr(3),
				}},
			},
			expectContains: []string{
				`"limits"`,
				`"dimension":"sandbox.count"`,
				`"limit":3`,
			},
			expectNotContain: []string{
				`"Limits"`,
				`"Dimension"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.spec)
			require.NoError(t, err)

			for _, expected := range tt.expectContains {
				assert.Contains(t, string(raw), expected)
			}
			for _, unexpected := range tt.expectNotContain {
				assert.NotContains(t, string(raw), unexpected)
			}
		})
	}
}

func TestAPIKeyQuotaInvalidInternalSpecMarshal(t *testing.T) {
	tests := []struct {
		name        string
		key         CreatedTeamAPIKey
		expectError string
	}{
		{
			name: "invalid internal quota spec is rejected during marshal",
			key: CreatedTeamAPIKey{
				QuotaSpec: &QuotaSpec{
					Limits: []QuotaLimit{{
						Dimension: DimSandboxCount,
						Limit:     int64Ptr(-1),
					}},
				},
			},
			expectError: "limit must be non-negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := json.Marshal(tt.key)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
		})
	}
}

func int64Ptr(v int64) *int64 {
	return &v
}
