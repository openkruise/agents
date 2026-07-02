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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// QuotaDimension identifies a metered resource axis.
type QuotaDimension string

const (
	DimSandboxCount QuotaDimension = "sandbox.count"
	DimLimitsCPU    QuotaDimension = "limits.cpu"
	DimLimitsMemory QuotaDimension = "limits.memory"
)

// QuotaScope defines the lifecycle window a limit applies to.
type QuotaScope string

const (
	ScopeAll     QuotaScope = "all"
	ScopeRunning QuotaScope = "running"
)

// ErrQuotaLimitNegative is returned when a quota limit is negative.
var ErrQuotaLimitNegative = errors.New("quota limit must be non-negative")

// ErrInvalidQuotaSpec is returned when a quota spec cannot be decoded or validated.
var ErrInvalidQuotaSpec = errors.New("invalid quota spec")

// QuotaLimit is a single dimension/scope/limit triple.
type QuotaLimit struct {
	Dimension QuotaDimension `json:"dimension"`
	Scope     QuotaScope     `json:"scope"`
	Limit     int64          `json:"limit"`
}

// QuotaSpec is the canonical, storage-neutral quota definition.
type QuotaSpec struct {
	Limits []QuotaLimit `json:"limits,omitempty"`
}

// IsLimited reports whether the spec carries at least one limit.
func (q *QuotaSpec) IsLimited() bool {
	return q != nil && len(q.Limits) > 0
}

// LimitedPairs returns dimension → scope → limit triples for enforcement.
func (q *QuotaSpec) LimitedPairs() map[QuotaDimension]map[QuotaScope]int64 {
	if q == nil {
		return nil
	}

	pairs := make(map[QuotaDimension]map[QuotaScope]int64, len(q.Limits))
	for _, limit := range q.Limits {
		if _, ok := pairs[limit.Dimension]; !ok {
			pairs[limit.Dimension] = map[QuotaScope]int64{}
		}
		pairs[limit.Dimension][limit.Scope] = limit.Limit
	}
	return pairs
}

// DeepCopy returns an independent copy of the spec.
func (q *QuotaSpec) DeepCopy() *QuotaSpec {
	if q == nil {
		return nil
	}

	out := &QuotaSpec{Limits: make([]QuotaLimit, len(q.Limits))}
	copy(out.Limits, q.Limits)
	return out
}

// NormalizeQuotaSpec validates and normalizes a quota spec. A nil or empty spec
// is treated as unlimited and returns (nil, nil).
func NormalizeQuotaSpec(spec *QuotaSpec) (*QuotaSpec, error) {
	if spec == nil || len(spec.Limits) == 0 {
		return nil, nil
	}

	normalized := &QuotaSpec{Limits: make([]QuotaLimit, 0, len(spec.Limits))}
	seen := make(map[string]struct{}, len(spec.Limits))
	for _, limit := range spec.Limits {
		if err := validateQuotaDimension(limit.Dimension); err != nil {
			return nil, err
		}
		if err := validateQuotaScope(limit.Scope); err != nil {
			return nil, err
		}
		if limit.Limit < 0 {
			return nil, ErrQuotaLimitNegative
		}

		key := string(limit.Dimension) + "\x00" + string(limit.Scope)
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("duplicate quota limit for dimension %q and scope %q", limit.Dimension, limit.Scope)
		}
		seen[key] = struct{}{}
		normalized.Limits = append(normalized.Limits, limit)
	}

	if len(normalized.Limits) == 0 {
		return nil, nil
	}
	return normalized, nil
}

// DecodeQuotaSpec deserializes a stored quota JSON blob and normalizes it.
func DecodeQuotaSpec(raw []byte) (*QuotaSpec, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}

	var spec QuotaSpec
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return nil, fmt.Errorf("%w: unmarshal quota: %v", ErrInvalidQuotaSpec, err)
	}
	normalized, err := NormalizeQuotaSpec(&spec)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidQuotaSpec, err)
	}
	return normalized, nil
}

// MarshalQuotaSpec normalizes and serializes a quota spec for storage.
func MarshalQuotaSpec(spec *QuotaSpec) ([]byte, error) {
	normalized, err := NormalizeQuotaSpec(spec)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidQuotaSpec, err)
	}
	if normalized == nil {
		return nil, nil
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("marshal quota: %w", err)
	}
	return raw, nil
}

var AllowedQuotaDimensions = map[QuotaDimension]struct{}{
	DimSandboxCount: {},
	DimLimitsCPU:    {},
	DimLimitsMemory: {},
}

var AllowedQuotaScopes = map[QuotaScope]struct{}{
	ScopeAll:     {},
	ScopeRunning: {},
}

func validateQuotaDimension(dimension QuotaDimension) error {
	if _, ok := AllowedQuotaDimensions[dimension]; ok {
		return nil
	}
	return fmt.Errorf("unsupported quota dimension %q", dimension)
}

func validateQuotaScope(scope QuotaScope) error {
	if _, ok := AllowedQuotaScopes[scope]; ok {
		return nil
	}
	return fmt.Errorf("unsupported quota scope %q", scope)
}
