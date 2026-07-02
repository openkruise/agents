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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	quotaspec "github.com/openkruise/agents/pkg/sandbox-manager/quota/spec"

	"github.com/stretchr/testify/assert"
)

func TestFootprintFromResourceUsesLimits(t *testing.T) {
	tests := []struct {
		name string
		in   infra.SandboxResource
		want map[quotaspec.QuotaDimension]int64
	}{
		{
			name: "uses limits not requests",
			in: infra.SandboxResource{
				Requests: infra.ResourceList{CPUMilli: 500, MemoryMB: 512},
				Limits:   infra.ResourceList{CPUMilli: 1500, MemoryMB: 1537},
			},
			want: map[quotaspec.QuotaDimension]int64{
				quotaspec.DimLimitsCPU:    1500,
				quotaspec.DimLimitsMemory: 1537,
			},
		},
		{
			name: "missing limits produce zeros",
			in:   infra.SandboxResource{},
			want: map[quotaspec.QuotaDimension]int64{
				quotaspec.DimLimitsCPU:    0,
				quotaspec.DimLimitsMemory: 0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, FootprintFromResource(tt.in))
		})
	}
}

func TestFootprintFromCalculatedResourceRoundsLimitMemoryUp(t *testing.T) {
	res := infra.CalculateResourceFromContainers([]corev1.Container{{
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: *resource.NewQuantity(1024*1024+1, resource.BinarySI),
			},
		},
	}})

	got := FootprintFromResource(res)
	assert.Equal(t, int64(2), got[quotaspec.DimLimitsMemory])
}
