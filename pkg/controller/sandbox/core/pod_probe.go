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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/autopause"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/utils"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
	kruiseappsv1alpha1 "github.com/openkruise/kruise-api/apps/v1alpha1"
)

const (
	virtualKubeletNodeLabelKey   = autopause.VirtualKubeletNodeLabelKey
	virtualKubeletNodeLabelValue = autopause.VirtualKubeletNodeLabelValue
)

var podGVK = schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}

// podProbeItem represents a single probe entry in the kruise.io/podprobe annotation.
// This struct follows the PodProbeMarker Serverless protocol format.
type podProbeItem struct {
	ContainerName    string       `json:"containerName"`
	Name             string       `json:"name"`
	PodConditionType string       `json:"podConditionType"`
	Probe            corev1.Probe `json:"probe"`
}

// PodProbeManager validates probe configuration, injects probes during pod
// creation, and syncs probe conditions during reconciliation. Real nodes are
// served through a PodProbeMarker, virtual nodes through the kruise.io/podprobe
// annotation.
type PodProbeManager struct {
	client.Client
	recorder record.EventRecorder
}

// NewPodProbeManager creates a new PodProbeManager.
func NewPodProbeManager(cli client.Client, recorder record.EventRecorder) *PodProbeManager {
	return &PodProbeManager{Client: cli, recorder: recorder}
}

// InjectProbe writes the kruise.io/podprobe annotation before the pod is
// persisted. Serverless platforms consume it at pod creation and never re-read a
// later patch, so it cannot be added once the pod runs; it is skipped only when
// the pod's hard constraints exclude virtual-kubelet nodes, where the
// PodProbeMarker delivers probes instead.
//
// Only the probes are validated here: an unsupported probe must never reach the
// annotation, while an invalid AutoPausePolicy leaves the probes executable and
// is reported by EnsureProbe on the ProbeValid condition instead.
func (m *PodProbeManager) InjectProbe(ctx context.Context, box *agentsv1alpha1.Sandbox, pod *corev1.Pod) {
	if !probeFeatureEnabled() || len(box.Spec.Probes) == 0 {
		return
	}
	ctx = klog.NewContext(ctx, klog.FromContext(ctx).WithValues("sandbox", klog.KObj(box)))
	if errs := validateProbes(box.Spec.Probes); len(errs) > 0 {
		klog.FromContext(ctx).Error(errs.ToAggregate(), "probe validation failed, skipping injection")
		return
	}
	if m.isPodExcludedFromVirtualKubelet(ctx, pod) {
		return
	}
	expected := buildPodProbeAnnotation(box, pod)
	if expected == "" {
		return
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[agentsv1alpha1.AnnotationPodProbe] = expected
}

// isPodExcludedFromVirtualKubelet reports whether the pod's hard constraints
// exclude virtual-kubelet nodes, so the PodProbeMarker covers its placement and
// the annotation can be skipped. A bound pod is resolved from its node; for an
// unbound one only type=virtual-kubelet constraints count, since unrelated
// labels cannot prove a virtual node is impossible and preferred affinity is not
// required. Anything invalid or ambiguous keeps the annotation: a missed one is
// unrecoverable on a serverless platform, an extra one is inert on a real node.
func (m *PodProbeManager) isPodExcludedFromVirtualKubelet(ctx context.Context, pod *corev1.Pod) bool {
	if pod.Spec.NodeName != "" {
		virtual, err := m.isVirtualNode(ctx, pod.Spec.NodeName)
		if err != nil {
			klog.FromContext(ctx).Error(err, "failed to check node type, injecting pod probe annotation")
			return false
		}
		return !virtual
	}

	required := requiredNodeAffinity(pod)
	if required != nil {
		if _, err := nodeaffinity.NewNodeSelector(required); err != nil {
			klog.FromContext(ctx).Error(err, "invalid node affinity, injecting pod probe annotation")
			return false
		}
		if len(required.NodeSelectorTerms) == 0 {
			return false
		}
	}

	if nodeType, constrained := pod.Spec.NodeSelector[virtualKubeletNodeLabelKey]; constrained && nodeType != virtualKubeletNodeLabelValue {
		return true
	}
	return required != nil && !requiredNodeAffinityMayMatchVirtualKubelet(required)
}

// requiredNodeAffinityMayMatchVirtualKubelet reports whether any required node
// affinity term can match a virtual-kubelet node. Terms are OR-ed, so every term
// must exclude the virtual-kubelet label before the annotation can be skipped.
func requiredNodeAffinityMayMatchVirtualKubelet(required *corev1.NodeSelector) bool {
	for _, term := range required.NodeSelectorTerms {
		if nodeSelectorTermMayMatchVirtualKubelet(term) {
			return true
		}
	}
	return false
}

// nodeSelectorTermMayMatchVirtualKubelet evaluates only requirements on the
// virtual-kubelet label. Other requirements, including MatchFields, cannot prove
// that a virtual node is impossible and are therefore treated as compatible.
func nodeSelectorTermMayMatchVirtualKubelet(term corev1.NodeSelectorTerm) bool {
	for _, requirement := range term.MatchExpressions {
		if requirement.Key != virtualKubeletNodeLabelKey {
			continue
		}

		hasVirtualKubeletValue := false
		for _, value := range requirement.Values {
			if value == virtualKubeletNodeLabelValue {
				hasVirtualKubeletValue = true
				break
			}
		}

		switch requirement.Operator {
		case corev1.NodeSelectorOpIn:
			if !hasVirtualKubeletValue {
				return false
			}
		case corev1.NodeSelectorOpNotIn:
			if hasVirtualKubeletValue {
				return false
			}
		case corev1.NodeSelectorOpDoesNotExist, corev1.NodeSelectorOpGt, corev1.NodeSelectorOpLt:
			return false
		case corev1.NodeSelectorOpExists:
		default:
			return true
		}
	}
	return true
}

// requiredNodeAffinity returns the pod's hard node affinity, or nil when the pod
// does not constrain its node placement.
func requiredNodeAffinity(pod *corev1.Pod) *corev1.NodeSelector {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return nil
	}
	return pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
}

// probeFeatureEnabled reports whether the probe feature is switched on. Both
// pod mutation (InjectProbe, EnsureProbe) and the pause/resume decision loop
// read the same gate, so a rollback stops the feature end to end.
func probeFeatureEnabled() bool {
	return utilfeature.DefaultFeatureGate.Enabled(features.AutoPauseControllerGate)
}

// kruiseIntegrationEnabled reports whether OpenKruise integration is on. Off
// means no PodProbeMarker API access at all, so the controller still runs on
// clusters without OpenKruise installed.
func kruiseIntegrationEnabled() bool {
	return utilfeature.DefaultFeatureGate.Enabled(features.KruiseIntegrationGate)
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
		klog.FromContext(ctx).Error(errs.ToAggregate(), "probe validation failed")
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

// EnsureProbe validates the probe configuration, makes Spec.Probes take effect
// on the pod, and mirrors Pod.Status.Conditions to Sandbox.Status.Conditions. A
// real node gets a PodProbeMarker; a virtual node gets nothing written, because
// the platform does not re-read the creation-time annotation, so drift against it
// is only logged.
//
// Gated on AutoPauseControllerGate like InjectProbe, and real-node delivery
// additionally on KruiseIntegrationGate. Turning a gate off stops all
// PodProbeMarker access and leaves existing markers running until their owning
// pod is deleted.
func (m *PodProbeManager) EnsureProbe(ctx context.Context, box *agentsv1alpha1.Sandbox, pod *corev1.Pod, newStatus *agentsv1alpha1.SandboxStatus) error {
	if !probeFeatureEnabled() {
		return nil
	}
	ctx = klog.NewContext(ctx, klog.FromContext(ctx).WithValues("sandbox", klog.KObj(box)))
	unclaimed := isUnclaimedPoolSandbox(box)
	if unclaimed {
		// Clear result state before validation or any write so even an error
		// cannot retain data from the previous claim on a warm-pool Sandbox.
		clearProbeResults(newStatus)
	}
	if !m.validate(ctx, box, newStatus) {
		return nil
	}

	// Pod must be scheduled before we can determine the probe delivery mechanism.
	if pod.Spec.NodeName == "" {
		return nil
	}

	virtual, err := m.isVirtualNode(ctx, pod.Spec.NodeName)
	if err != nil {
		return fmt.Errorf("failed to check node type: %w", err)
	}

	if virtual {
		logPodProbeAnnotationDrift(ctx, box, pod)
	} else {
		if err := m.ensurePodProbeMarker(ctx, box, pod); err != nil {
			return err
		}
		// TODO: the creation-time annotation is inert on a real node — only the
		// serverless platform reads it — and stripping it would cost a Patch per
		// pod. Remove it if a concrete need arises.
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

// logPodProbeAnnotationDrift logs a kruise.io/podprobe annotation that no longer
// matches Spec.Probes instead of rewriting it: the platform consumed the
// annotation at pod creation and will not re-read a patch, so applying the change
// needs a new pod.
func logPodProbeAnnotationDrift(ctx context.Context, box *agentsv1alpha1.Sandbox, pod *corev1.Pod) {
	expected := buildPodProbeAnnotation(box, pod)
	current := ""
	if pod.Annotations != nil {
		current = pod.Annotations[agentsv1alpha1.AnnotationPodProbe]
	}
	if expected == current {
		return
	}
	klog.FromContext(ctx).Info("pod probe annotation does not match spec.probes and cannot be updated after pod creation; recreate the pod to apply the change")
}

// reportKruiseCRDMissing logs and emits a Warning event for an unavailable
// PodProbeMarker CRD, and still returns the error so the reconcile keeps
// retrying. KruiseIntegrationGate is the operator's declaration that the cluster
// runs OpenKruise, so a missing CRD is a misconfiguration rather than a transient
// failure, and silently skipping delivery would leave a sandbox that looks
// healthy and never pauses. The constant message lets the recorder aggregate
// retries into one event.
func (m *PodProbeManager) reportKruiseCRDMissing(ctx context.Context, box *agentsv1alpha1.Sandbox, err error) error {
	klog.FromContext(ctx).Error(err, "PodProbeMarker CRD is not available, real-node probes cannot be delivered")
	m.recorder.Event(box, corev1.EventTypeWarning, "PodProbeMarkerCRDMissing",
		"PodProbeMarker CRD is not available, real-node probes cannot be delivered; install OpenKruise or disable the KruiseIntegration feature gate")
	return fmt.Errorf("PodProbeMarker CRD is not available: %w", err)
}

// ensurePodProbeMarker creates or updates the sandbox's PodProbeMarker, owned by
// the pod so Kubernetes GC deletes it when the pod goes away (pause/terminate).
// It is a no-op unless KruiseIntegrationGate is enabled.
func (m *PodProbeManager) ensurePodProbeMarker(ctx context.Context, box *agentsv1alpha1.Sandbox, pod *corev1.Pod) error {
	if !kruiseIntegrationEnabled() {
		// No informer and no marker access: real-node probe conditions stay
		// Unknown and the pause decision fails closed on them.
		return nil
	}
	if len(box.Spec.Probes) == 0 {
		return m.deletePodProbeMarker(ctx, box)
	}

	desired := buildPodProbeMarker(box, pod)
	existing := &kruiseappsv1alpha1.PodProbeMarker{}
	err := m.Get(ctx, types.NamespacedName{Name: box.Name, Namespace: box.Namespace}, existing)
	if meta.IsNoMatchError(err) {
		return m.reportKruiseCRDMissing(ctx, box, err)
	}
	if errors.IsNotFound(err) {
		if err := m.Create(ctx, desired); err != nil {
			if !errors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create PodProbeMarker: %w", err)
			}
			// The cached Get missed a marker that exists: the informer has not
			// caught up with a just-created marker, or GC has not removed the old
			// pod's marker yet. Fall through to the update path.
			if gerr := m.Get(ctx, types.NamespacedName{Name: box.Name, Namespace: box.Namespace}, existing); gerr != nil {
				return client.IgnoreNotFound(gerr)
			}
		} else {
			klog.FromContext(ctx).Info("created PodProbeMarker")
			return nil
		}
	} else if err != nil {
		return fmt.Errorf("failed to get PodProbeMarker: %w", err)
	}

	// Re-point the OwnerReference when the pod was recreated — same name, new UID
	// — otherwise GC of the old pod deletes the surviving marker and probe
	// delivery drops until a later reconcile recreates it.
	if !podProbeMarkerSpecEqual(existing.Spec, desired.Spec) || !ownerReferencesEqual(existing.OwnerReferences, desired.OwnerReferences) {
		existing.Spec = desired.Spec
		existing.OwnerReferences = desired.OwnerReferences
		if err := m.Update(ctx, existing); err != nil {
			return fmt.Errorf("failed to update PodProbeMarker: %w", err)
		}
		klog.FromContext(ctx).Info("updated PodProbeMarker")
	}
	return nil
}

// ownerReferencesEqual compares owner identity, ignoring the controller and
// blockOwnerDeletion flags that buildPodProbeMarker sets identically.
func ownerReferencesEqual(a, b []metav1.OwnerReference) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Kind != b[i].Kind || a[i].Name != b[i].Name || a[i].UID != b[i].UID {
			return false
		}
	}
	return true
}

// deletePodProbeMarker removes the PodProbeMarker for a sandbox if it exists.
func (m *PodProbeManager) deletePodProbeMarker(ctx context.Context, box *agentsv1alpha1.Sandbox) error {
	ppm := &kruiseappsv1alpha1.PodProbeMarker{}
	err := m.Get(ctx, types.NamespacedName{Name: box.Name, Namespace: box.Namespace}, ppm)
	if meta.IsNoMatchError(err) {
		return m.reportKruiseCRDMissing(ctx, box, err)
	}
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get PodProbeMarker for deletion: %w", err)
	}
	if err := m.Delete(ctx, ppm); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to delete PodProbeMarker: %w", err)
	}
	klog.FromContext(ctx).Info("deleted PodProbeMarker")
	return nil
}

// isVirtualNode reports whether the node carries the type=virtual-kubelet label.
// There is no universal standard for virtual nodes — this label is the convention
// the controller agrees on with its platform — so a provider marking nodes
// differently is classified as real. The label only steers post-scheduling
// delivery, while creation-time injection is decided from the pod's scheduling
// constraints, so a mislabeled node costs a missing probe condition rather than
// an unrecoverable missing annotation.
func (m *PodProbeManager) isVirtualNode(ctx context.Context, nodeName string) (bool, error) {
	node := &corev1.Node{}
	if err := m.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return false, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}
	return autopause.IsVirtualKubeletNode(node), nil
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

// buildPodProbeMarker constructs a PodProbeMarker from Sandbox.Spec.Probes,
// owned by the pod so Kubernetes GC deletes it when the pod is deleted. The
// selector matches the sandbox-uid label stamped on every generated pod: a UID
// always fits a label value, unlike the sandbox name.
func buildPodProbeMarker(box *agentsv1alpha1.Sandbox, pod *corev1.Pod) *kruiseappsv1alpha1.PodProbeMarker {
	defaultContainer := ""
	if len(pod.Spec.Containers) > 0 {
		defaultContainer = pod.Spec.Containers[0].Name
	}

	probes := make([]kruiseappsv1alpha1.PodContainerProbe, 0, len(box.Spec.Probes))
	for i := range box.Spec.Probes {
		p := &box.Spec.Probes[i]
		containerName := p.ContainerName
		if containerName == "" {
			containerName = defaultContainer
		}
		probes = append(probes, kruiseappsv1alpha1.PodContainerProbe{
			Name:             p.Name,
			ContainerName:    containerName,
			Probe:            kruiseappsv1alpha1.ContainerProbeSpec{Probe: p.Probe},
			PodConditionType: agentsv1alpha1.ProbeConditionType(p.Name),
		})
	}

	return &kruiseappsv1alpha1.PodProbeMarker{
		ObjectMeta: metav1.ObjectMeta{
			Name:            box.Name,
			Namespace:       box.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(pod, podGVK)},
		},
		Spec: kruiseappsv1alpha1.PodProbeMarkerSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				agentsv1alpha1.LabelSandboxUID: string(box.UID),
			}},
			Probes: probes,
		},
	}
}

// podProbeMarkerSpecEqual compares serialized specs, so it is order-sensitive;
// order follows the stable Spec.Probes slice, so a difference is a real change.
func podProbeMarkerSpecEqual(a, b kruiseappsv1alpha1.PodProbeMarkerSpec) bool {
	if len(a.Probes) != len(b.Probes) {
		return false
	}
	aJSON, _ := json.Marshal(a)
	bJSON, _ := json.Marshal(b)
	return string(aJSON) == string(bJSON)
}
