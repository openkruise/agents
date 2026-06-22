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

package core

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

// ---- helpers ----------------------------------------------------------------

func newSandbox(namespace, name string, phase agentsv1alpha1.SandboxPhase, opts ...func(*agentsv1alpha1.Sandbox)) *agentsv1alpha1.Sandbox {
	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         namespace,
			Name:              name,
			CreationTimestamp: metav1.Now(),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: phase,
		},
	}
	for _, opt := range opts {
		opt(box)
	}
	return box
}

func withPriorityAnnotation(value string) func(*agentsv1alpha1.Sandbox) {
	return func(box *agentsv1alpha1.Sandbox) {
		if box.Annotations == nil {
			box.Annotations = map[string]string{}
		}
		box.Annotations[agentsv1alpha1.SandboxAnnotationPriority] = value
	}
}

func withCreationTimestamp(t time.Time) func(*agentsv1alpha1.Sandbox) {
	return func(box *agentsv1alpha1.Sandbox) {
		box.CreationTimestamp = metav1.NewTime(t)
	}
}

func withDeletionTimestamp(t time.Time) func(*agentsv1alpha1.Sandbox) {
	return func(box *agentsv1alpha1.Sandbox) {
		ts := metav1.NewTime(t)
		box.DeletionTimestamp = &ts
	}
}

func withReadyCondition(status metav1.ConditionStatus) func(*agentsv1alpha1.Sandbox) {
	return func(box *agentsv1alpha1.Sandbox) {
		box.Status.Conditions = []metav1.Condition{
			{
				Type:   string(agentsv1alpha1.SandboxConditionReady),
				Status: status,
			},
		}
	}
}

// ---- TestIsHighPrioritySandbox ---------------------------------------------

func TestIsHighPrioritySandbox(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		box      *agentsv1alpha1.Sandbox
		expected bool
	}{
		{
			name:     "no annotation",
			box:      newSandbox("default", "box1", agentsv1alpha1.SandboxPending),
			expected: false,
		},
		{
			name:     "empty annotation value",
			box:      newSandbox("default", "box1", agentsv1alpha1.SandboxPending, withPriorityAnnotation("")),
			expected: false,
		},
		{
			name:     "annotation value 0 (default/normal priority)",
			box:      newSandbox("default", "box1", agentsv1alpha1.SandboxPending, withPriorityAnnotation("0")),
			expected: false,
		},
		{
			name:     "annotation value 1 (high priority)",
			box:      newSandbox("default", "box1", agentsv1alpha1.SandboxPending, withPriorityAnnotation("1")),
			expected: true,
		},
		{
			name:     "annotation value 100 (high priority)",
			box:      newSandbox("default", "box1", agentsv1alpha1.SandboxPending, withPriorityAnnotation("100")),
			expected: true,
		},
		{
			name:     "annotation value negative",
			box:      newSandbox("default", "box1", agentsv1alpha1.SandboxPending, withPriorityAnnotation("-1")),
			expected: false,
		},
		{
			name:     "annotation value invalid string",
			box:      newSandbox("default", "box1", agentsv1alpha1.SandboxPending, withPriorityAnnotation("high")),
			expected: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsHighPrioritySandbox(ctx, tt.box)
			if got != tt.expected {
				t.Errorf("IsHighPrioritySandbox() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// ---- TestIsCreatingSandbox -------------------------------------------------

func TestIsCreatingSandbox(t *testing.T) {
	tests := []struct {
		name     string
		box      *agentsv1alpha1.Sandbox
		expected bool
	}{
		{
			name:     "deleting sandbox is not creating",
			box:      newSandbox("default", "box1", agentsv1alpha1.SandboxPending, withDeletionTimestamp(time.Now())),
			expected: false,
		},
		{
			name:     "phase Paused is not creating",
			box:      newSandbox("default", "box1", agentsv1alpha1.SandboxPaused),
			expected: false,
		},
		{
			name:     "phase Resuming is not creating",
			box:      newSandbox("default", "box1", agentsv1alpha1.SandboxResuming),
			expected: false,
		},
		{
			name:     "phase Succeeded is not creating",
			box:      newSandbox("default", "box1", agentsv1alpha1.SandboxSucceeded),
			expected: false,
		},
		{
			name:     "phase Failed is not creating",
			box:      newSandbox("default", "box1", agentsv1alpha1.SandboxFailed),
			expected: false,
		},
		{
			name:     "Running with Ready=True is not creating",
			box:      newSandbox("default", "box1", agentsv1alpha1.SandboxRunning, withReadyCondition(metav1.ConditionTrue)),
			expected: false,
		},
		{
			name:     "Running without Ready condition is creating",
			box:      newSandbox("default", "box1", agentsv1alpha1.SandboxRunning),
			expected: true,
		},
		{
			name:     "Running with Ready=False is creating",
			box:      newSandbox("default", "box1", agentsv1alpha1.SandboxRunning, withReadyCondition(metav1.ConditionFalse)),
			expected: true,
		},
		{
			name:     "phase Pending is creating",
			box:      newSandbox("default", "box1", agentsv1alpha1.SandboxPending),
			expected: true,
		},
		{
			name:     "empty phase is creating",
			box:      newSandbox("default", "box1", ""),
			expected: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isCreatingSandbox(tt.box)
			if got != tt.expected {
				t.Errorf("isCreatingSandbox() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// ---- TestUpdateRateLimiter -------------------------------------------------

func TestUpdateRateLimiter(t *testing.T) {
	t.Run("add high-priority creating sandbox", func(t *testing.T) {
		rl := NewRateLimiter()
		box := newSandbox("default", "box1", agentsv1alpha1.SandboxPending, withPriorityAnnotation("1"))
		result := rl.UpdateRateLimiter(box)
		if !result {
			t.Error("expected UpdateRateLimiter to return true after adding")
		}
		if rl.getPrioritySandboxTrackCount() != 1 {
			t.Errorf("expected track count=1, got %d", rl.getPrioritySandboxTrackCount())
		}
	})

	t.Run("add same sandbox twice returns true without duplicating", func(t *testing.T) {
		rl := NewRateLimiter()
		box := newSandbox("default", "box1", agentsv1alpha1.SandboxPending, withPriorityAnnotation("1"))
		rl.UpdateRateLimiter(box)
		result := rl.UpdateRateLimiter(box)
		if !result {
			t.Error("expected UpdateRateLimiter to return true on duplicate add")
		}
		if rl.getPrioritySandboxTrackCount() != 1 {
			t.Errorf("expected track count=1, got %d", rl.getPrioritySandboxTrackCount())
		}
	})

	t.Run("delete sandbox not in track returns false", func(t *testing.T) {
		rl := NewRateLimiter()
		box := newSandbox("default", "box1", agentsv1alpha1.SandboxRunning, withReadyCondition(metav1.ConditionTrue))
		result := rl.UpdateRateLimiter(box)
		if result {
			t.Error("expected UpdateRateLimiter to return false when not in track")
		}
		if rl.getPrioritySandboxTrackCount() != 0 {
			t.Errorf("expected track count=0, got %d", rl.getPrioritySandboxTrackCount())
		}
	})

	t.Run("sandbox transitions to ready, removed from track", func(t *testing.T) {
		rl := NewRateLimiter()
		box := newSandbox("default", "box1", agentsv1alpha1.SandboxPending)
		rl.UpdateRateLimiter(box)

		// Sandbox becomes ready
		box.Status.Phase = agentsv1alpha1.SandboxRunning
		box.Status.Conditions = []metav1.Condition{
			{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue},
		}
		result := rl.UpdateRateLimiter(box)
		if result {
			t.Error("expected UpdateRateLimiter to return false after sandbox is ready")
		}
		if rl.getPrioritySandboxTrackCount() != 0 {
			t.Errorf("expected track count=0 after removal, got %d", rl.getPrioritySandboxTrackCount())
		}
	})

	t.Run("sandbox creation timeout, removed from track", func(t *testing.T) {
		rl := NewRateLimiter()
		oldDelay := maxSandboxCreateDelay
		maxSandboxCreateDelay = 1 // 1 second delay
		defer func() { maxSandboxCreateDelay = oldDelay }()

		box := newSandbox("default", "box1", agentsv1alpha1.SandboxPending,
			withCreationTimestamp(time.Now().Add(-10*time.Second)),
		)
		// First call: sandbox is creating but timed out -> should be removed
		result := rl.UpdateRateLimiter(box)
		if result {
			t.Error("expected UpdateRateLimiter to return false (timed-out sandbox not tracked)")
		}
		if rl.getPrioritySandboxTrackCount() != 0 {
			t.Errorf("expected track count=0, got %d", rl.getPrioritySandboxTrackCount())
		}
	})

	t.Run("sandbox within delay is tracked", func(t *testing.T) {
		rl := NewRateLimiter()
		oldDelay := maxSandboxCreateDelay
		maxSandboxCreateDelay = 300 // 5 minutes
		defer func() { maxSandboxCreateDelay = oldDelay }()

		box := newSandbox("default", "box1", agentsv1alpha1.SandboxPending,
			withCreationTimestamp(time.Now().Add(-5*time.Second)),
		)
		result := rl.UpdateRateLimiter(box)
		if !result {
			t.Error("expected UpdateRateLimiter to return true for sandbox within delay")
		}
		if rl.getPrioritySandboxTrackCount() != 1 {
			t.Errorf("expected track count=1, got %d", rl.getPrioritySandboxTrackCount())
		}
	})

	t.Run("multiple sandboxes from different namespaces tracked independently", func(t *testing.T) {
		rl := NewRateLimiter()
		box1 := newSandbox("ns1", "box1", agentsv1alpha1.SandboxPending)
		box2 := newSandbox("ns2", "box1", agentsv1alpha1.SandboxPending)
		box3 := newSandbox("ns1", "box2", agentsv1alpha1.SandboxPending)
		rl.UpdateRateLimiter(box1)
		rl.UpdateRateLimiter(box2)
		rl.UpdateRateLimiter(box3)
		if rl.getPrioritySandboxTrackCount() != 3 {
			t.Errorf("expected track count=3, got %d", rl.getPrioritySandboxTrackCount())
		}
	})
}

// ---- TestGetRateLimitDuration ----------------------------------------------

func TestGetRateLimitDuration(t *testing.T) {
	ctx := context.Background()

	t.Run("feature gate disabled: no rate limiting", func(t *testing.T) {
		_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodRateLimitGate=false")
		defer func() { _ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodRateLimitGate=false") }()

		rl := NewRateLimiter()
		box := newSandbox("default", "box1", agentsv1alpha1.SandboxPending)
		d, stop := rl.getRateLimitDuration(ctx, nil, box)
		if stop || d != 0 {
			t.Errorf("expected (0, false) when feature gate disabled, got (%v, %v)", d, stop)
		}
	})

	t.Run("feature gate enabled: high-priority sandbox not rate-limited", func(t *testing.T) {
		_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodRateLimitGate=true")
		defer func() { _ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodRateLimitGate=false") }()

		rl := NewRateLimiter()
		box := newSandbox("default", "box1", agentsv1alpha1.SandboxPending, withPriorityAnnotation("1"))
		d, stop := rl.getRateLimitDuration(ctx, nil, box)
		if stop || d != 0 {
			t.Errorf("expected (0, false) for high-priority sandbox, got (%v, %v)", d, stop)
		}
		// High-priority sandbox should have been added to track
		if rl.getPrioritySandboxTrackCount() != 1 {
			t.Errorf("expected high-priority sandbox to be tracked, count=%d", rl.getPrioritySandboxTrackCount())
		}
	})

	t.Run("feature gate enabled: normal sandbox not rate-limited when below threshold", func(t *testing.T) {
		_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodRateLimitGate=true")
		defer func() { _ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodRateLimitGate=false") }()

		rl := NewRateLimiter()
		box := newSandbox("default", "box1", agentsv1alpha1.SandboxPending) // normal priority
		d, stop := rl.getRateLimitDuration(ctx, nil, box)
		if stop || d != 0 {
			t.Errorf("expected (0, false) below threshold, got (%v, %v)", d, stop)
		}
	})

	t.Run("feature gate enabled: normal sandbox rate-limited when threshold exceeded and within delay", func(t *testing.T) {
		_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodRateLimitGate=true")
		defer func() { _ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodRateLimitGate=false") }()

		oldThreshold := prioritySandboxThreshold
		prioritySandboxThreshold = 2
		defer func() { prioritySandboxThreshold = oldThreshold }()

		oldDelay := maxSandboxCreateDelay
		maxSandboxCreateDelay = 300 // 5 minutes
		defer func() { maxSandboxCreateDelay = oldDelay }()

		rl := NewRateLimiter()
		// Pre-fill track to exceed threshold
		for i := 0; i < 3; i++ {
			b := newSandbox("default", fmt.Sprintf("high-%d", i), agentsv1alpha1.SandboxPending)
			key := fmt.Sprintf("%s/%s", b.Namespace, b.Name)
			rl.highPrioritySandboxTrack[key] = &SandboxTrack{Namespace: b.Namespace, Name: b.Name}
		}

		// Sandbox created recently (within delay) should be rate-limited
		box := newSandbox("default", "normal-box", agentsv1alpha1.SandboxPending,
			withCreationTimestamp(time.Now().Add(-5*time.Second)))
		d, stop := rl.getRateLimitDuration(ctx, nil, box)
		if !stop {
			t.Error("expected stop=true when threshold exceeded and within delay")
		}
		if d != 3*time.Second {
			t.Errorf("expected requeue after 3s, got %v", d)
		}
	})

	t.Run("feature gate enabled: normal sandbox exceeding delay not rate-limited", func(t *testing.T) {
		_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodRateLimitGate=true")
		defer func() { _ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodRateLimitGate=false") }()

		oldThreshold := prioritySandboxThreshold
		prioritySandboxThreshold = 0 // threshold at 0, any count triggers
		defer func() { prioritySandboxThreshold = oldThreshold }()

		oldDelay := maxSandboxCreateDelay
		maxSandboxCreateDelay = 60 // 60 seconds
		defer func() { maxSandboxCreateDelay = oldDelay }()

		rl := NewRateLimiter()
		rl.highPrioritySandboxTrack["ns/hp1"] = &SandboxTrack{Namespace: "ns", Name: "hp1"}

		// Sandbox created long ago (exceeding delay) should NOT be rate-limited
		box := newSandbox("default", "normal-box", agentsv1alpha1.SandboxPending,
			withCreationTimestamp(time.Now().Add(-120*time.Second)))
		d, stop := rl.getRateLimitDuration(ctx, nil, box)
		if stop || d != 0 {
			t.Errorf("expected (0, false) when sandbox exceeds maxSandboxCreateDelay, got (%v, %v)", d, stop)
		}
	})

	t.Run("feature gate enabled: normal sandbox with pod already exists still rate-limited if within delay", func(t *testing.T) {
		_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodRateLimitGate=true")
		defer func() { _ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodRateLimitGate=false") }()

		oldThreshold := prioritySandboxThreshold
		prioritySandboxThreshold = 0 // threshold at 0, any count triggers
		defer func() { prioritySandboxThreshold = oldThreshold }()

		oldDelay := maxSandboxCreateDelay
		maxSandboxCreateDelay = 300 // 5 minutes
		defer func() { maxSandboxCreateDelay = oldDelay }()

		rl := NewRateLimiter()
		rl.highPrioritySandboxTrack["ns/hp1"] = &SandboxTrack{Namespace: "ns", Name: "hp1"}

		// Pod exists but sandbox within delay: still rate-limited
		box := newSandbox("default", "normal-box", agentsv1alpha1.SandboxPending,
			withCreationTimestamp(time.Now().Add(-5*time.Second)))
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1"}}
		d, stop := rl.getRateLimitDuration(ctx, pod, box)
		if !stop || d != 3*time.Second {
			t.Errorf("expected (3s, true) when within delay, got (%v, %v)", d, stop)
		}
	})

	t.Run("feature gate enabled: running phase sandbox rate-limited if within delay", func(t *testing.T) {
		_ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodRateLimitGate=true")
		defer func() { _ = utilfeature.DefaultMutableFeatureGate.Set("SandboxCreatePodRateLimitGate=false") }()

		oldThreshold := prioritySandboxThreshold
		prioritySandboxThreshold = 0
		defer func() { prioritySandboxThreshold = oldThreshold }()

		oldDelay := maxSandboxCreateDelay
		maxSandboxCreateDelay = 300
		defer func() { maxSandboxCreateDelay = oldDelay }()

		rl := NewRateLimiter()
		rl.highPrioritySandboxTrack["ns/hp1"] = &SandboxTrack{Namespace: "ns", Name: "hp1"}

		// Running sandbox within delay: still rate-limited
		box := newSandbox("default", "normal-box", agentsv1alpha1.SandboxRunning,
			withCreationTimestamp(time.Now().Add(-5*time.Second)))
		d, stop := rl.getRateLimitDuration(ctx, nil, box)
		if !stop || d != 3*time.Second {
			t.Errorf("expected (3s, true) for running sandbox within delay, got (%v, %v)", d, stop)
		}
	})
}

// ---- maxSandboxCreateDelay -----------------------------------------------

func TestMaxSandboxCreateDelay(t *testing.T) {
	old := maxSandboxCreateDelay
	maxSandboxCreateDelay = 42
	defer func() { maxSandboxCreateDelay = old }()
	if got := MaxSandboxCreateDelay(); got != 42 {
		t.Errorf("maxSandboxCreateDelay() = %d, want 42", got)
	}
}

// ---- TestNewRateLimiter ----------------------------------------------------

func TestNewRateLimiter(t *testing.T) {
	rl := NewRateLimiter()
	if rl == nil {
		t.Fatal("NewRateLimiter() returned nil")
	}
	if rl.highPrioritySandboxTrack == nil {
		t.Error("expected highPrioritySandboxTrack to be initialized")
	}
	if rl.getPrioritySandboxTrackCount() != 0 {
		t.Errorf("expected empty track on init, got %d", rl.getPrioritySandboxTrackCount())
	}
}
