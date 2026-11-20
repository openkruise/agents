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

package claim

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"

	"github.com/openkruise/agents/api/v1alpha1"
)

func TestSetMemoryPersistence(t *testing.T) {
	tests := []struct {
		name     string
		contents []string
		enabled  *bool
		want     []string
	}{
		{
			name:     "nil enabled leaves contents untouched",
			contents: []string{v1alpha1.PersistentContentFilesystem},
			enabled:  nil,
			want:     []string{v1alpha1.PersistentContentFilesystem},
		},
		{
			name:     "enable on empty adds memory",
			contents: nil,
			enabled:  ptr.To(true),
			want:     []string{v1alpha1.PersistentContentMemory},
		},
		{
			name:     "enable keeps existing memory without duplicating",
			contents: []string{v1alpha1.PersistentContentMemory},
			enabled:  ptr.To(true),
			want:     []string{v1alpha1.PersistentContentMemory},
		},
		{
			name:     "enable collapses duplicate memory entries",
			contents: []string{v1alpha1.PersistentContentMemory, v1alpha1.PersistentContentMemory},
			enabled:  ptr.To(true),
			want:     []string{v1alpha1.PersistentContentMemory},
		},
		{
			name:     "enable preserves other contents and adds memory",
			contents: []string{v1alpha1.PersistentContentFilesystem, v1alpha1.PersistentContentIp},
			enabled:  ptr.To(true),
			want:     []string{v1alpha1.PersistentContentFilesystem, v1alpha1.PersistentContentIp, v1alpha1.PersistentContentMemory},
		},
		{
			name:     "disable removes memory and keeps other contents",
			contents: []string{v1alpha1.PersistentContentMemory, v1alpha1.PersistentContentFilesystem},
			enabled:  ptr.To(false),
			want:     []string{v1alpha1.PersistentContentFilesystem},
		},
		{
			name:     "disable removes every memory entry",
			contents: []string{v1alpha1.PersistentContentMemory, v1alpha1.PersistentContentMemory, v1alpha1.PersistentContentIp},
			enabled:  ptr.To(false),
			want:     []string{v1alpha1.PersistentContentIp},
		},
		{
			name:     "disable without memory is a no-op on contents",
			contents: []string{v1alpha1.PersistentContentFilesystem},
			enabled:  ptr.To(false),
			want:     []string{v1alpha1.PersistentContentFilesystem},
		},
		{
			name:     "disable on empty stays empty",
			contents: nil,
			enabled:  ptr.To(false),
			want:     []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &v1alpha1.Sandbox{}
			sbx.Spec.PersistentContents = tt.contents
			SetMemoryPersistence(sbx, tt.enabled)
			assert.ElementsMatch(t, tt.want, sbx.Spec.PersistentContents)
		})
	}
}

func TestEnableWakeOnIngressTraffic(t *testing.T) {
	tests := []struct {
		name         string
		initial      *v1alpha1.AutoPausePolicy
		pauseTimeout time.Duration
		wantTimeout  *time.Duration
	}{
		{
			name:         "nil policy with positive timeout arms rule with pause timeout",
			initial:      nil,
			pauseTimeout: 10 * time.Minute,
			wantTimeout:  ptr.To(10 * time.Minute),
		},
		{
			name:         "nil policy with zero timeout arms rule without pause timeout",
			initial:      nil,
			pauseTimeout: 0,
			wantTimeout:  nil,
		},
		{
			name:         "nil policy with negative timeout arms rule without pause timeout",
			initial:      nil,
			pauseTimeout: -time.Minute,
			wantTimeout:  nil,
		},
		{
			name: "existing pause policy is preserved",
			initial: &v1alpha1.AutoPausePolicy{
				Pause: &v1alpha1.PausePolicy{WhenProbedIdleState: &v1alpha1.ProbedIdleStateRule{Probe: "p", MessageRegex: "idle"}},
			},
			pauseTimeout: 5 * time.Minute,
			wantTimeout:  ptr.To(5 * time.Minute),
		},
		{
			name: "existing sibling resume rule is preserved",
			initial: &v1alpha1.AutoPausePolicy{
				Resume: &v1alpha1.ResumePolicy{WhenProbedScheduleTime: &v1alpha1.ProbedScheduleTimeRule{}},
			},
			pauseTimeout: 0,
			wantTimeout:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &v1alpha1.Sandbox{}
			sbx.Spec.AutoPausePolicy = tt.initial
			EnableWakeOnIngressTraffic(sbx, tt.pauseTimeout)

			policy := sbx.Spec.AutoPausePolicy
			assert.NotNil(t, policy)
			assert.NotNil(t, policy.Resume)
			rule := policy.Resume.OnIngressTraffic
			assert.NotNil(t, rule)
			if tt.wantTimeout == nil {
				assert.Nil(t, rule.PauseTimeout)
			} else {
				assert.NotNil(t, rule.PauseTimeout)
				assert.Equal(t, *tt.wantTimeout, rule.PauseTimeout.Duration)
			}
			if tt.initial != nil {
				assert.Equal(t, tt.initial.Pause, policy.Pause, "pause policy must be preserved")
				if tt.initial.Resume != nil {
					assert.Equal(t, tt.initial.Resume.WhenProbedScheduleTime, policy.Resume.WhenProbedScheduleTime,
						"sibling resume rule must be preserved")
				}
			}
		})
	}
}

func TestClearWakeOnIngressTraffic(t *testing.T) {
	idlePause := &v1alpha1.PausePolicy{WhenProbedIdleState: &v1alpha1.ProbedIdleStateRule{Probe: "p", MessageRegex: "idle"}}
	scheduleResume := &v1alpha1.ResumePolicy{WhenProbedScheduleTime: &v1alpha1.ProbedScheduleTimeRule{}}

	tests := []struct {
		name       string
		initial    *v1alpha1.AutoPausePolicy
		wantPolicy *v1alpha1.AutoPausePolicy
	}{
		{
			name:       "nil policy stays nil",
			initial:    nil,
			wantPolicy: nil,
		},
		{
			name:       "policy without resume stays unchanged",
			initial:    &v1alpha1.AutoPausePolicy{Pause: idlePause},
			wantPolicy: &v1alpha1.AutoPausePolicy{Pause: idlePause},
		},
		{
			name:       "ingress-only resume is pruned back to nil policy",
			initial:    &v1alpha1.AutoPausePolicy{Resume: &v1alpha1.ResumePolicy{OnIngressTraffic: &v1alpha1.IngressTrafficRule{}}},
			wantPolicy: nil,
		},
		{
			name:       "ingress rule removed but pause policy preserved",
			initial:    &v1alpha1.AutoPausePolicy{Pause: idlePause, Resume: &v1alpha1.ResumePolicy{OnIngressTraffic: &v1alpha1.IngressTrafficRule{}}},
			wantPolicy: &v1alpha1.AutoPausePolicy{Pause: idlePause},
		},
		{
			name:       "ingress rule removed but sibling resume rule preserved",
			initial:    &v1alpha1.AutoPausePolicy{Resume: &v1alpha1.ResumePolicy{WhenProbedScheduleTime: scheduleResume.WhenProbedScheduleTime, OnIngressTraffic: &v1alpha1.IngressTrafficRule{}}},
			wantPolicy: &v1alpha1.AutoPausePolicy{Resume: scheduleResume},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &v1alpha1.Sandbox{}
			sbx.Spec.AutoPausePolicy = tt.initial
			ClearWakeOnIngressTraffic(sbx)
			assert.Equal(t, tt.wantPolicy, sbx.Spec.AutoPausePolicy)
		})
	}
}

func TestWakeOnIngressTrafficRoundTripPrunesToFreshSpec(t *testing.T) {
	fresh := &v1alpha1.Sandbox{}
	sbx := &v1alpha1.Sandbox{}
	EnableWakeOnIngressTraffic(sbx, 10*time.Minute)
	assert.NotNil(t, sbx.Spec.AutoPausePolicy, "arming must create the policy")
	ClearWakeOnIngressTraffic(sbx)
	assert.Equal(t, fresh.Spec.AutoPausePolicy, sbx.Spec.AutoPausePolicy,
		"arm then clear must leave a pooled spec byte-identical to a fresh one")
}
