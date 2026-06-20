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
)

type QuotaDimension string

const DimSandboxCount QuotaDimension = "sandbox.count"

type QuotaScope struct {
	Template string `json:"template,omitempty"`
}

type QuotaLimit struct {
	Dimension QuotaDimension `json:"dimension"`
	Scope     QuotaScope     `json:"scope,omitempty"`
	Limit     *int64         `json:"limit,omitempty"`
}

type QuotaSpec struct {
	Limits []QuotaLimit `json:"limits,omitempty"`
}

type APIKeyQuota struct {
	Sandbox *SandboxQuota `json:"sandbox,omitempty"`
}

type SandboxQuota struct {
	Count *int64 `json:"count,omitempty"`
}

func (q *APIKeyQuota) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	for key := range raw {
		if key != "sandbox" {
			return fmt.Errorf("unsupported quota field %q", key)
		}
	}

	if rawSandbox, ok := raw["sandbox"]; ok {
		if string(rawSandbox) == "null" {
			return nil
		}

		var sandbox SandboxQuota
		if err := json.Unmarshal(rawSandbox, &sandbox); err != nil {
			return err
		}
		q.Sandbox = &sandbox
	}

	return nil
}

func (q *SandboxQuota) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	for key := range raw {
		if key != "count" {
			return fmt.Errorf("unsupported quota field %q", "sandbox."+key)
		}
	}

	if rawCount, ok := raw["count"]; ok {
		if string(rawCount) == "null" {
			return nil
		}

		var count int64
		if err := json.Unmarshal(rawCount, &count); err != nil {
			return err
		}
		q.Count = &count
	}

	return nil
}

func (q *APIKeyQuota) ToQuotaSpec() (*QuotaSpec, error) {
	if q == nil || q.Sandbox == nil || q.Sandbox.Count == nil {
		return nil, nil
	}

	spec := &QuotaSpec{
		Limits: []QuotaLimit{{
			Dimension: DimSandboxCount,
			Limit:     q.Sandbox.Count,
		}},
	}

	normalized, err := NormalizeQuotaSpec(spec)
	if err != nil {
		if err.Error() == "limit must be non-negative" {
			return nil, fmt.Errorf("quota limit must be non-negative: %w", err)
		}
		return nil, err
	}

	return normalized, nil
}

// APIKeyQuotaFromSpec converts an already-normalized internal quota spec to the public wire model.
// Callers that load untrusted storage must call NormalizeQuotaSpec first.
func APIKeyQuotaFromSpec(spec *QuotaSpec) *APIKeyQuota {
	if spec == nil {
		return nil
	}

	if count, ok := spec.SandboxCountLimit(); ok {
		return &APIKeyQuota{
			Sandbox: &SandboxQuota{Count: &count},
		}
	}

	return nil
}

func NormalizeQuotaSpec(spec *QuotaSpec) (*QuotaSpec, error) {
	if spec == nil || len(spec.Limits) == 0 {
		return nil, nil
	}

	normalized := &QuotaSpec{
		Limits: make([]QuotaLimit, 0, len(spec.Limits)),
	}
	seen := map[string]struct{}{}

	for _, limit := range spec.Limits {
		if limit.Scope.Template != "" {
			return nil, fmt.Errorf("quota scope is not supported")
		}
		if limit.Dimension != DimSandboxCount {
			return nil, fmt.Errorf("unsupported quota dimension %q", limit.Dimension)
		}

		key := string(limit.Dimension) + "|" + limit.Scope.Template
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("duplicate quota limit for dimension %q", limit.Dimension)
		}
		seen[key] = struct{}{}

		if limit.Limit == nil {
			continue
		}
		if *limit.Limit < 0 {
			return nil, fmt.Errorf("limit must be non-negative")
		}
		value := *limit.Limit
		normalized.Limits = append(normalized.Limits, QuotaLimit{
			Dimension: limit.Dimension,
			Scope:     limit.Scope,
			Limit:     &value,
		})
	}

	if len(normalized.Limits) == 0 {
		return nil, nil
	}

	return normalized, nil
}

func (q *QuotaSpec) SandboxCountLimit() (int64, bool) {
	if q == nil {
		return 0, false
	}

	for _, limit := range q.Limits {
		if limit.Dimension != DimSandboxCount || limit.Scope.Template != "" || limit.Limit == nil {
			continue
		}
		return *limit.Limit, true
	}

	return 0, false
}

func (q *QuotaSpec) IsLimited() bool {
	_, ok := q.SandboxCountLimit()
	return ok
}

func (q *QuotaSpec) DeepCopy() *QuotaSpec {
	if q == nil {
		return nil
	}

	out := &QuotaSpec{
		Limits: make([]QuotaLimit, len(q.Limits)),
	}
	for i := range q.Limits {
		out.Limits[i] = q.Limits[i]
		if q.Limits[i].Limit != nil {
			value := *q.Limits[i].Limit
			out.Limits[i].Limit = &value
		}
	}

	return out
}

func (q *APIKeyQuota) deepCopy() *APIKeyQuota {
	if q == nil {
		return nil
	}

	out := &APIKeyQuota{}
	if q.Sandbox != nil {
		out.Sandbox = &SandboxQuota{}
		if q.Sandbox.Count != nil {
			value := *q.Sandbox.Count
			out.Sandbox.Count = &value
		}
	}

	return out
}
