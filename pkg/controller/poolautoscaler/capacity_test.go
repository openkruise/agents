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

package poolautoscaler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestResolveIntOrPercent(t *testing.T) {
	tests := []struct {
		name     string
		val      intstr.IntOrString
		total    int32
		expected int32
	}{
		{
			name:     "absolute value",
			val:      intstr.FromInt32(10),
			total:    100,
			expected: 10,
		},
		{
			name:     "percentage 70% of 10",
			val:      intstr.FromString("70%"),
			total:    10,
			expected: 7,
		},
		{
			name:     "percentage 70% of 4 rounds up",
			val:      intstr.FromString("70%"),
			total:    4,
			expected: 3,
		},
		{
			name:     "percentage 60% of 4 rounds up",
			val:      intstr.FromString("60%"),
			total:    4,
			expected: 3,
		},
		{
			name:     "percentage 80% of 4 rounds up",
			val:      intstr.FromString("80%"),
			total:    4,
			expected: 4,
		},
		{
			name:     "percentage 10% of 100",
			val:      intstr.FromString("10%"),
			total:    100,
			expected: 10,
		},
		{
			name:     "zero absolute",
			val:      intstr.FromInt32(0),
			total:    50,
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := resolveIntOrPercent(tt.val, tt.total)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// Tolerance coverage is in TestComputeWatermarks.

func TestClampToBounds(t *testing.T) {
	tests := []struct {
		name        string
		minReplicas int32
		maxReplicas int32
		desired     int32
		expected    int32
	}{
		{
			name:        "within bounds",
			minReplicas: 5,
			maxReplicas: 20,
			desired:     10,
			expected:    10,
		},
		{
			name:        "below min",
			minReplicas: 5,
			maxReplicas: 20,
			desired:     2,
			expected:    5,
		},
		{
			name:        "above max",
			minReplicas: 5,
			maxReplicas: 20,
			desired:     30,
			expected:    20,
		},
		{
			name:        "nil minReplicas defaults to 0",
			minReplicas: 0,
			maxReplicas: 10,
			desired:     0,
			expected:    0,
		},
		{
			name:        "desired equals min",
			minReplicas: 5,
			maxReplicas: 20,
			desired:     5,
			expected:    5,
		},
		{
			name:        "desired equals max",
			minReplicas: 5,
			maxReplicas: 20,
			desired:     20,
			expected:    20,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reconciler{}
			pa := &agentsv1alpha1.PoolAutoscaler{
				Spec: agentsv1alpha1.PoolAutoscalerSpec{
					MinReplicas: tt.minReplicas,
					MaxReplicas: tt.maxReplicas,
				},
			}
			result := r.clampToBounds(pa, tt.desired)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestComputeDesiredReplicas(t *testing.T) {
	tests := []struct {
		name            string
		capacityPolicy  *agentsv1alpha1.CapacityPolicy
		specReplicas    int32
		statusReplicas  int32
		available       int32
		expectedDesired int32
		expectedReason  string
	}{
		{
			name:            "no scaling policy returns spec unchanged",
			capacityPolicy:  nil,
			specReplicas:    10,
			statusReplicas:  10,
			available:       10,
			expectedDesired: 10,
			expectedReason:  "no scaling policy",
		},
		{
			name: "scale-up in progress (spec > status) — wait",
			capacityPolicy: &agentsv1alpha1.CapacityPolicy{
				TargetAvailable: intstr.FromInt32(10),
				Tolerance: func() *intstr.IntOrString {
					v := intstr.FromInt32(2)
					return &v
				}(),
			},
			specReplicas:    10,
			statusReplicas:  5,
			available:       0, // below lower, but spec > status → wait
			expectedDesired: 10,
			expectedReason:  "waiting for previous scale-up to complete",
		},
		{
			name: "absolute: steady state within tolerance — no change",
			capacityPolicy: &agentsv1alpha1.CapacityPolicy{
				TargetAvailable: intstr.FromInt32(10),
				Tolerance: func() *intstr.IntOrString {
					v := intstr.FromInt32(2)
					return &v
				}(),
			},
			specReplicas:    10,
			statusReplicas:  10,
			available:       10,
			expectedDesired: 10,
			expectedReason:  "within tolerance",
		},
		{
			name: "absolute: available below lower — scale up by deficit",
			capacityPolicy: &agentsv1alpha1.CapacityPolicy{
				TargetAvailable: intstr.FromInt32(7),
				Tolerance: func() *intstr.IntOrString {
					v := intstr.FromInt32(1)
					return &v
				}(),
			},
			specReplicas:    10,
			statusReplicas:  10,
			available:       4,  // below lower=6, target=7, deficit=7-4=3
			expectedDesired: 13, // statusReplicas(10) + 3 = 13
			expectedReason:  "available below lower watermark",
		},
		{
			name: "absolute: available in dead zone — stable",
			capacityPolicy: &agentsv1alpha1.CapacityPolicy{
				TargetAvailable: intstr.FromInt32(10),
				Tolerance: func() *intstr.IntOrString {
					v := intstr.FromInt32(5)
					return &v
				}(),
			},
			specReplicas:    5,
			statusReplicas:  5,
			available:       5,
			expectedDesired: 5,
			expectedReason:  "within tolerance",
		},
		{
			name: "absolute: scale down by excess",
			capacityPolicy: &agentsv1alpha1.CapacityPolicy{
				TargetAvailable: intstr.FromInt32(7),
				Tolerance: func() *intstr.IntOrString {
					v := intstr.FromInt32(1)
					return &v
				}(),
			},
			specReplicas:    10,
			statusReplicas:  10,
			available:       9, // above upper=8, excess=9-7=2
			expectedDesired: 8, // 10 - 2 = 8
			expectedReason:  "available above upper watermark",
		},
		{
			name: "percentage: convergence step 10→7",
			capacityPolicy: &agentsv1alpha1.CapacityPolicy{
				TargetAvailable: intstr.FromString("70%"),
				Tolerance: func() *intstr.IntOrString {
					v := intstr.FromString("10%")
					return &v
				}(),
			},
			specReplicas:    10,
			statusReplicas:  10,
			available:       10, // target=7, excess=10-7=3
			expectedDesired: 7,  // 10-3=7
			expectedReason:  "available above upper watermark",
		},
		{
			name: "percentage: low readiness — scale up by deficit",
			capacityPolicy: &agentsv1alpha1.CapacityPolicy{
				TargetAvailable: intstr.FromString("70%"),
				Tolerance: func() *intstr.IntOrString {
					v := intstr.FromString("10%")
					return &v
				}(),
			},
			specReplicas:    10,
			statusReplicas:  10,
			available:       5,  // target=7, lower=6, 5<6 → deficit=7-5=2
			expectedDesired: 12, // 10 + 2 = 12
			expectedReason:  "available below lower watermark",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reconciler{
				monitors: make(map[types.NamespacedName]*capacityMonitor),
			}
			pa := &agentsv1alpha1.PoolAutoscaler{
				Spec: agentsv1alpha1.PoolAutoscalerSpec{
					CapacityPolicy: tt.capacityPolicy,
				},
			}
			pa.Name = "test-pa"
			pa.Namespace = "default"

			result, err := r.computeDesiredReplicas(
				context.Background(), pa,
				tt.specReplicas, tt.statusReplicas, tt.available,
			)
			assert.NoError(t, err)
			assert.Equal(t, tt.expectedDesired, result.desiredReplicas, "desired replicas mismatch")
			assert.Contains(t, result.reason, tt.expectedReason)
		})
	}
}

func TestComputeWatermarks(t *testing.T) {
	tests := []struct {
		name            string
		targetAvailable intstr.IntOrString
		tolerance       *intstr.IntOrString
		base            int32
		expectedTarget  int32
		expectedLower   int32
		expectedUpper   int32
	}{
		{
			name:            "absolute target and tolerance",
			targetAvailable: intstr.FromInt32(10),
			tolerance: func() *intstr.IntOrString {
				v := intstr.FromInt32(5)
				return &v
			}(),
			base:           100,
			expectedTarget: 10,
			expectedLower:  5,
			expectedUpper:  15,
		},
		{
			name:            "percentage 70%/10% with base=4 (proposal t0)",
			targetAvailable: intstr.FromString("70%"),
			tolerance: func() *intstr.IntOrString {
				v := intstr.FromString("10%")
				return &v
			}(),
			base:           4,
			expectedTarget: 3, // ceil(4*0.7) = ceil(2.8) = 3
			expectedLower:  3, // ceil(4*0.6) = ceil(2.4) = 3
			expectedUpper:  4, // ceil(4*0.8) = ceil(3.2) = 4
		},
		{
			name:            "percentage 70%/10% with base=7 (proposal t4)",
			targetAvailable: intstr.FromString("70%"),
			tolerance: func() *intstr.IntOrString {
				v := intstr.FromString("10%")
				return &v
			}(),
			base:           7,
			expectedTarget: 5, // ceil(7*0.7) = ceil(4.9) = 5
			expectedLower:  5, // ceil(7*0.6) = ceil(4.2) = 5
			expectedUpper:  6, // ceil(7*0.8) = ceil(5.6) = 6
		},
		{
			name:            "percentage 70%/10% with base=12 (proposal t6)",
			targetAvailable: intstr.FromString("70%"),
			tolerance: func() *intstr.IntOrString {
				v := intstr.FromString("10%")
				return &v
			}(),
			base:           12,
			expectedTarget: 9,  // ceil(12*0.7) = ceil(8.4) = 9
			expectedLower:  8,  // ceil(12*0.6) = ceil(7.2) = 8
			expectedUpper:  10, // ceil(12*0.8) = ceil(9.6) = 10
		},
		{
			name:            "percentage 70%/10% with base=21 (proposal t8)",
			targetAvailable: intstr.FromString("70%"),
			tolerance: func() *intstr.IntOrString {
				v := intstr.FromString("10%")
				return &v
			}(),
			base:           21,
			expectedTarget: 15, // ceil(21*0.7) = ceil(14.7) = 15
			expectedLower:  13, // ceil(21*0.6) = ceil(12.6) = 13
			expectedUpper:  17, // ceil(21*0.8) = ceil(16.8) = 17
		},
		{
			name:            "percentage 70%/10% with base=36 (proposal t10)",
			targetAvailable: intstr.FromString("70%"),
			tolerance: func() *intstr.IntOrString {
				v := intstr.FromString("10%")
				return &v
			}(),
			base:           36,
			expectedTarget: 26, // ceil(36*0.7) = ceil(25.2) = 26
			expectedLower:  22, // ceil(36*0.6) = ceil(21.6) = 22
			expectedUpper:  29, // ceil(36*0.8) = ceil(28.8) = 29
		},
		{
			name:            "nil tolerance defaults to 10%",
			targetAvailable: intstr.FromString("70%"),
			tolerance:       nil,
			base:            10,
			expectedTarget:  7, // ceil(10*0.7) = 7
			expectedLower:   6, // ceil(10*0.6) = 6
			expectedUpper:   8, // ceil(10*0.8) = 8
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, lower, upper := computeWatermarks(tt.targetAvailable, tt.tolerance, tt.base)
			assert.Equal(t, tt.expectedTarget, target, "target mismatch")
			assert.Equal(t, tt.expectedLower, lower, "lower watermark mismatch")
			assert.Equal(t, tt.expectedUpper, upper, "upper watermark mismatch")
		})
	}
}

func TestRecordScale(t *testing.T) {
	t.Run("record scale up sets lastScaleUpAt", func(t *testing.T) {
		m := &capacityMonitor{
			lastScaleDownAt: time.Now().Add(-5 * time.Second),
		}
		now := time.Now()
		m.recordScale(true, now)
		assert.Equal(t, now, m.lastScaleUpAt)
		// lastScaleDownAt unchanged
		assert.False(t, m.lastScaleDownAt.IsZero())
	})

	t.Run("record scale down sets lastScaleDownAt", func(t *testing.T) {
		m := &capacityMonitor{
			lastScaleUpAt: time.Now().Add(-5 * time.Second),
		}
		now := time.Now()
		m.recordScale(false, now)
		assert.Equal(t, now, m.lastScaleDownAt)
		// lastScaleUpAt unchanged
		assert.False(t, m.lastScaleUpAt.IsZero())
	})

	t.Run("record scale does not clear samples", func(t *testing.T) {
		now := time.Now()
		m := &capacityMonitor{
			samples: []sample{
				{timestamp: now.Add(-30 * time.Second), available: 5, statusReplicas: 10},
				{timestamp: now.Add(-15 * time.Second), available: 6, statusReplicas: 10},
			},
		}
		m.recordScale(true, now)
		assert.Len(t, m.samples, 2)
	})
}

func TestAddSampleIfDue(t *testing.T) {
	expectedCap := int(observationWindowSeconds/samplingIntervalSeconds) + 1 // 4 with default 15/5

	tests := []struct {
		name           string
		lastSampleAt   time.Time
		now            time.Time
		available      int32
		statusReplicas int32
		expectedAdded  bool
		expectedLen    int
		expectedCap    int // -1 means don't check
	}{
		{
			name:           "first sample added",
			lastSampleAt:   time.Time{},
			now:            time.Now(),
			available:      10,
			statusReplicas: 20,
			expectedAdded:  true,
			expectedLen:    1,
			expectedCap:    expectedCap,
		},
		{
			name:           "sample within interval rejected",
			lastSampleAt:   time.Now(),
			now:            time.Now().Add(time.Duration(samplingIntervalSeconds) * time.Second / 2), // half the sampling interval
			available:      10,
			statusReplicas: 20,
			expectedAdded:  false,
			expectedLen:    0,
			expectedCap:    -1,
		},
		{
			name:           "sample after interval added",
			lastSampleAt:   time.Now().Add(-time.Duration(samplingIntervalSeconds) * time.Second * 2), // twice the sampling interval
			now:            time.Now(),
			available:      10,
			statusReplicas: 20,
			expectedAdded:  true,
			expectedLen:    1,
			expectedCap:    expectedCap,
		},
		{
			name:           "pre-allocation on first use",
			lastSampleAt:   time.Time{},
			now:            time.Now(),
			available:      1,
			statusReplicas: 1,
			expectedAdded:  true,
			expectedLen:    1,
			expectedCap:    expectedCap,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &capacityMonitor{
				lastSampleAt: tt.lastSampleAt,
			}
			added := m.addSampleIfDue(tt.available, tt.statusReplicas, tt.now)
			assert.Equal(t, tt.expectedAdded, added)
			assert.Len(t, m.samples, tt.expectedLen)
			if tt.expectedCap >= 0 {
				assert.Equal(t, tt.expectedCap, cap(m.samples))
			}
		})
	}
}

func TestPruneSamples(t *testing.T) {
	window := time.Duration(observationWindowSeconds) * time.Second

	tests := []struct {
		name          string
		setupSamples  func(now time.Time) []sample
		expectedLen   int
		expectedAvail int32 // available of first remaining sample (0 means don't check)
	}{
		{
			name: "all samples within window",
			setupSamples: func(now time.Time) []sample {
				return []sample{
					{timestamp: now.Add(-window / 2), available: 5, statusReplicas: 10},
					{timestamp: now.Add(-window / 4), available: 6, statusReplicas: 10},
				}
			},
			expectedLen:   2,
			expectedAvail: 0,
		},
		{
			name: "old samples pruned",
			setupSamples: func(now time.Time) []sample {
				return []sample{
					{timestamp: now.Add(-window * 2), available: 5, statusReplicas: 10},
					{timestamp: now.Add(-window / 2), available: 6, statusReplicas: 10},
				}
			},
			expectedLen:   1,
			expectedAvail: 6,
		},
		{
			name: "boundary - exactly at window edge",
			setupSamples: func(now time.Time) []sample {
				return []sample{
					{timestamp: now.Add(-window), available: 5, statusReplicas: 10}, // exactly at cutoff
					{timestamp: now.Add(-window / 3), available: 6, statusReplicas: 10},
				}
			},
			expectedLen:   1,
			expectedAvail: 6,
		},
		{
			name: "empty samples",
			setupSamples: func(now time.Time) []sample {
				return nil
			},
			expectedLen:   0,
			expectedAvail: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			m := &capacityMonitor{
				samples: tt.setupSamples(now),
			}
			m.pruneSamples(now)
			assert.Len(t, m.samples, tt.expectedLen)
			if tt.expectedAvail > 0 && tt.expectedLen > 0 {
				assert.Equal(t, tt.expectedAvail, m.samples[0].available)
			}
		})
	}
}

func TestAggregatedValues(t *testing.T) {
	tests := []struct {
		name           string
		samples        []sample
		expectedAvail  int32
		expectedStatus int32
		expectedOk     bool
	}{
		{
			name:           "empty samples",
			samples:        nil,
			expectedAvail:  0,
			expectedStatus: 0,
			expectedOk:     false,
		},
		{
			name: "single sample",
			samples: []sample{
				{timestamp: time.Now(), available: 7, statusReplicas: 20},
			},
			expectedAvail:  7,
			expectedStatus: 20,
			expectedOk:     true,
		},
		{
			name: "multiple samples",
			samples: []sample{
				{timestamp: time.Now().Add(-30 * time.Second), available: 10, statusReplicas: 20},
				{timestamp: time.Now().Add(-20 * time.Second), available: 8, statusReplicas: 20},
				{timestamp: time.Now().Add(-10 * time.Second), available: 6, statusReplicas: 20},
			},
			expectedAvail:  8, // (10+8+6)/3 = 8
			expectedStatus: 20,
			expectedOk:     true,
		},
		{
			name: "rounding uses math.Round",
			samples: []sample{
				{timestamp: time.Now().Add(-10 * time.Second), available: 7, statusReplicas: 20},
				{timestamp: time.Now(), available: 8, statusReplicas: 20},
			},
			expectedAvail:  8, // (7+8)/2 = 7.5 rounds to 8
			expectedStatus: 20,
			expectedOk:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &capacityMonitor{
				samples: tt.samples,
			}
			avail, status, _, ok := m.aggregatedValues()
			assert.Equal(t, tt.expectedAvail, avail)
			assert.Equal(t, tt.expectedStatus, status)
			assert.Equal(t, tt.expectedOk, ok)
		})
	}
}

func TestObserveAndAggregate(t *testing.T) {
	tests := []struct {
		name              string
		setupMonitor      func(m *capacityMonitor)
		rawAvailable      int32
		rawStatusReplicas int32
		hasSecondCall     bool
		secondAvailable   int32
		secondStatus      int32
		expectedAvail     int32
		expectedStatus    int32
	}{
		{
			name: "no samples - warm-up fallback",
			setupMonitor: func(m *capacityMonitor) {
				// empty monitor — first sample will be added with raw values
			},
			rawAvailable:      10,
			rawStatusReplicas: 20,
			expectedAvail:     10,
			expectedStatus:    20,
		},
		{
			name: "single sample",
			setupMonitor: func(m *capacityMonitor) {
				now := time.Now()
				m.samples = []sample{
					{timestamp: now, available: 10, statusReplicas: 20},
				}
				m.lastSampleAt = now
			},
			rawAvailable:      5, // different from existing sample
			rawStatusReplicas: 10,
			expectedAvail:     10, // returns existing sample's value, not raw
			expectedStatus:    20,
		},
		{
			name: "multiple samples average",
			setupMonitor: func(m *capacityMonitor) {
				now := time.Now()
				window := time.Duration(observationWindowSeconds) * time.Second
				m.samples = []sample{
					{timestamp: now.Add(-window * 2 / 3), available: 10, statusReplicas: 20},
					{timestamp: now.Add(-window / 2), available: 8, statusReplicas: 20},
					{timestamp: now.Add(-window / 4), available: 6, statusReplicas: 20},
				}
				m.lastSampleAt = now // recent enough to prevent new sample addition
			},
			rawAvailable:      5,
			rawStatusReplicas: 10,
			expectedAvail:     8, // (10+8+6)/3 = 8
			expectedStatus:    20,
		},
		{
			name: "sampling interval enforced",
			setupMonitor: func(m *capacityMonitor) {
				// empty monitor — first call will add a sample
			},
			rawAvailable:      10,
			rawStatusReplicas: 20,
			hasSecondCall:     true,
			secondAvailable:   5,
			secondStatus:      10,
			expectedAvail:     10, // second call returns first sample's value
			expectedStatus:    20,
		},
		{
			name: "window expiry prunes old samples",
			setupMonitor: func(m *capacityMonitor) {
				oldTime := time.Now().Add(-120 * time.Second)
				m.samples = []sample{
					{timestamp: oldTime, available: 100, statusReplicas: 100},
				}
				m.lastSampleAt = oldTime
			},
			rawAvailable:      5,
			rawStatusReplicas: 10,
			expectedAvail:     5, // old sample pruned, only new sample (5, 10)
			expectedStatus:    10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reconciler{
				monitors: make(map[types.NamespacedName]*capacityMonitor),
			}
			pa := &agentsv1alpha1.PoolAutoscaler{}
			pa.Name = "test-pa"
			pa.Namespace = "default"
			key := types.NamespacedName{Namespace: pa.Namespace, Name: pa.Name}

			monitor := &capacityMonitor{}
			tt.setupMonitor(monitor)
			r.monitors[key] = monitor

			avail, status := r.observeAndAggregate(
				context.Background(), pa, tt.rawAvailable, tt.rawStatusReplicas,
			)

			if tt.hasSecondCall {
				avail, status = r.observeAndAggregate(
					context.Background(), pa, tt.secondAvailable, tt.secondStatus,
				)
			}

			assert.Equal(t, tt.expectedAvail, avail)
			assert.Equal(t, tt.expectedStatus, status)
		})
	}
}

func TestApplyStabilizationWindow_Cooldown(t *testing.T) {
	tests := []struct {
		name            string
		specReplicas    int32
		desiredReplicas int32
		scaleUpWindow   *int32
		scaleDownWindow *int32
		setupMonitor    func(m *capacityMonitor)
		expected        int32
	}{
		{
			name:            "first scale up - no cooldown",
			specReplicas:    10,
			desiredReplicas: 15,
			scaleUpWindow:   int32Ptr(60),
			scaleDownWindow: int32Ptr(60),
			setupMonitor: func(m *capacityMonitor) {
				// lastScaleUpAt is zero (default) — first scale is immediate
			},
			expected: 15,
		},
		{
			name:            "scale up in cooldown",
			specReplicas:    10,
			desiredReplicas: 15,
			scaleUpWindow:   int32Ptr(60),
			scaleDownWindow: int32Ptr(60),
			setupMonitor: func(m *capacityMonitor) {
				m.lastScaleUpAt = time.Now().Add(-10 * time.Second) // recent, within 60s cooldown
			},
			expected: 10, // returns spec (blocked by cooldown)
		},
		{
			name:            "scale up cooldown expired",
			specReplicas:    10,
			desiredReplicas: 15,
			scaleUpWindow:   int32Ptr(60),
			scaleDownWindow: int32Ptr(60),
			setupMonitor: func(m *capacityMonitor) {
				m.lastScaleUpAt = time.Now().Add(-100 * time.Second) // 100s > 60s cooldown
			},
			expected: 15,
		},
		{
			name:            "first scale down - no cooldown",
			specReplicas:    10,
			desiredReplicas: 5,
			scaleUpWindow:   int32Ptr(60),
			scaleDownWindow: int32Ptr(60),
			setupMonitor: func(m *capacityMonitor) {
				// lastScaleDownAt is zero (default) — first scale is immediate
			},
			expected: 5,
		},
		{
			name:            "scale down in cooldown",
			specReplicas:    10,
			desiredReplicas: 5,
			scaleUpWindow:   int32Ptr(60),
			scaleDownWindow: int32Ptr(60),
			setupMonitor: func(m *capacityMonitor) {
				m.lastScaleDownAt = time.Now().Add(-10 * time.Second) // recent, within 60s cooldown
			},
			expected: 10, // returns spec (blocked by cooldown)
		},
		{
			name:            "scale down cooldown expired",
			specReplicas:    10,
			desiredReplicas: 5,
			scaleUpWindow:   int32Ptr(60),
			scaleDownWindow: int32Ptr(60),
			setupMonitor: func(m *capacityMonitor) {
				m.lastScaleDownAt = time.Now().Add(-100 * time.Second) // 100s > 60s cooldown
			},
			expected: 5,
		},
		{
			name:            "scale down blocked by recent scale up",
			specReplicas:    10,
			desiredReplicas: 5, // scale down
			scaleUpWindow:   int32Ptr(60),
			scaleDownWindow: int32Ptr(60),
			setupMonitor: func(m *capacityMonitor) {
				m.lastScaleUpAt = time.Now().Add(-10 * time.Second) // recent scale up
				// lastScaleDownAt is zero — but scale down still blocked by recent scale up
			},
			expected: 10, // scale down blocked: last action (scale up) was 10s ago < 60s window
		},
		{
			name:            "dead zone - no scaling",
			specReplicas:    10,
			desiredReplicas: 10,
			scaleUpWindow:   int32Ptr(60),
			scaleDownWindow: int32Ptr(60),
			setupMonitor: func(m *capacityMonitor) {
				m.lastScaleUpAt = time.Now().Add(-10 * time.Second)
				m.lastScaleDownAt = time.Now().Add(-10 * time.Second)
			},
			expected: 10,
		},
		{
			name:            "window zero means no cooldown",
			specReplicas:    10,
			desiredReplicas: 15,
			scaleUpWindow:   int32Ptr(0),
			scaleDownWindow: int32Ptr(0),
			setupMonitor: func(m *capacityMonitor) {
				m.lastScaleUpAt = time.Now().Add(-1 * time.Second) // recent, would be in cooldown if window > 0
			},
			expected: 15,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reconciler{
				monitors: make(map[types.NamespacedName]*capacityMonitor),
			}
			pa := &agentsv1alpha1.PoolAutoscaler{
				Spec: agentsv1alpha1.PoolAutoscalerSpec{
					CapacityPolicy: &agentsv1alpha1.CapacityPolicy{
						ScaleUp: &agentsv1alpha1.CapacityScalingRules{
							StabilizationWindowSeconds: tt.scaleUpWindow,
						},
						ScaleDown: &agentsv1alpha1.CapacityScalingRules{
							StabilizationWindowSeconds: tt.scaleDownWindow,
						},
					},
				},
			}
			pa.Name = "test-pa"
			pa.Namespace = "default"

			key := types.NamespacedName{Namespace: pa.Namespace, Name: pa.Name}
			monitor := &capacityMonitor{}
			tt.setupMonitor(monitor)
			r.monitors[key] = monitor

			result := r.applyStabilizationWindow(
				pa, tt.specReplicas, tt.desiredReplicas,
			)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestRecordScaleAction(t *testing.T) {
	t.Run("record scale up action", func(t *testing.T) {
		r := &Reconciler{monitors: make(map[types.NamespacedName]*capacityMonitor)}
		key := types.NamespacedName{Namespace: "default", Name: "test-pa"}
		monitor := &capacityMonitor{}
		r.monitors[key] = monitor

		r.recordScaleAction(key, true)

		assert.False(t, monitor.lastScaleUpAt.IsZero())
		assert.True(t, monitor.lastScaleDownAt.IsZero())
	})

	t.Run("record scale down action", func(t *testing.T) {
		r := &Reconciler{monitors: make(map[types.NamespacedName]*capacityMonitor)}
		key := types.NamespacedName{Namespace: "default", Name: "test-pa"}
		monitor := &capacityMonitor{}
		r.monitors[key] = monitor

		r.recordScaleAction(key, false)

		assert.True(t, monitor.lastScaleUpAt.IsZero())
		assert.False(t, monitor.lastScaleDownAt.IsZero())
	})

	t.Run("samples not cleared after recordScaleAction", func(t *testing.T) {
		r := &Reconciler{monitors: make(map[types.NamespacedName]*capacityMonitor)}
		key := types.NamespacedName{Namespace: "default", Name: "test-pa"}
		now := time.Now()
		monitor := &capacityMonitor{
			samples: []sample{
				{timestamp: now.Add(-30 * time.Second), available: 5, statusReplicas: 10},
				{timestamp: now.Add(-15 * time.Second), available: 6, statusReplicas: 10},
			},
		}
		r.monitors[key] = monitor

		r.recordScaleAction(key, true)

		assert.Len(t, monitor.samples, 2) // samples still exist
	})
}

// TestApplyStabilizationWindow_DefaultWindow tests the fallback logic when
// ScaleUp/ScaleDown or their StabilizationWindowSeconds are nil.
// - nil ScaleUp -> defaultScaleUpStabilization = 0 (no cooldown, immediate)
// - nil ScaleDown -> defaultScaleDownStabilization = 300 (300s cooldown)
func TestApplyStabilizationWindow_DefaultWindow(t *testing.T) {
	tests := []struct {
		name            string
		specReplicas    int32
		desiredReplicas int32
		setupMonitor    func(m *capacityMonitor)
		expected        int32
	}{
		{
			name:            "nil ScaleUp and ScaleDown - scale up uses default 0 (immediate)",
			specReplicas:    10,
			desiredReplicas: 15,
			setupMonitor: func(m *capacityMonitor) {
				m.lastScaleUpAt = time.Now().Add(-1 * time.Second) // recent, but default is 0 = no cooldown
			},
			expected: 15,
		},
		{
			name:            "nil ScaleUp and ScaleDown - scale down uses default 300s cooldown, within cooldown",
			specReplicas:    10,
			desiredReplicas: 5,
			setupMonitor: func(m *capacityMonitor) {
				m.lastScaleDownAt = time.Now().Add(-10 * time.Second) // within 300s default
			},
			expected: 10, // blocked by default 300s cooldown
		},
		{
			name:            "nil ScaleUp and ScaleDown - scale down uses default 300s cooldown, expired",
			specReplicas:    10,
			desiredReplicas: 5,
			setupMonitor: func(m *capacityMonitor) {
				m.lastScaleDownAt = time.Now().Add(-400 * time.Second) // beyond 300s default
			},
			expected: 5, // allowed
		},
		{
			name:            "nil ScaleUp and ScaleDown - first scale down no cooldown",
			specReplicas:    10,
			desiredReplicas: 5,
			setupMonitor: func(m *capacityMonitor) {
				// lastScaleDownAt is zero - first scale is immediate even with default 300s
			},
			expected: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reconciler{
				monitors: make(map[types.NamespacedName]*capacityMonitor),
			}
			// CapacityPolicy with nil ScaleUp/ScaleDown - triggers default fallback logic
			pa := &agentsv1alpha1.PoolAutoscaler{
				Spec: agentsv1alpha1.PoolAutoscalerSpec{
					CapacityPolicy: &agentsv1alpha1.CapacityPolicy{},
				},
			}
			pa.Name = "test-pa"
			pa.Namespace = "default"

			key := types.NamespacedName{Namespace: pa.Namespace, Name: pa.Name}
			monitor := &capacityMonitor{}
			tt.setupMonitor(monitor)
			r.monitors[key] = monitor

			result := r.applyStabilizationWindow(
				pa, tt.specReplicas, tt.desiredReplicas,
			)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestGetOrCreateMonitor_CreationPath verifies that observeAndAggregate
// creates a capacityMonitor when none exists for the given PoolAutoscaler.
func TestGetOrCreateMonitor_CreationPath(t *testing.T) {
	r := &Reconciler{monitors: make(map[types.NamespacedName]*capacityMonitor)}
	pa := &agentsv1alpha1.PoolAutoscaler{}
	pa.Name = "test-pa"
	pa.Namespace = "default"
	key := types.NamespacedName{Namespace: pa.Namespace, Name: pa.Name}

	// Ensure no monitor exists before the call
	_, exists := r.monitors[key]
	assert.False(t, exists)

	// Call observeAndAggregate - should create monitor internally
	avgAvail, avgStatus := r.observeAndAggregate(context.Background(), pa, 10, 20)

	// With a single sample added, aggregated values equal the raw values
	assert.Equal(t, int32(10), avgAvail)
	assert.Equal(t, int32(20), avgStatus)

	// Monitor was created
	m, ok := r.monitors[key]
	assert.True(t, ok)
	assert.NotNil(t, m)
	assert.Len(t, m.samples, 1) // first sample was added
}

// TestDeleteMonitor verifies that deleteMonitor removes the capacity
// monitor for a given key, is a no-op for non-existent keys, and does
// not affect other monitors.
func TestDeleteMonitor(t *testing.T) {
	t.Run("removes existing monitor", func(t *testing.T) {
		r := &Reconciler{monitors: make(map[types.NamespacedName]*capacityMonitor)}
		key := types.NamespacedName{Namespace: "default", Name: "test-pa"}
		r.monitors[key] = &capacityMonitor{
			samples: []sample{
				{timestamp: time.Now(), available: 5, statusReplicas: 10},
			},
		}

		r.deleteMonitor(key)

		_, exists := r.monitors[key]
		assert.False(t, exists)
	})

	t.Run("no-op when monitor does not exist", func(t *testing.T) {
		r := &Reconciler{monitors: make(map[types.NamespacedName]*capacityMonitor)}
		key := types.NamespacedName{Namespace: "default", Name: "nonexistent"}

		// Should not panic
		r.deleteMonitor(key)

		_, exists := r.monitors[key]
		assert.False(t, exists)
	})

	t.Run("other monitors unaffected", func(t *testing.T) {
		r := &Reconciler{monitors: make(map[types.NamespacedName]*capacityMonitor)}
		key1 := types.NamespacedName{Namespace: "default", Name: "pa-1"}
		key2 := types.NamespacedName{Namespace: "default", Name: "pa-2"}
		r.monitors[key1] = &capacityMonitor{}
		r.monitors[key2] = &capacityMonitor{}

		r.deleteMonitor(key1)

		_, exists1 := r.monitors[key1]
		assert.False(t, exists1)
		_, exists2 := r.monitors[key2]
		assert.True(t, exists2)
	})
}

func int32Ptr(v int32) *int32 {
	return &v
}

// ---------------------------------------------------------------------------
// computeCronDesiredReplicas tests
// ---------------------------------------------------------------------------

func TestComputeCronDesiredReplicas(t *testing.T) {
	utc := "UTC"
	tests := []struct {
		name           string
		cronPolicies   []agentsv1alpha1.CronScalingPolicy
		specReplicas   int32
		now            time.Time
		creationTime   time.Time
		expectReplicas int32
		expectReason   string
		expectError    string
	}{
		{
			name: "cron policy triggers - returns targetReplicas",
			cronPolicies: []agentsv1alpha1.CronScalingPolicy{
				{Name: "scale-up", Schedule: "0 8 * * *", TargetReplicas: 50, TimeZone: &utc},
			},
			specReplicas:   10,
			now:            time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC),
			creationTime:   time.Date(2026, 7, 3, 7, 0, 0, 0, time.UTC),
			expectReplicas: 50,
			expectReason:   "cron policy \"scale-up\" triggered",
			expectError:    "",
		},
		{
			name: "no cron policy triggered - returns specReplicas",
			cronPolicies: []agentsv1alpha1.CronScalingPolicy{
				{Name: "scale-up", Schedule: "0 8 * * *", TargetReplicas: 50, TimeZone: &utc},
			},
			specReplicas:   10,
			now:            time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC),
			creationTime:   time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC), // just created
			expectReplicas: 10,
			expectReason:   "no cron policy has triggered yet",
			expectError:    "",
		},
		{
			name: "error from evaluateCronPolicies - returns specReplicas and error",
			cronPolicies: []agentsv1alpha1.CronScalingPolicy{
				{Name: "bad", Schedule: "INVALID", TargetReplicas: 10, TimeZone: &utc},
			},
			specReplicas:   10,
			now:            time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC),
			creationTime:   time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC),
			expectReplicas: 10,
			expectReason:   "",
			expectError:    "invalid cron expression",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reconciler{}
			// Build initial AppliedCronPolicies from creationTime to simulate existing baseline
			var appliedCron []agentsv1alpha1.CronScalingPolicyStatus
			if !tt.creationTime.IsZero() {
				for _, p := range tt.cronPolicies {
					t := metav1.NewTime(tt.creationTime)
					appliedCron = append(appliedCron, agentsv1alpha1.CronScalingPolicyStatus{Name: p.Name, LastScheduleTime: &t})
				}
			}
			pa := &agentsv1alpha1.PoolAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-pa",
					Namespace:         "default",
					CreationTimestamp: metav1.NewTime(tt.creationTime),
				},
				Spec: agentsv1alpha1.PoolAutoscalerSpec{
					CronPolicies: tt.cronPolicies,
				},
				Status: agentsv1alpha1.PoolAutoscalerStatus{
					AppliedCronPolicies: appliedCron,
				},
			}
			replicas, reason, _, err := r.computeCronDesiredReplicas(context.Background(), pa, tt.specReplicas, tt.now)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Equal(t, tt.expectReplicas, replicas)
				assert.Empty(t, reason)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expectReplicas, replicas)
				assert.Contains(t, reason, tt.expectReason)
			}
		})
	}
}
