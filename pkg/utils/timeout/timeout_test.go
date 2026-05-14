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

package timeout

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestGetTimeoutFromSandbox(t *testing.T) {
	baseTime := time.Now()

	tests := []struct {
		name     string
		sandbox  *agentsv1alpha1.Sandbox
		expected Options
	}{
		{
			name: "No timeout configured",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{},
			},
			expected: Options{},
		},
		{
			name: "Has shutdown timeout",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					ShutdownTime: &metav1.Time{Time: baseTime.Add(time.Minute)},
				},
			},
			expected: Options{
				ShutdownTime: NormalizeTime(baseTime.Add(time.Minute)),
			},
		},
		{
			name: "Has pause timeout",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					PauseTime: &metav1.Time{Time: baseTime.Add(2 * time.Minute)},
				},
			},
			expected: Options{
				PauseTime: NormalizeTime(baseTime.Add(2 * time.Minute)),
			},
		},
		{
			name: "Has both timeouts",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					ShutdownTime: &metav1.Time{Time: baseTime.Add(3 * time.Minute)},
					PauseTime:    &metav1.Time{Time: baseTime.Add(4 * time.Minute)},
				},
			},
			expected: Options{
				ShutdownTime: NormalizeTime(baseTime.Add(3 * time.Minute)),
				PauseTime:    NormalizeTime(baseTime.Add(4 * time.Minute)),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetTimeoutFromSandbox(tt.sandbox)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestEqual(t *testing.T) {
	base := time.Now()
	timeWithNanos := base.Add(1500 * time.Millisecond)
	rounded := timeWithNanos.Round(0).Truncate(time.Second)

	tests := []struct {
		name string
		a    Options
		b    Options
		want bool
	}{
		{
			name: "Both zero",
			a:    Options{},
			b:    Options{},
			want: true,
		},
		{
			name: "Shutdown times same after normalization",
			a: Options{
				ShutdownTime: timeWithNanos,
			},
			b: Options{
				ShutdownTime: rounded.Add(500 * time.Millisecond),
			},
			want: true,
		},
		{
			name: "Pause time mismatched",
			a: Options{
				PauseTime: base.Add(time.Minute),
			},
			b: Options{
				PauseTime: base.Add(2 * time.Minute),
			},
			want: false,
		},
		{
			name: "Shutdown and pause mismatch",
			a: Options{
				ShutdownTime: base.Add(time.Minute),
			},
			b: Options{
				ShutdownTime: base.Add(time.Minute),
				PauseTime:    base.Add(2 * time.Minute),
			},
			want: false,
		},
		{
			name: "Baseline ignored",
			a: Options{
				ShutdownTime: base.Add(time.Minute),
				PauseTime:    base.Add(2 * time.Minute),
				Baseline: &Options{
					ShutdownTime: base.Add(3 * time.Minute),
				},
			},
			b: Options{
				ShutdownTime: base.Add(time.Minute),
				PauseTime:    base.Add(2 * time.Minute),
				Baseline: &Options{
					PauseTime: base.Add(4 * time.Minute),
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Equal(tt.a, tt.b)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestShouldExtendTimeout(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name      string
		current   Options
		requested Options
		want      bool
	}{
		{
			name: "Current timeout zero, cannot extend",
			current: Options{
				ShutdownTime: time.Time{},
			},
			requested: Options{
				ShutdownTime: now.Add(time.Minute),
			},
			want: false,
		},
		{
			name: "Requested timeout zero, cannot extend",
			current: Options{
				ShutdownTime: now.Add(time.Minute),
			},
			requested: Options{},
			want:      false,
		},
		{
			name: "Requested later than current",
			current: Options{
				ShutdownTime: now.Add(time.Minute),
			},
			requested: Options{
				ShutdownTime: now.Add(2 * time.Minute),
			},
			want: true,
		},
		{
			name: "Requested earlier than current",
			current: Options{
				ShutdownTime: now.Add(2 * time.Minute),
			},
			requested: Options{
				ShutdownTime: now.Add(time.Minute),
			},
			want: false,
		},
		{
			name: "Pause time has priority",
			current: Options{
				ShutdownTime: now.Add(10 * time.Minute),
				PauseTime:    now.Add(time.Minute),
			},
			requested: Options{
				ShutdownTime: now.Add(20 * time.Minute),
				PauseTime:    now.Add(2 * time.Minute),
			},
			want: true,
		},
		{
			name: "Same effective end time with different baselines does not extend",
			current: Options{
				ShutdownTime: now.Add(time.Minute),
				Baseline: &Options{
					ShutdownTime: now.Add(3 * time.Minute),
				},
			},
			requested: Options{
				ShutdownTime: now.Add(time.Minute),
				Baseline: &Options{
					ShutdownTime: now.Add(4 * time.Minute),
				},
			},
			want: false,
		},
		{
			name: "Later effective end time extends despite different baselines",
			current: Options{
				ShutdownTime: now.Add(time.Minute),
				Baseline: &Options{
					PauseTime: now.Add(3 * time.Minute),
				},
			},
			requested: Options{
				ShutdownTime: now.Add(2 * time.Minute),
				Baseline: &Options{
					PauseTime: now.Add(4 * time.Minute),
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldExtendTimeout(tt.current, tt.requested)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNormalizeTime(t *testing.T) {
	tests := []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{
			name: "Zero time",
			in:   time.Time{},
			want: time.Time{},
		},
		{
			name: "Truncate to whole second",
			in:   time.Date(2026, time.January, 2, 3, 4, 5, 123456789, time.UTC),
			want: time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeTime(tt.in)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTimeoutEndAt(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name string
		in   Options
		want time.Time
	}{
		{
			name: "Pause time takes precedence",
			in: Options{
				ShutdownTime: now.Add(time.Minute),
				PauseTime:    now.Add(2 * time.Minute),
			},
			want: now.Add(2 * time.Minute),
		},
		{
			name: "Fallback to shutdown time",
			in: Options{
				ShutdownTime: now.Add(time.Minute),
			},
			want: now.Add(time.Minute),
		},
		{
			name: "Both zero",
			in:   Options{},
			want: time.Time{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := timeoutEndAt(tt.in)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTimeEqual(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name string
		a    time.Time
		b    time.Time
		want bool
	}{
		{
			name: "Both zero",
			a:    time.Time{},
			b:    time.Time{},
			want: true,
		},
		{
			name: "One zero",
			a:    time.Time{},
			b:    now,
			want: false,
		},
		{
			name: "Close but normalized same second",
			a:    time.Date(2026, time.January, 2, 3, 4, 5, 900_000_000, time.UTC),
			b:    time.Date(2026, time.January, 2, 3, 4, 6, 100_000_000, time.UTC),
			want: false,
		},
		{
			name: "Exact same wall time with different monotonic",
			a:    now,
			b:    now.Add(0),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := timeEqual(tt.a, tt.b)
			assert.Equal(t, tt.want, got)
		})
	}
}
