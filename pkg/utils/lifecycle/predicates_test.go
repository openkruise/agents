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

package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestIsNotTerminating(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name string
		sbx  *agentsv1alpha1.Sandbox
		want bool
	}{
		{name: "nil sandbox is not active", sbx: nil, want: false},
		{name: "running sandbox is active", sbx: &agentsv1alpha1.Sandbox{Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning}}, want: true},
		{name: "failed sandbox still counts until deletion", sbx: &agentsv1alpha1.Sandbox{Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxFailed}}, want: true},
		{name: "succeeded sandbox still counts until deletion", sbx: &agentsv1alpha1.Sandbox{Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxSucceeded}}, want: true},
		{name: "deletion timestamp is not active", sbx: &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now}}, want: false},
		{name: "terminating phase is not active", sbx: &agentsv1alpha1.Sandbox{Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxTerminating}}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsNotTerminating(tt.sbx))
		})
	}
}

func TestIsLiveForQuota(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name string
		sbx  *agentsv1alpha1.Sandbox
		want bool
	}{
		{name: "nil sandbox is not live", sbx: nil, want: false},
		{name: "running sandbox is live", sbx: &agentsv1alpha1.Sandbox{Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning}}, want: true},
		{name: "deletion timestamp is not live", sbx: &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now}}, want: false},
		{name: "terminating phase is not live", sbx: &agentsv1alpha1.Sandbox{Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxTerminating}}, want: false},
		{name: "reusing phase is not live", sbx: &agentsv1alpha1.Sandbox{Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRecycling}}, want: false},
		{
			name: "reuse trigger on reusable sandbox is not live",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
					agentsv1alpha1.AnnotationCleanup:        "true",
					agentsv1alpha1.AnnotationCleanupEnabled: "true",
				}},
				Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning},
			},
			want: false,
		},
		{
			name: "reuse trigger without reusable marker is live",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{agentsv1alpha1.AnnotationCleanup: "true"}},
				Status:     agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsLiveForQuota(tt.sbx))
		})
	}
}
