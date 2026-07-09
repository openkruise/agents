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

func TestParseReservePausedSandboxDuration(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		want        time.Duration
		expectError string
	}{
		{name: "forever", raw: timeout.ReservePausedSandboxDurationForeverValue, want: timeout.ForeverReservePausedSandboxDuration},
		{name: "positive duration", raw: "240h", want: 240 * time.Hour},
		{name: "empty explicit value", raw: "", expectError: "use \"forever\""},
		{name: "zero without unit", raw: "0", expectError: "use \"forever\""},
		{name: "zero duration", raw: "0s", expectError: "use \"forever\""},
		{name: "negative duration", raw: "-1h", expectError: "use \"forever\""},
		{name: "never rejected", raw: "never", expectError: "use \"forever\""},
		{name: "invalid duration", raw: "abc", expectError: "use \"forever\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseReservePausedSandboxDuration(tt.raw)
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

func TestResolveReservePausedSandboxDurationAnnotation(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        time.Duration
		wantManaged bool
		expectError string
	}{
		{name: "absent", annotations: nil, wantManaged: false},
		{
			name:        "forever",
			annotations: map[string]string{agentsv1alpha1.AnnotationReservePausedSandboxDuration: timeout.ReservePausedSandboxDurationForeverValue},
			want:        timeout.ForeverReservePausedSandboxDuration,
			wantManaged: true,
		},
		{
			name:        "custom duration",
			annotations: map[string]string{agentsv1alpha1.AnnotationReservePausedSandboxDuration: "30m"},
			want:        30 * time.Minute,
			wantManaged: true,
		},
		{
			name:        "invalid persisted annotation",
			annotations: map[string]string{agentsv1alpha1.AnnotationReservePausedSandboxDuration: "invalid"},
			wantManaged: true,
			expectError: "use \"forever\"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, managed, err := ResolveReservePausedSandboxDurationAnnotation(tt.annotations)
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
