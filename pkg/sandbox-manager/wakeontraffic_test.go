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

package sandbox_manager

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestParseWakeOnTrafficPolicy(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expect      Policy
		expectError error
	}{
		{
			name:        "disabled with nil annotations",
			annotations: nil,
			expectError: ErrAutoResumeDisabled,
		},
		{
			name:        "disabled with empty annotations",
			annotations: map[string]string{},
			expectError: ErrAutoResumeDisabled,
		},
		{
			name: "accepts never policy",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:never",
			},
			expect: Policy{Form: PolicyFormNever},
		},
		{
			name: "accepts one second duration",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:1s",
			},
			expect: Policy{Form: PolicyFormDuration, Duration: time.Second},
		},
		{
			name: "accepts thirty second duration",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:30s",
			},
			expect: Policy{Form: PolicyFormDuration, Duration: 30 * time.Second},
		},
		{
			name: "accepts minute duration",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:5m",
			},
			expect: Policy{Form: PolicyFormDuration, Duration: 5 * time.Minute},
		},
		{
			name: "accepts compound hour minute duration",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:2h30m",
			},
			expect: Policy{Form: PolicyFormDuration, Duration: 2*time.Hour + 30*time.Minute},
		},
		{
			name: "rejects rfc3339 timestamp payload",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:2099-01-01T00:00:00Z",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects invalid kind",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "foo:300s",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects missing payload separator",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects missing kind",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: ":300s",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects empty value",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects true",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "true",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects false",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "false",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects empty timeout payload",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects zero without unit",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:0",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects zero duration",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:0s",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects negative duration",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:-1s",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects subsecond duration",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:500ms",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects invalid duration text",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:abc",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects duration without unit",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:5",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects capitalized never",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:Never",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects uppercase never",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:NEVER",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects leading space before never",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: " timeout:never",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects trailing space after never",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:never ",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects trailing newline after never",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:never\n",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects leading space before duration",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout: 300s",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects trailing newline",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:300s\n",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
		{
			name: "rejects just under one second",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:999ms",
			},
			expectError: ErrInvalidAutoResumePolicy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseWakeOnTrafficPolicy(tt.annotations)
			if tt.expectError != nil {
				require.Error(t, err)
				assert.True(t, errors.Is(err, tt.expectError))
				assert.Equal(t, Policy{}, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expect, got)
		})
	}
}
