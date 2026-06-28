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
	"fmt"

	quotaspec "github.com/openkruise/agents/pkg/sandbox-manager/quota/spec"
)

// Transitional re-exports so existing models.Quota* references keep compiling during
// the quota relocation. Removed in Task 9 after every caller migrates to quotaspec.*.
type (
	QuotaSpec      = quotaspec.QuotaSpec
	QuotaLimit     = quotaspec.QuotaLimit
	QuotaDimension = quotaspec.QuotaDimension
	QuotaScope     = quotaspec.QuotaScope
)

const (
	DimSandboxCount = quotaspec.DimSandboxCount
	DimLimitsCPU    = quotaspec.DimLimitsCPU
	DimLimitsMemory = quotaspec.DimLimitsMemory
	ScopeAll        = quotaspec.ScopeAll
	ScopeRunning    = quotaspec.ScopeRunning
)

var (
	ErrQuotaLimitNegative = quotaspec.ErrQuotaLimitNegative
	NormalizeQuotaSpec    = quotaspec.NormalizeQuotaSpec
)

// quotaWireDimensions defines the canonical full-key iteration order for
// wire format encoding and decoding.
var quotaWireDimensions = []quotaspec.QuotaDimension{
	quotaspec.DimSandboxCount,
	quotaspec.DimLimitsCPU,
	quotaspec.DimLimitsMemory,
}

// QuotaSpecFromWire decodes the public wire format (scope → full-dimension → limit)
// into a validated QuotaSpec. Short keys such as "count", "cpu", and "memory" are
// rejected; callers must use the full keys "sandbox.count", "limits.cpu", and
// "limits.memory".
func QuotaSpecFromWire(raw json.RawMessage) (*quotaspec.QuotaSpec, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}

	var wire map[string]map[string]int64
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("unmarshal quota wire: %w", err)
	}
	if len(wire) == 0 {
		return nil, nil
	}

	for scopeName, dims := range wire {
		scope := quotaspec.QuotaScope(scopeName)
		if err := validateQuotaScope(scope); err != nil {
			return nil, err
		}
		for dimName := range dims {
			if _, err := quotaDimensionFromWireKey(dimName); err != nil {
				return nil, err
			}
		}
	}

	spec := &quotaspec.QuotaSpec{}
	for _, scope := range []quotaspec.QuotaScope{quotaspec.ScopeRunning, quotaspec.ScopeAll} {
		dims, ok := wire[string(scope)]
		if !ok {
			continue
		}
		for _, dimension := range quotaWireDimensions {
			limit, exists := dims[string(dimension)]
			if !exists {
				continue
			}
			spec.Limits = append(spec.Limits, quotaspec.QuotaLimit{
				Dimension: dimension,
				Scope:     scope,
				Limit:     limit,
			})
		}
	}

	return quotaspec.NormalizeQuotaSpec(spec)
}

// WireFromQuotaSpec encodes a QuotaSpec into the public wire format using full
// dimension keys ("sandbox.count", "limits.cpu", "limits.memory").
func WireFromQuotaSpec(spec *quotaspec.QuotaSpec) json.RawMessage {
	if spec == nil || len(spec.Limits) == 0 {
		return nil
	}

	wire := make(map[string]map[string]int64, 2)
	for _, limit := range spec.Limits {
		scopeKey := string(limit.Scope)
		if _, ok := wire[scopeKey]; !ok {
			wire[scopeKey] = map[string]int64{}
		}
		wire[scopeKey][string(limit.Dimension)] = limit.Limit
	}

	raw, _ := json.Marshal(wire)
	return raw
}

// DecodeQuotaSpec delegates to the relocated quotaspec.DecodeQuotaSpec.
func DecodeQuotaSpec(raw []byte) (*quotaspec.QuotaSpec, error) { return quotaspec.DecodeQuotaSpec(raw) }

// MarshalQuotaSpec delegates to the relocated quotaspec.MarshalQuotaSpec.
func MarshalQuotaSpec(spec *quotaspec.QuotaSpec) ([]byte, error) {
	return quotaspec.MarshalQuotaSpec(spec)
}

func validateQuotaScope(scope quotaspec.QuotaScope) error {
	switch scope {
	case quotaspec.ScopeAll, quotaspec.ScopeRunning:
		return nil
	default:
		return fmt.Errorf("unsupported quota scope %q", scope)
	}
}

func quotaDimensionFromWireKey(key string) (quotaspec.QuotaDimension, error) {
	switch quotaspec.QuotaDimension(key) {
	case quotaspec.DimSandboxCount, quotaspec.DimLimitsCPU, quotaspec.DimLimitsMemory:
		return quotaspec.QuotaDimension(key), nil
	case "count", "cpu", "memory":
		return "", fmt.Errorf("unsupported quota dimension %q; use full key", key)
	default:
		return "", fmt.Errorf("unsupported quota dimension %q", key)
	}
}
