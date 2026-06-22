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

package pausedretention

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

func TestParseReservePausedSandboxFor(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		want        time.Duration
		expectError string
	}{
		{name: "default", raw: timeout.ReservePausedSandboxForDefaultValue, want: timeout.DefaultReservePausedSandboxFor},
		{name: "positive duration", raw: "240h", want: 240 * time.Hour},
		{name: "empty explicit value", raw: "", expectError: "use \"default\""},
		{name: "zero without unit", raw: "0", expectError: "use \"default\""},
		{name: "zero duration", raw: "0s", expectError: "use \"default\""},
		{name: "negative duration", raw: "-1h", expectError: "use \"default\""},
		{name: "never rejected", raw: "never", expectError: "use \"default\""},
		{name: "forever rejected", raw: "forever", expectError: "use \"default\""},
		{name: "invalid duration", raw: "abc", expectError: "use \"default\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseReservePausedSandboxFor(tt.raw)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveReservePausedSandboxForAnnotation(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        time.Duration
		wantManaged bool
		expectError string
	}{
		{name: "absent", annotations: nil, wantManaged: false},
		{
			name:        "default",
			annotations: map[string]string{agentsv1alpha1.AnnotationReservePausedSandboxFor: timeout.ReservePausedSandboxForDefaultValue},
			want:        timeout.DefaultReservePausedSandboxFor,
			wantManaged: true,
		},
		{
			name:        "custom duration",
			annotations: map[string]string{agentsv1alpha1.AnnotationReservePausedSandboxFor: "30m"},
			want:        30 * time.Minute,
			wantManaged: true,
		},
		{
			name:        "invalid persisted annotation",
			annotations: map[string]string{agentsv1alpha1.AnnotationReservePausedSandboxFor: "forever"},
			wantManaged: true,
			expectError: "use \"default\"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, managed, err := ResolveReservePausedSandboxForAnnotation(tt.annotations)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Equal(t, tt.wantManaged, managed)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantManaged, managed)
		})
	}
}

func TestPausedShutdownTime(t *testing.T) {
	tests := []struct {
		name            string
		anchor          time.Time
		pausedRetention time.Duration
		want            time.Time
	}{
		{
			name:            "adds paused retention",
			anchor:          time.Date(2026, time.June, 11, 10, 0, 0, 123, time.UTC),
			pausedRetention: 30 * time.Minute,
			want:            time.Date(2026, time.June, 11, 10, 30, 0, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PausedShutdownTime(tt.anchor, tt.pausedRetention)
			assert.Equal(t, tt.want, got)
		})
	}
}
