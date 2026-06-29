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
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	quotaspec "github.com/openkruise/agents/pkg/sandbox-manager/quota/spec"
	"github.com/openkruise/agents/pkg/sandbox/lifecycle"
)

func IsLiveForQuota(sbx *agentsv1alpha1.Sandbox) bool {
	return lifecycle.IsLiveForQuota(sbx)
}

// InRunningScope uses Spec.Paused (pause request), matching current production behavior.
// Design §5 pseudocode uses Status.Phase; §15 leaves the exact transition-phase boundary to
// implementation. Do not change this predicate during the refactor.
func InRunningScope(sbx *agentsv1alpha1.Sandbox) bool {
	return lifecycle.IsLiveForQuota(sbx) && !sbx.Spec.Paused
}

func ConditionalScopesOf(sbx *agentsv1alpha1.Sandbox) []quotaspec.QuotaScope {
	if !InRunningScope(sbx) {
		return []quotaspec.QuotaScope{}
	}
	return []quotaspec.QuotaScope{quotaspec.ScopeRunning}
}

// FootprintFromResource converts an infra SandboxResource to a quota footprint.
func FootprintFromResource(resource infra.SandboxResource) map[quotaspec.QuotaDimension]int64 {
	return map[quotaspec.QuotaDimension]int64{
		quotaspec.DimLimitsCPU:    resource.Limits.CPUMilli,
		quotaspec.DimLimitsMemory: resource.Limits.MemoryMB,
	}
}

func FootprintOf(sbx *agentsv1alpha1.Sandbox) map[quotaspec.QuotaDimension]int64 {
	if sbx == nil || sbx.Spec.Template == nil {
		return FootprintFromResource(infra.SandboxResource{})
	}
	return FootprintFromResource(infra.CalculateResourceFromContainers(sbx.Spec.Template.Spec.Containers))
}
