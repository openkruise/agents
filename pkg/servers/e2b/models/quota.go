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

// quotaWireDimensions defines the canonical full-key iteration order for
// wire format encoding and decoding.
var quotaWireDimensions = []quotaspec.QuotaDimension{
	quotaspec.DimSandboxCount,
	quotaspec.DimLimitsCPU,
	quotaspec.DimLimitsMemory,
}

// QuotaWire is the typed wire-format representation of quota for JSON
// encoding and decoding in API requests and responses.
//
// JSON shape: {"running":{"sandbox.count":10,"limits.cpu":8000},"all":{"sandbox.count":50}}
type QuotaWire struct {
	// Scopes maps scope names (e.g. "running", "all") to their dimension limits.
	Scopes map[string]map[string]int64 `json:",omitempty"`
}

// MarshalJSON implements json.Marshaler. Returns JSON null for a nil receiver.
func (q *QuotaWire) MarshalJSON() ([]byte, error) {
	if q == nil {
		return []byte("null"), nil
	}
	return json.Marshal(q.Scopes)
}

// UnmarshalJSON implements json.Unmarshaler. Accepts JSON null or an object
// with scope keys.
func (q *QuotaWire) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		q.Scopes = nil
		return nil
	}
	return json.Unmarshal(data, &q.Scopes)
}

// IsEmpty reports whether the wire carries no scope data.
func (q *QuotaWire) IsEmpty() bool {
	return q == nil || len(q.Scopes) == 0
}

// ToQuotaSpec converts the wire format into a validated QuotaSpec.
// Returns nil for empty/unlimited quotas.
func (q *QuotaWire) ToQuotaSpec() (*quotaspec.QuotaSpec, error) {
	if q.IsEmpty() {
		return nil, nil
	}

	spec := &quotaspec.QuotaSpec{}
	for _, scope := range []quotaspec.QuotaScope{quotaspec.ScopeRunning, quotaspec.ScopeAll} {
		dims, ok := q.Scopes[string(scope)]
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

// QuotaWireFromSpec builds a QuotaWire from an internal QuotaSpec.
// Returns nil for nil or unlimited specs.
func QuotaWireFromSpec(spec *quotaspec.QuotaSpec) *QuotaWire {
	if spec == nil || len(spec.Limits) == 0 {
		return nil
	}

	wire := &QuotaWire{Scopes: make(map[string]map[string]int64, 2)}
	for _, limit := range spec.Limits {
		scopeKey := string(limit.Scope)
		if _, ok := wire.Scopes[scopeKey]; !ok {
			wire.Scopes[scopeKey] = map[string]int64{}
		}
		wire.Scopes[scopeKey][string(limit.Dimension)] = limit.Limit
	}
	return wire
}

// QuotaSpecFromWire decodes the public wire format into a validated QuotaSpec.
// Accepts a nil wire for unlimited quotas.
func QuotaSpecFromWire(wire *QuotaWire) (*quotaspec.QuotaSpec, error) {
	if wire == nil {
		return nil, nil
	}

	for scopeName := range wire.Scopes {
		scope := quotaspec.QuotaScope(scopeName)
		if err := validateQuotaScope(scope); err != nil {
			return nil, err
		}
		for dimName := range wire.Scopes[scopeName] {
			switch quotaspec.QuotaDimension(dimName) {
			case quotaspec.DimSandboxCount, quotaspec.DimLimitsCPU, quotaspec.DimLimitsMemory:
				// valid
			default:
				return nil, fmt.Errorf("unsupported quota dimension %q", dimName)
			}
		}
	}

	return wire.ToQuotaSpec()
}

// WireFromQuotaSpec encodes a QuotaSpec into the public wire format.
func WireFromQuotaSpec(spec *quotaspec.QuotaSpec) *QuotaWire {
	return QuotaWireFromSpec(spec)
}

func validateQuotaScope(scope quotaspec.QuotaScope) error {
	switch scope {
	case quotaspec.ScopeAll, quotaspec.ScopeRunning:
		return nil
	default:
		return fmt.Errorf("unsupported quota scope %q", scope)
	}
}
