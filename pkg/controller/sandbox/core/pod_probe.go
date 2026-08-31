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
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/autopause"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/utils"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

// podProbeItem represents a single probe entry in the kruise.io/podprobe annotation.
// This struct follows the PodProbeMarker Serverless protocol format.
type podProbeItem struct {
	ContainerName    string       `json:"containerName"`
	Name             string       `json:"name"`
	PodConditionType string       `json:"podConditionType"`
	Probe            corev1.Probe `json:"probe"`
}

// PodProbeManager handles all probe-related operations: validation, annotation
// injection during pod creation, and annotation syncing during reconciliation.
// It encapsulates annotation format, validation rules, and patch mechanics so
// callers don't need to know the implementation details.
type PodProbeManager struct {
	client.Client
	recorder record.EventRecorder
}

// NewPodProbeManager creates a new PodProbeManager.
func NewPodProbeManager(cli client.Client, recorder record.EventRecorder) *PodProbeManager {
	return &PodProbeManager{Client: cli, recorder: recorder}
}

// InjectProbe injects probe configurations into the pod during pod creation.
// This is an in-memory operation called before the pod is persisted. If any
// probe is invalid, injection is skipped entirely — the validation error will
// be reported as a Condition by EnsureProbe during the Running phase.
//
// Only the probes are validated here. An invalid AutoPausePolicy leaves the
// probes themselves executable, so it must not stop them from being injected;
// EnsureProbe reports it on the ProbeValid condition instead.
//
// Injection is gated on AutoPauseControllerGate: the gate exists so the whole
// probe feature can be rolled back, and leaving a pod running probes the
// decision loop no longer reads would defeat that.
func (m *PodProbeManager) InjectProbe(ctx context.Context, box *agentsv1alpha1.Sandbox, pod *corev1.Pod) {
	if !probeFeatureEnabled() {
		return
	}
	if errs := validateProbes(box.Spec.Probes); len(errs) > 0 {
		klog.FromContext(ctx).Error(errs.ToAggregate(), "probe validation failed, skipping injection", "sandbox", klog.KObj(box))
		return
	}
	data := buildPodProbeAnnotation(box, pod)
	if data == "" {
		return
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[agentsv1alpha1.AnnotationPodProbe] = data
}

// probeFeatureEnabled reports whether the probe feature is switched on. Both
// pod mutation (InjectProbe, EnsureProbe) and the pause/resume decision loop
// read the same gate, so a rollback stops the feature end to end.
func probeFeatureEnabled() bool {
	return utilfeature.DefaultFeatureGate.Enabled(features.AutoPauseControllerGate)
}

// validate validates probe and auto-pause policy configurations and updates the
// SandboxConditionProbeValid condition. A Warning event is emitted only on the
// first transition to invalid (not on every reconcile) to avoid event spam.
// When the configuration is valid, the condition is set to True (if not already).
//
// The return value reports whether the probes can be applied to the Pod, not
// whether the whole configuration is valid. A policy error does not make the
// probes unusable, and refusing to sync on it would freeze both the Pod
// annotation and the probe conditions — so removing spec.probes while leaving a
// now-dangling policy behind would keep the stale conditions the pause decision
// reads instead of clearing them.
func (m *PodProbeManager) validate(ctx context.Context, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus) bool {
	if len(box.Spec.Probes) == 0 && box.Spec.AutoPausePolicy == nil {
		// Nothing left to validate, so drop the verdict on the configuration that
		// used to be here. Otherwise a ProbeValid=False from an earlier spec stays
		// on the status forever, reporting a failure the user has already fixed by
		// removing the configuration.
		utils.RemoveSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionProbeValid))
		return true
	}
	probeErrs := validateProbes(box.Spec.Probes)
	errs := append(field.ErrorList{}, probeErrs...)
	errs = append(errs, validateAutoPausePolicy(box)...)
	if len(errs) > 0 {
		klog.FromContext(ctx).Error(errs.ToAggregate(), "probe validation failed", "sandbox", klog.KObj(box))
		// Only emit Event on the first transition to invalid, not on every reconcile.
		existingCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionProbeValid))
		if existingCond == nil || existingCond.Status != metav1.ConditionFalse {
			m.recorder.Eventf(box, corev1.EventTypeWarning, "ProbeValidationFailed", "probe validation failed: %v", errs.ToAggregate())
		}
		utils.SetSandboxCondition(newStatus, metav1.Condition{
			Type:               string(agentsv1alpha1.SandboxConditionProbeValid),
			Status:             metav1.ConditionFalse,
			Reason:             agentsv1alpha1.SandboxProbeValidReasonValidationFailed,
			Message:            errs.ToAggregate().Error(),
			LastTransitionTime: metav1.Now(),
		})
		return len(probeErrs) == 0
	}
	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionProbeValid))
	if cond == nil || cond.Status != metav1.ConditionTrue {
		utils.SetSandboxCondition(newStatus, metav1.Condition{
			Type:               string(agentsv1alpha1.SandboxConditionProbeValid),
			Status:             metav1.ConditionTrue,
			Reason:             agentsv1alpha1.SandboxProbeValidReasonValidationPassed,
			Message:            "",
			LastTransitionTime: metav1.Now(),
		})
	}
	return true
}

// EnsureProbe validates probe configurations and makes the sandbox's current
// Spec.Probes take effect on the pod. If the probes themselves are invalid, the
// Condition is set to False and no further action is taken. Otherwise the pod is
// patched (via RawPatch to avoid resourceVersion conflicts) so the runtime picks
// up any changes to Spec.Probes while the sandbox is Running. Finally, probe
// conditions are synced from Pod.Status.Conditions to Sandbox.Status.Conditions.
//
// Like InjectProbe it is gated on AutoPauseControllerGate, so a rollback stops
// touching pods and stops writing probe conditions. Conditions already written
// are left in place: the decision loop is off too, so nothing reads them.
func (m *PodProbeManager) EnsureProbe(ctx context.Context, box *agentsv1alpha1.Sandbox, pod *corev1.Pod, newStatus *agentsv1alpha1.SandboxStatus) error {
	if !probeFeatureEnabled() {
		return nil
	}
	unclaimed := isUnclaimedPoolSandbox(box)
	if unclaimed {
		// Clear result state before validation or Pod patching so even an error
		// cannot retain data from the previous claim on a warm-pool Sandbox.
		clearProbeResults(newStatus)
	}
	if !m.validate(ctx, box, newStatus) {
		return nil
	}

	expected := buildPodProbeAnnotation(box, pod)
	current := ""
	if pod.Annotations != nil {
		current = pod.Annotations[agentsv1alpha1.AnnotationPodProbe]
	}
	// If annotation doesn't match, patch the pod so the runtime picks up changes.
	if expected != current {
		// Build a minimal JSON merge patch targeting only the annotation key.
		// Using RawPatch avoids resourceVersion conflicts that can occur with
		// MergeFrom when the pod has been updated by other controllers (e.g. kubelet).
		var annotations map[string]interface{}
		if expected == "" {
			annotations = map[string]interface{}{agentsv1alpha1.AnnotationPodProbe: nil}
		} else {
			annotations = map[string]interface{}{agentsv1alpha1.AnnotationPodProbe: expected}
		}
		patchMap := map[string]interface{}{
			"metadata": map[string]interface{}{
				"annotations": annotations,
			},
		}
		by, _ := json.Marshal(patchMap)
		rcvObject := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: pod.Name}}
		if err := m.Patch(ctx, rcvObject, client.RawPatch(types.MergePatchType, by)); err != nil {
			return fmt.Errorf("failed to patch pod probe annotation: %w", err)
		}
		// Update local copy so subsequent logic sees the change
		pod.Annotations = rcvObject.Annotations
		klog.FromContext(ctx).Info("ensured pod probe", "sandbox", klog.KObj(box), "pod", klog.KObj(pod))
	}

	// Unclaimed pool sandboxes run probes to stay warm, but their results must
	// not become effective before claim. Wait to mirror the current Pod results
	// until claimed; result state was cleared before operations that can fail.
	if unclaimed {
		return nil
	}

	// Sync probe conditions from Pod to Sandbox (handles add/update/remove).
	m.syncConditions(box, pod, newStatus)
	return nil
}

// syncConditions synchronizes probe-related Conditions between Pod and Sandbox.
// Three cases are handled:
//  1. New probe added (or pod condition not yet available): set Unknown condition
//     so consumers know the probe is pending.
//  2. Probe removed from spec: remove the corresponding condition.
//  3. Normal case: sync the pod condition (status/reason/message) to sandbox.
func (m *PodProbeManager) syncConditions(box *agentsv1alpha1.Sandbox, pod *corev1.Pod, newStatus *agentsv1alpha1.SandboxStatus) {
	expectedConds := make(map[string]bool)
	for _, probe := range box.Spec.Probes {
		condType := agentsv1alpha1.ProbeConditionType(probe.Name)
		expectedConds[condType] = true

		podCond := findPodCondition(pod, condType)
		if podCond == nil {
			// Case 1: New probe — pod condition not yet available, set Unknown if not already set.
			existing := utils.GetSandboxCondition(newStatus, condType)
			if existing == nil {
				utils.SetSandboxCondition(newStatus, metav1.Condition{
					Type:               condType,
					Status:             metav1.ConditionUnknown,
					Reason:             agentsv1alpha1.ProbeReasonPending,
					Message:            "probe result not yet available",
					LastTransitionTime: metav1.Now(),
				})
			}
			continue
		}

		// Case 3: Normal sync from pod condition. For a claimed pool Sandbox,
		// clamp the first effective transition to claim time so probe history
		// accumulated while warming cannot consume the idle threshold.
		existing := utils.GetSandboxCondition(newStatus, condType)
		transition := probeTransitionTime(existing, podCond)
		transition = effectiveProbeTransitionTime(box, existing, transition)
		// SetSandboxCondition is idempotent — it skips if status/reason/message all match.
		utils.SetSandboxCondition(newStatus, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionStatus(podCond.Status),
			Reason:             probeConditionReason(podCond),
			Message:            podCond.Message,
			LastTransitionTime: transition,
		})
		// Probe scripts keep Status=True and report their result in Message, so a
		// message-only transition must still advance LastTransitionTime for
		// auto-pause thresholds to measure from the right moment.
		// SetSandboxCondition refreshes the timestamp only on a Status change, so
		// mirror the Pod condition here instead.
		if cond := utils.GetSandboxCondition(newStatus, condType); cond != nil {
			cond.LastTransitionTime = transition
		}
	}

	// Case 2: Remove conditions for probes no longer in spec.
	var toRemove []string
	for _, cond := range newStatus.Conditions {
		if strings.HasPrefix(cond.Type, agentsv1alpha1.ProbeConditionPrefix) && !expectedConds[cond.Type] {
			toRemove = append(toRemove, cond.Type)
		}
	}
	for _, condType := range toRemove {
		utils.RemoveSandboxCondition(newStatus, condType)
	}
}

// probeTransitionTime resolves the LastTransitionTime to record for a probe
// condition. PodCondition.LastTransitionTime is omitempty while the Sandbox
// condition schema requires a non-zero timestamp, so a missing pod timestamp has
// to be substituted.
//
// The substitute must not be the current time on every reconcile. Auto-pause
// measures its idle threshold from this timestamp, so a timestamp that keeps
// advancing keeps the threshold permanently in the future and auto-pause never
// fires — silently, with no error, Event, or log. It would also defeat the
// status DeepEqual check and emit a status patch on every reconcile. So keep the
// timestamp already recorded and only take the current time when the probe
// reports a result different from the recorded one, which is the moment a
// transition actually happened.
func probeTransitionTime(existing *metav1.Condition, podCond *corev1.PodCondition) metav1.Time {
	reason := probeConditionReason(podCond)
	unchanged := existing != nil &&
		existing.Status == metav1.ConditionStatus(podCond.Status) &&
		existing.Reason == reason &&
		existing.Message == podCond.Message
	if unchanged && !existing.LastTransitionTime.IsZero() {
		return existing.LastTransitionTime
	}

	if existing != nil {
		// Condition producers are only required to advance LastTransitionTime
		// when Status changes. Prefer a newer producer timestamp when available;
		// otherwise timestamp a message/reason-only transition when we observe it.
		if !podCond.LastTransitionTime.IsZero() && podCond.LastTransitionTime.After(existing.LastTransitionTime.Time) {
			return podCond.LastTransitionTime
		}
		return metav1.Now()
	}
	if !podCond.LastTransitionTime.IsZero() {
		return podCond.LastTransitionTime
	}
	return metav1.Now()
}

// effectiveProbeTransitionTime prevents probe history accumulated while a
// Sandbox was warming in a pool from consuming its idle threshold immediately
// after claim. AnnotationClaimTime is written atomically with the claimed label.
func effectiveProbeTransitionTime(box *agentsv1alpha1.Sandbox, existing *metav1.Condition, transition metav1.Time) metav1.Time {
	if box.Labels[agentsv1alpha1.LabelSandboxIsClaimed] != agentsv1alpha1.True {
		return transition
	}
	claimedAt, err := time.Parse(time.RFC3339, box.Annotations[agentsv1alpha1.AnnotationClaimTime])
	if err != nil {
		// A claimed pool Sandbox without trustworthy claim metadata must not
		// inherit warm-up history. Start at first observation and then preserve it.
		if existing != nil && !existing.LastTransitionTime.IsZero() {
			return existing.LastTransitionTime
		}
		return metav1.Now()
	}
	claimTime := metav1.NewTime(claimedAt)
	if !transition.Before(&claimTime) {
		return transition
	}
	return claimTime
}

func isUnclaimedPoolSandbox(box *agentsv1alpha1.Sandbox) bool {
	return box.Labels[agentsv1alpha1.LabelSandboxIsClaimed] == agentsv1alpha1.False
}

func clearProbeResults(status *agentsv1alpha1.SandboxStatus) {
	var toRemove []string
	for _, cond := range status.Conditions {
		if strings.HasPrefix(cond.Type, agentsv1alpha1.ProbeConditionPrefix) {
			toRemove = append(toRemove, cond.Type)
		}
	}
	for _, condType := range toRemove {
		utils.RemoveSandboxCondition(status, condType)
	}
	for i := range status.Schedules {
		status.Schedules[i].NextPauseTime = nil
		status.Schedules[i].NextResumeTime = nil
	}
}

// probeConditionReason returns a non-empty Reason for a probe condition. The
// kruise PodProbeMarker leaves PodCondition.Reason empty, but the Sandbox
// condition schema requires a non-empty reason, and a single invalid entry makes
// the apiserver reject the whole status patch.
func probeConditionReason(podCond *corev1.PodCondition) string {
	if podCond.Reason != "" {
		return podCond.Reason
	}
	switch podCond.Status {
	case corev1.ConditionTrue:
		return agentsv1alpha1.ProbeReasonSucceeded
	case corev1.ConditionFalse:
		return agentsv1alpha1.ProbeReasonError
	default:
		return agentsv1alpha1.ProbeReasonPending
	}
}

// findPodCondition finds a condition by type in Pod.Status.Conditions.
func findPodCondition(pod *corev1.Pod, condType string) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if string(pod.Status.Conditions[i].Type) == condType {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

// --- internal helpers ---

// validateProbes validates each probe in the spec using K8s field validation.
// Currently only the Exec probe handler is supported; HTTPGet, TCPSocket,
// and GRPC handlers are rejected.
//
// The rules live in pkg/autopause so the webhooks that admit SandboxSet and
// SandboxTemplate reject the same configurations this controller would only
// report on the ProbeValid condition.
func validateProbes(probes []agentsv1alpha1.Probe) field.ErrorList {
	return autopause.ValidateProbes(probes, field.NewPath("spec", "probes"))
}

// validateAutoPausePolicy validates the auto-pause policy against the probes it
// references. Sandboxes created directly have no validating webhook, so the
// controller is the only place a rule pointing at an undefined probe surfaces.
func validateAutoPausePolicy(box *agentsv1alpha1.Sandbox) field.ErrorList {
	return autopause.ValidateAutoPausePolicy(box.Spec.AutoPausePolicy, box.Spec.Probes, field.NewPath("spec", "autoPausePolicy"))
}

// buildPodProbeAnnotation builds the kruise.io/podprobe annotation value from
// Sandbox.Spec.Probes. The caller is responsible for validating probes before
// calling this function. Returns empty string if no probes are configured.
func buildPodProbeAnnotation(box *agentsv1alpha1.Sandbox, pod *corev1.Pod) string {
	if len(box.Spec.Probes) == 0 {
		return ""
	}

	// Determine default container name (first container in the pod)
	defaultContainer := ""
	if len(pod.Spec.Containers) > 0 {
		defaultContainer = pod.Spec.Containers[0].Name
	}

	items := make([]podProbeItem, 0, len(box.Spec.Probes))
	for i := range box.Spec.Probes {
		probe := &box.Spec.Probes[i]
		containerName := probe.ContainerName
		if containerName == "" {
			containerName = defaultContainer
		}
		items = append(items, podProbeItem{
			ContainerName:    containerName,
			Name:             probe.Name,
			PodConditionType: agentsv1alpha1.ProbeConditionType(probe.Name),
			Probe:            probe.Probe,
		})
	}

	data, _ := json.Marshal(items)
	return string(data)
}
