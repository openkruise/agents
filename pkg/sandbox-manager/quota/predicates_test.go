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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"

	"github.com/stretchr/testify/assert"
)

func TestScopePredicates(t *testing.T) {
	sbx := func(phase agentsv1alpha1.SandboxPhase, paused, deleting bool) *agentsv1alpha1.Sandbox {
		s := &agentsv1alpha1.Sandbox{}
		s.Status.Phase = phase
		s.Spec.Paused = paused
		if deleting {
			now := metav1.Now()
			s.DeletionTimestamp = &now
		}
		return s
	}

	tests := []struct {
		name        string
		sbx         *agentsv1alpha1.Sandbox
		wantLive    bool
		wantRunning bool
		wantScopes  []QuotaScope
	}{
		{
			name:        "running not paused",
			sbx:         sbx(agentsv1alpha1.SandboxRunning, false, false),
			wantLive:    true,
			wantRunning: true,
			wantScopes:  []QuotaScope{ScopeRunning},
		},
		{
			name:        "running paused",
			sbx:         sbx(agentsv1alpha1.SandboxPaused, true, false),
			wantLive:    true,
			wantRunning: false,
			wantScopes:  []QuotaScope{},
		},
		{
			name:        "pending live and running",
			sbx:         sbx(agentsv1alpha1.SandboxPending, false, false),
			wantLive:    true,
			wantRunning: true,
			wantScopes:  []QuotaScope{ScopeRunning},
		},
		{
			name:        "failed still live",
			sbx:         sbx(agentsv1alpha1.SandboxFailed, false, false),
			wantLive:    true,
			wantRunning: true,
			wantScopes:  []QuotaScope{ScopeRunning},
		},
		{
			name:        "terminating freed",
			sbx:         sbx(agentsv1alpha1.SandboxTerminating, false, false),
			wantLive:    false,
			wantRunning: false,
			wantScopes:  []QuotaScope{},
		},
		{
			name:        "deletion requested freed",
			sbx:         sbx(agentsv1alpha1.SandboxRunning, false, true),
			wantLive:    false,
			wantRunning: false,
			wantScopes:  []QuotaScope{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantLive, IsLiveForQuota(tt.sbx))
			assert.Equal(t, tt.wantRunning, InRunningScope(tt.sbx))
			assert.Equal(t, tt.wantScopes, ConditionalScopesOf(tt.sbx))
		})
	}
}

func TestFootprintOf(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("500m"),
										corev1.ResourceMemory: resource.MustParse("1Gi"),
									},
									Limits: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("2000m"),
										corev1.ResourceMemory: resource.MustParse("4Gi"),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	assert.Equal(t, map[QuotaDimension]int64{
		DimLimitsCPU:    2000,
		DimLimitsMemory: 4096,
	}, FootprintOf(sbx))
}

func TestFootprintOfRoundsMemoryUpToMiB(t *testing.T) {
	sbx := runningSandbox(time.Now(), "owner", "lock", time.Hour, 0, 0, false)
	sbx.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = *resource.NewQuantity(1024*1024+1, resource.BinarySI)

	assert.Equal(t, int64(2), FootprintOf(sbx)[DimLimitsMemory])
}

func TestFootprintFromResourceUsesLimits(t *testing.T) {
	tests := []struct {
		name string
		in   infra.SandboxResource
		want map[QuotaDimension]int64
	}{
		{
			name: "uses limits not requests",
			in: infra.SandboxResource{
				Requests: infra.ResourceList{CPUMilli: 500, MemoryMB: 512},
				Limits:   infra.ResourceList{CPUMilli: 1500, MemoryMB: 1537},
			},
			want: map[QuotaDimension]int64{
				DimLimitsCPU:    1500,
				DimLimitsMemory: 1537,
			},
		},
		{
			name: "missing limits produce zeros",
			in:   infra.SandboxResource{},
			want: map[QuotaDimension]int64{
				DimLimitsCPU:    0,
				DimLimitsMemory: 0,
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
	assert.Equal(t, int64(2), got[DimLimitsMemory])
}
