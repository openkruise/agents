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

package quota

import (
	"sort"

	quotaspec "github.com/openkruise/agents/pkg/sandbox-manager/quota/spec"
)

func normalizeFootprint(in map[quotaspec.QuotaDimension]int64) map[quotaspec.QuotaDimension]int64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[quotaspec.QuotaDimension]int64, len(in))
	for dim, amount := range in {
		if _, ok := quotaspec.AllowedQuotaDimensions[dim]; !ok || dim == quotaspec.DimSandboxCount {
			continue
		}
		if amount == 0 {
			continue
		}
		out[dim] = amount
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeScopes(in []quotaspec.QuotaScope) []quotaspec.QuotaScope {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[quotaspec.QuotaScope]struct{}, len(in))
	out := make([]quotaspec.QuotaScope, 0, len(in))
	for _, scope := range in {
		if _, ok := quotaspec.AllowedQuotaScopes[scope]; !ok || scope == quotaspec.ScopeAll {
			continue
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		out = append(out, scope)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i] < out[j]
	})
	return out
}
