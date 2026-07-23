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
	"fmt"
	"math"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

const (
	defaultTolerancePercent       = 10
	defaultScaleDownStabilization = 300 // seconds
	defaultScaleUpStabilization   = 0   // seconds
)

// Observation window parameters — set once via command-line flags at startup
// and read-only afterwards. NOT safe for concurrent modification at runtime.
var (
	observationWindowSeconds = 15
	samplingIntervalSeconds  = 5
)

// sample records a single observation of available and status replicas.
type sample struct {
	timestamp      time.Time
	available      int32
	statusReplicas int32
}

// capacityMonitor tracks sustained-condition timers for a single PoolAutoscaler.
type capacityMonitor struct {
	mu sync.Mutex

	// Observation window samples. Continuously maintained — NOT cleared
	// after scaling. Each sample captures (available, statusReplicas)
	// at a point in time, collected at samplingInterval cadence.
	samples      []sample
	lastSampleAt time.Time

	// Cooldown timestamps: last time a scale operation was executed
	// in each direction. Zero means never scaled in that direction
	// (no cooldown — first scale is immediate).
	lastScaleUpAt   time.Time
	lastScaleDownAt time.Time
}

// recordScale sets the cooldown timestamp for the given direction.
// Called after a scale operation completes. Does NOT clear samples —
// the observation window is continuously maintained.
func (m *capacityMonitor) recordScale(scaleUp bool, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if scaleUp {
		m.lastScaleUpAt = now
	} else {
		m.lastScaleDownAt = now
	}
}

// addSampleIfDue adds a new sample if the sampling interval has elapsed.
// Returns true if a sample was added.
func (m *capacityMonitor) addSampleIfDue(available, statusReplicas int32, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	interval := time.Duration(samplingIntervalSeconds) * time.Second
	if !m.lastSampleAt.IsZero() && now.Sub(m.lastSampleAt) < interval {
		return false
	}
	// Pre-allocate capacity on first use
	if cap(m.samples) == 0 {
		maxSamples := observationWindowSeconds/samplingIntervalSeconds + 1
		m.samples = make([]sample, 0, maxSamples)
	}
	m.samples = append(m.samples, sample{
		timestamp:      now,
		available:      available,
		statusReplicas: statusReplicas,
	})
	m.lastSampleAt = now
	return true
}

// pruneSamples removes samples older than the observation window.
func (m *capacityMonitor) pruneSamples(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	window := time.Duration(observationWindowSeconds) * time.Second
	cutoff := now.Add(-window)
	idx := 0
	for i, s := range m.samples {
		if s.timestamp.After(cutoff) {
			idx = i
			break
		}
		idx = i + 1
	}
	if idx > 0 {
		m.samples = m.samples[idx:]
	}
}

// aggregatedValues returns the average available and statusReplicas
// from samples within the observation window, along with the sample count.
// Returns ok=false if no samples exist.
func (m *capacityMonitor) aggregatedValues() (avgAvailable, avgReplicas int32, sampleCount int, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.samples)
	if n == 0 {
		return 0, 0, 0, false
	}
	var sumAvailable, sumReplicas int64
	for _, s := range m.samples {
		sumAvailable += int64(s.available)
		sumReplicas += int64(s.statusReplicas)
	}
	return int32(math.Round(float64(sumAvailable) / float64(n))), int32(math.Round(float64(sumReplicas) / float64(n))), n, true
}

// getLastSampleAt returns the time of the last sample in a thread-safe manner.
func (m *capacityMonitor) getLastSampleAt() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastSampleAt
}

// computeDesiredReplicas calculates the desired spec.replicas for the SandboxSet.
//
// Key insight: SandboxSet.Spec.Replicas controls the number of *unclaimed* sandboxes.
// When a sandbox is claimed, it leaves the SandboxSet's scope and the SandboxSet
// auto-creates a replacement. So we only need to decide the pool size — the
// SandboxSet handles replenishment after claims.
//
// The formula avoids conflating "creating" (not yet ready) pods with "used" (claimed)
// pods, which was the source of oscillation in the original implementation.

// computeDesiredReplicasResult holds the output of computeDesiredReplicas.
type computeDesiredReplicasResult struct {
	desiredReplicas     int32
	reason              string
	appliedCronPolicies []agentsv1alpha1.CronScalingPolicyStatus
	cronTriggered       bool // true when a cron policy determined the desired replicas
}

func (r *Reconciler) computeDesiredReplicas(ctx context.Context, pa *agentsv1alpha1.PoolAutoscaler, specReplicas, statusReplicas, available int32) (computeDesiredReplicasResult, error) {
	logger := klog.FromContext(ctx)

	// Evaluate cron policies first — cron takes priority over capacity when triggered.
	if len(pa.Spec.CronPolicies) > 0 {
		desired, reason, cronStatuses, err := r.computeCronDesiredReplicas(ctx, pa, specReplicas, time.Now())
		if err != nil {
			return computeDesiredReplicasResult{}, err
		}
		// If a cron policy has triggered, use its targetReplicas directly.
		if reason != ReasonNoCronTriggered {
			logger.V(3).Info("Cron policy triggered, overriding capacity", "reason", reason, "desired", desired)
			return computeDesiredReplicasResult{desired, reason, cronStatuses, true}, nil
		}
	}

	if pa.Spec.CapacityPolicy == nil {
		return computeDesiredReplicasResult{specReplicas, "no scaling policy", nil, false}, nil
	}

	// Use statusReplicas as the percentage base.
	// Combined with the SandboxSet watch, the autoscaler reacts immediately
	// when available drops after claims.
	targetAvailable, lowerWatermark, upperWatermark := computeWatermarks(
		pa.Spec.CapacityPolicy.TargetAvailable,
		pa.Spec.CapacityPolicy.Tolerance,
		statusReplicas,
	)

	logger.V(5).Info("Capacity policy evaluation",
		"specReplicas", specReplicas,
		"statusReplicas", statusReplicas,
		"available", available,
		"targetAvailable", targetAvailable,
		"lowerWatermark", lowerWatermark,
		"upperWatermark", upperWatermark,
	)

	// Scale up: available dropped below lower watermark.
	// desired = statusReplicas + deficit
	// Guard: don't scale up further while a previous scale-up is still in progress
	// (pods are being created). This prevents runaway feedback loops.
	if available < lowerWatermark {
		if specReplicas > statusReplicas {
			return computeDesiredReplicasResult{specReplicas, "waiting for previous scale-up to complete", nil, false}, nil
		}
		deficit := targetAvailable - available
		desired := statusReplicas + deficit
		if desired < 0 {
			desired = 0
		}
		return computeDesiredReplicasResult{desired, "available below lower watermark", nil, false}, nil
	}

	// Scale down: available exceeded upper watermark.
	// desired = statusReplicas - excess
	if available > upperWatermark {
		excess := available - targetAvailable
		desired := statusReplicas - excess
		if desired < 0 {
			desired = 0
		}
		return computeDesiredReplicasResult{desired, "available above upper watermark", nil, false}, nil
	}

	// Within dead zone [lower, upper] — stable, no change.
	return computeDesiredReplicasResult{specReplicas, "within tolerance", nil, false}, nil
}

// computeCronDesiredReplicas evaluates cron policies and returns the desired replicas
// along with the applied cron policy statuses for status reporting.
func (r *Reconciler) computeCronDesiredReplicas(ctx context.Context, pa *agentsv1alpha1.PoolAutoscaler, specReplicas int32, now time.Time) (int32, string, []agentsv1alpha1.CronScalingPolicyStatus, error) {
	targetReplicas, reason, appliedStatuses, err := evaluateCronPolicies(
		pa.Spec.CronPolicies, now, pa.Status.AppliedCronPolicies,
	)
	if err != nil {
		return specReplicas, "", nil, err
	}

	if reason == ReasonNoCronTriggered {
		return specReplicas, reason, appliedStatuses, nil
	}

	return targetReplicas, reason, appliedStatuses, nil
}

// applyStabilizationWindow checks whether the cooldown period has elapsed
// since the last scale operation. Scaling only proceeds when the cooldown
// has expired (or when no prior scale has occurred).
//
// This is a cooldown model, NOT a sustained-condition model:
//   - First scale is immediate (no cooldown).
//   - After ANY scale action (up or down), the opposite direction cannot
//     scale until its stabilizationWindowSeconds have elapsed since that action.
//   - This prevents thrashing: a cron scale-up won't be immediately undone
//     by capacity scale-down.
//   - The observation window samples are NOT cleared after scaling.
//
// cooldownExpired checks if enough time has elapsed since the last scale action.
// Returns true if scaling is allowed (cooldown expired or first-time scale).
func cooldownExpired(lastScaleAt time.Time, windowSeconds int32, now time.Time) bool {
	if windowSeconds == 0 {
		return true
	}
	if lastScaleAt.IsZero() {
		return true // first scale, no cooldown
	}
	return now.Sub(lastScaleAt) >= time.Duration(windowSeconds)*time.Second
}

func (r *Reconciler) applyStabilizationWindow(pa *agentsv1alpha1.PoolAutoscaler, specReplicas, desiredReplicas int32) int32 {
	if desiredReplicas == specReplicas {
		return desiredReplicas
	}

	key := types.NamespacedName{Namespace: pa.Namespace, Name: pa.Name}
	monitor := r.getOrCreateMonitor(key)
	now := time.Now()

	monitor.mu.Lock()
	defer monitor.mu.Unlock()

	// Compute the most recent scale action time (either direction).
	lastScaleAt := monitor.lastScaleUpAt
	if monitor.lastScaleDownAt.After(lastScaleAt) {
		lastScaleAt = monitor.lastScaleDownAt
	}

	if desiredReplicas > specReplicas {
		// Scale up — check cooldown since last ANY scale action
		windowSeconds := int32(defaultScaleUpStabilization)
		if pa.Spec.CapacityPolicy != nil && pa.Spec.CapacityPolicy.ScaleUp != nil &&
			pa.Spec.CapacityPolicy.ScaleUp.StabilizationWindowSeconds != nil {
			windowSeconds = *pa.Spec.CapacityPolicy.ScaleUp.StabilizationWindowSeconds
		}
		if cooldownExpired(lastScaleAt, windowSeconds, now) {
			return desiredReplicas
		}
		return specReplicas // in cooldown
	}

	// desiredReplicas < specReplicas — scale down, check cooldown since last ANY scale action
	windowSeconds := int32(defaultScaleDownStabilization)
	if pa.Spec.CapacityPolicy != nil && pa.Spec.CapacityPolicy.ScaleDown != nil &&
		pa.Spec.CapacityPolicy.ScaleDown.StabilizationWindowSeconds != nil {
		windowSeconds = *pa.Spec.CapacityPolicy.ScaleDown.StabilizationWindowSeconds
	}
	if cooldownExpired(lastScaleAt, windowSeconds, now) {
		return desiredReplicas
	}
	return specReplicas // in cooldown
}

// observeAndAggregate records a sample (if sampling interval has elapsed)
// and returns the aggregated (averaged) available and statusReplicas values
// from samples within the observation window.
//
// When no samples exist yet (e.g., after controller restart), returns the
// raw instantaneous values as fallback — equivalent to the behavior without
// observation window.
//
// The aggregated values are passed to computeDesiredReplicas as if they were
// instantaneous values. computeDesiredReplicas does not change.
func (r *Reconciler) observeAndAggregate(
	ctx context.Context,
	pa *agentsv1alpha1.PoolAutoscaler,
	rawAvailable, rawStatusReplicas int32,
) (avgAvailable, avgReplicas int32) {
	logger := klog.FromContext(ctx)

	key := types.NamespacedName{Namespace: pa.Namespace, Name: pa.Name}
	monitor := r.getOrCreateMonitor(key)
	now := time.Now()

	monitor.addSampleIfDue(rawAvailable, rawStatusReplicas, now)
	monitor.pruneSamples(now)

	avgAvail, avgStatus, sampleCount, ok := monitor.aggregatedValues()
	if !ok {
		// Warm-up fallback: no samples, use raw values
		return rawAvailable, rawStatusReplicas
	}

	logger.V(3).Info("Observation window aggregation",
		"rawAvailable", rawAvailable,
		"rawStatusReplicas", rawStatusReplicas,
		"avgAvailable", avgAvail,
		"avgReplicas", avgStatus,
		"sampleCount", sampleCount,
	)
	return avgAvail, avgStatus
}

// getOrCreateMonitor returns or creates the capacity monitor for the given key.
func (r *Reconciler) getOrCreateMonitor(key types.NamespacedName) *capacityMonitor {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.monitors[key]; ok {
		return m
	}
	m := &capacityMonitor{}
	r.monitors[key] = m
	return m
}

// deleteMonitor removes the capacity monitor for the given key.
func (r *Reconciler) deleteMonitor(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.monitors, key)
}

// recordScaleAction sets the cooldown timestamp after a scale operation.
// Does NOT clear observation window samples.
func (r *Reconciler) recordScaleAction(key types.NamespacedName, scaleUp bool) {
	r.mu.Lock()
	m, ok := r.monitors[key]
	r.mu.Unlock()
	if ok {
		m.recordScale(scaleUp, time.Now())
	}
}

// computeWatermarks computes targetAvailable, lower and upper watermarks.
//
// For percentage-based configs, the proposal requires combining the percentages
// BEFORE applying to the base and rounding up:
//
//	Lower = ceil(base × (targetPercent - tolerancePercent))
//	Upper = ceil(base × (targetPercent + tolerancePercent))
//
// NOT: ceil(base × targetPercent) - ceil(base × tolerancePercent),
// which produces different results due to ceiling rounding.
func computeWatermarks(targetVal intstr.IntOrString, toleranceVal *intstr.IntOrString, base int32) (target, lower, upper int32) {
	toleranceWithDefault := defaultToleranceForType(targetVal, toleranceVal)

	if targetVal.Type == intstr.String && toleranceWithDefault.Type == intstr.String {
		// Both are percentages: combine before applying to base
		targetPct := parsePercentValue(targetVal)
		tolerancePct := parsePercentValue(toleranceWithDefault)

		target = int32(math.Ceil(float64(base) * targetPct / 100.0))
		lower = int32(math.Ceil(float64(base) * (targetPct - tolerancePct) / 100.0))
		upper = int32(math.Ceil(float64(base) * (targetPct + tolerancePct) / 100.0))
	} else {
		// Absolute values (or mixed): resolve independently then combine
		target = resolveIntOrPercent(targetVal, base)
		tol := resolveIntOrPercent(toleranceWithDefault, base)
		lower = target - tol
		upper = target + tol
	}

	if lower < 0 {
		lower = 0
	}
	return target, lower, upper
}

// defaultToleranceForType returns the configured tolerance, or a default that
// matches the type of targetAvailable (percentage default for percentage target,
// absolute default for absolute target).
func defaultToleranceForType(targetVal intstr.IntOrString, tolerance *intstr.IntOrString) intstr.IntOrString {
	if tolerance != nil {
		return *tolerance
	}
	// Default tolerance: 10% (as percentage) when target is percentage,
	// otherwise resolve 10% of total as absolute (handled by caller).
	return intstr.FromString(fmt.Sprintf("%d%%", defaultTolerancePercent))
}

// parsePercentValue extracts the numeric portion from a percentage IntOrString (e.g., "70%" → 70).
func parsePercentValue(val intstr.IntOrString) float64 {
	p, _ := intstr.GetScaledValueFromIntOrPercent(&val, 100, false)
	return float64(p)
}

// resolveIntOrPercent resolves an IntOrString value to an absolute int32.
// If the value is a percentage, it is computed relative to `total` and rounded up.
func resolveIntOrPercent(val intstr.IntOrString, total int32) int32 {
	if val.Type == intstr.Int {
		return val.IntVal
	}
	percent, _ := intstr.GetScaledValueFromIntOrPercent(&val, int(total), true)
	return int32(percent)
}
