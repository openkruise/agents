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
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

func TestGetTimeoutFromSandbox(t *testing.T) {
	baseTime := time.Now()

	tests := []struct {
		name     string
		sandbox  *agentsv1alpha1.Sandbox
		expected infra.TimeoutOptions
	}{
		{
			name: "No timeout configured",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{},
			},
			expected: infra.TimeoutOptions{},
		},
		{
			name: "Has shutdown timeout",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					ShutdownTime: &metav1.Time{Time: baseTime.Add(time.Minute)},
				},
			},
			expected: infra.TimeoutOptions{
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
			expected: infra.TimeoutOptions{
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
			expected: infra.TimeoutOptions{
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

func TestSetTimeoutSnapshot(t *testing.T) {
	tests := []struct {
		name                string
		sandbox             *agentsv1alpha1.Sandbox
		expectRawAnnotation bool
		expectKeepOtherKey  bool
	}{
		{
			name: "No timeout configured clears existing snapshot",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationPauseTimeoutSnapshot: `{"shutdownTime":"2099-01-01T00:00:00Z"}`,
						"keep": "alive",
					},
				},
			},
			expectRawAnnotation: false,
			expectKeepOtherKey:  true,
		},
		{
			name: "Set snapshot from shutdown timeout",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					ShutdownTime: &metav1.Time{Time: time.Now().Add(10*time.Second + 123*time.Millisecond)},
				},
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}},
			},
			expectRawAnnotation: true,
		},
		{
			name: "Set snapshot from pause timeout and create annotation map",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					PauseTime: &metav1.Time{Time: time.Now().Add(20 * time.Second)},
				},
				ObjectMeta: metav1.ObjectMeta{
					Annotations: nil,
				},
			},
			expectRawAnnotation: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			beforeSnapshot := tt.sandbox.Annotations[agentsv1alpha1.AnnotationPauseTimeoutSnapshot]
			err := SetTimeoutSnapshot(tt.sandbox)
			assert.NoError(t, err)

			raw, exists := tt.sandbox.Annotations[agentsv1alpha1.AnnotationPauseTimeoutSnapshot]
			assert.Equal(t, tt.expectRawAnnotation, exists)

			if tt.expectRawAnnotation {
				var got infra.TimeoutOptions
				assert.NoError(t, json.Unmarshal([]byte(raw), &got))
				assert.Equal(t, GetTimeoutFromSandbox(tt.sandbox), got)
			}

			if tt.expectKeepOtherKey {
				assert.Equal(t, "alive", tt.sandbox.Annotations["keep"])
			}

			if !tt.expectRawAnnotation {
				assert.NotEqual(t, beforeSnapshot, raw)
				assert.NotContains(t, tt.sandbox.Annotations, agentsv1alpha1.AnnotationPauseTimeoutSnapshot)
			}
		})
	}
}

func TestSetTimeoutSnapshot_MarshalError(t *testing.T) {
	originMarshal := jsonMarshalTimeoutOptions
	marshalErr := errors.New("marshal timeout options error")
	jsonMarshalTimeoutOptions = func(_ any) ([]byte, error) {
		return nil, marshalErr
	}
	t.Cleanup(func() {
		jsonMarshalTimeoutOptions = originMarshal
	})

	err := SetTimeoutSnapshot(&agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			ShutdownTime: &metav1.Time{Time: time.Now().Add(time.Minute)},
		},
	})
	assert.EqualError(t, err, marshalErr.Error())
}

func TestGetTimeoutSnapshot(t *testing.T) {
	now := time.Now()
	valid := infra.TimeoutOptions{
		ShutdownTime: NormalizeTime(now.Add(time.Minute)),
		PauseTime:    NormalizeTime(now.Add(2 * time.Minute)),
	}
	raw, err := json.Marshal(valid)
	assert.NoError(t, err)

	tests := []struct {
		name      string
		sandbox   *agentsv1alpha1.Sandbox
		expected  infra.TimeoutOptions
		exists    bool
		expectErr string
	}{
		{
			name:     "No annotations",
			sandbox:  &agentsv1alpha1.Sandbox{},
			expected: infra.TimeoutOptions{},
			exists:   false,
		},
		{
			name: "No snapshot annotation",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"foo": "bar"},
				},
			},
			expected: infra.TimeoutOptions{},
			exists:   false,
		},
		{
			name: "Empty snapshot annotation",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{agentsv1alpha1.AnnotationPauseTimeoutSnapshot: ""},
				},
			},
			expected: infra.TimeoutOptions{},
			exists:   false,
		},
		{
			name: "Valid snapshot annotation",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationPauseTimeoutSnapshot: string(raw),
					},
				},
			},
			expected: valid,
			exists:   true,
		},
		{
			name: "Malformed snapshot annotation",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{agentsv1alpha1.AnnotationPauseTimeoutSnapshot: "{bad-json"},
				},
			},
			expected:  infra.TimeoutOptions{},
			exists:    false,
			expectErr: "invalid character",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, exists, err := GetTimeoutSnapshot(tt.sandbox)
			if tt.expectErr != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectErr)
				assert.False(t, exists)
				assert.Equal(t, tt.expected, got)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.exists, exists)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestIsTimeoutMatchedSnapshot(t *testing.T) {
	base := time.Now()
	valid := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			ShutdownTime: &metav1.Time{Time: base.Add(time.Minute)},
		},
	}
	snapshot, err := json.Marshal(infra.TimeoutOptions{
		ShutdownTime: base.Add(time.Minute).Round(0).Truncate(time.Second),
	})
	assert.NoError(t, err)
	valid.ObjectMeta = metav1.ObjectMeta{
		Annotations: map[string]string{
			agentsv1alpha1.AnnotationPauseTimeoutSnapshot: string(snapshot),
		},
	}

	sbxMismatch := valid.DeepCopy()
	sbxMismatch.Spec.ShutdownTime = &metav1.Time{Time: base.Add(2 * time.Minute)}

	tests := []struct {
		name      string
		sandbox   *agentsv1alpha1.Sandbox
		expected  bool
		expectErr string
	}{
		{
			name:     "No snapshot annotation",
			sandbox:  &agentsv1alpha1.Sandbox{Spec: valid.Spec},
			expected: false,
		},
		{
			name: "Malformed snapshot annotation",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{agentsv1alpha1.AnnotationPauseTimeoutSnapshot: "{bad-json"},
				},
				Spec: valid.Spec,
			},
			expected:  false,
			expectErr: "invalid character",
		},
		{
			name:     "Snapshot matched",
			sandbox:  valid,
			expected: true,
		},
		{
			name:     "Snapshot mismatched",
			sandbox:  sbxMismatch,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matched, err := IsTimeoutMatchedSnapshot(tt.sandbox)
			if tt.expectErr != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectErr)
				assert.False(t, matched)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.expected, matched)
		})
	}
}

func TestClearPauseTimeoutSnapshot(t *testing.T) {
	tests := []struct {
		name    string
		sandbox *agentsv1alpha1.Sandbox
	}{
		{
			name: "With snapshot annotation",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationPauseTimeoutSnapshot: `{"shutdownTime":"2099-01-01T00:00:00Z"}`,
						"foo": "bar",
					},
				},
			},
		},
		{
			name:    "With nil annotations",
			sandbox: &agentsv1alpha1.Sandbox{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ClearPauseTimeoutSnapshot(tt.sandbox)
			assert.NotContains(t, tt.sandbox.Annotations, agentsv1alpha1.AnnotationPauseTimeoutSnapshot)
			if tt.sandbox.Annotations == nil {
				assert.Nil(t, tt.sandbox.Annotations)
			} else {
				assert.Equal(t, "bar", tt.sandbox.Annotations["foo"])
			}
		})
	}
}

func TestEqual(t *testing.T) {
	base := time.Now()
	timeWithNanos := base.Add(1500 * time.Millisecond)
	rounded := timeWithNanos.Round(0).Truncate(time.Second)

	tests := []struct {
		name string
		a    infra.TimeoutOptions
		b    infra.TimeoutOptions
		want bool
	}{
		{
			name: "Both zero",
			a:    infra.TimeoutOptions{},
			b:    infra.TimeoutOptions{},
			want: true,
		},
		{
			name: "Shutdown times same after normalization",
			a: infra.TimeoutOptions{
				ShutdownTime: timeWithNanos,
			},
			b: infra.TimeoutOptions{
				ShutdownTime: rounded.Add(500 * time.Millisecond),
			},
			want: true,
		},
		{
			name: "Pause time mismatched",
			a: infra.TimeoutOptions{
				PauseTime: base.Add(time.Minute),
			},
			b: infra.TimeoutOptions{
				PauseTime: base.Add(2 * time.Minute),
			},
			want: false,
		},
		{
			name: "Shutdown and pause mismatch",
			a: infra.TimeoutOptions{
				ShutdownTime: base.Add(time.Minute),
			},
			b: infra.TimeoutOptions{
				ShutdownTime: base.Add(time.Minute),
				PauseTime:    base.Add(2 * time.Minute),
			},
			want: false,
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
		current   infra.TimeoutOptions
		requested infra.TimeoutOptions
		want      bool
	}{
		{
			name: "Current timeout zero, cannot extend",
			current: infra.TimeoutOptions{
				ShutdownTime: time.Time{},
			},
			requested: infra.TimeoutOptions{
				ShutdownTime: now.Add(time.Minute),
			},
			want: false,
		},
		{
			name: "Requested timeout zero, cannot extend",
			current: infra.TimeoutOptions{
				ShutdownTime: now.Add(time.Minute),
			},
			requested: infra.TimeoutOptions{},
			want:      false,
		},
		{
			name: "Requested later than current",
			current: infra.TimeoutOptions{
				ShutdownTime: now.Add(time.Minute),
			},
			requested: infra.TimeoutOptions{
				ShutdownTime: now.Add(2 * time.Minute),
			},
			want: true,
		},
		{
			name: "Requested earlier than current",
			current: infra.TimeoutOptions{
				ShutdownTime: now.Add(2 * time.Minute),
			},
			requested: infra.TimeoutOptions{
				ShutdownTime: now.Add(time.Minute),
			},
			want: false,
		},
		{
			name: "Pause time has priority",
			current: infra.TimeoutOptions{
				ShutdownTime: now.Add(10 * time.Minute),
				PauseTime:    now.Add(time.Minute),
			},
			requested: infra.TimeoutOptions{
				ShutdownTime: now.Add(20 * time.Minute),
				PauseTime:    now.Add(2 * time.Minute),
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
		in   infra.TimeoutOptions
		want time.Time
	}{
		{
			name: "Pause time takes precedence",
			in: infra.TimeoutOptions{
				ShutdownTime: now.Add(time.Minute),
				PauseTime:    now.Add(2 * time.Minute),
			},
			want: now.Add(2 * time.Minute),
		},
		{
			name: "Fallback to shutdown time",
			in: infra.TimeoutOptions{
				ShutdownTime: now.Add(time.Minute),
			},
			want: now.Add(time.Minute),
		},
		{
			name: "Both zero",
			in:   infra.TimeoutOptions{},
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
