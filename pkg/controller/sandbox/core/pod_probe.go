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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
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
func (m *PodProbeManager) InjectProbe(box *agentsv1alpha1.Sandbox, pod *corev1.Pod) {
	if errs := validateProbes(box.Spec.Probes); len(errs) > 0 {
		klog.ErrorS(errs.ToAggregate(), "probe validation failed, skipping injection", "sandbox", klog.KObj(box))
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

// validate validates probe configurations and updates the SandboxConditionProbeValid
// condition. Returns false if any probe is invalid, in which case the condition is
// set to False. A Warning event is emitted only on the first transition to invalid
// (not on every reconcile) to avoid event spam. When all probes are valid, the
// condition is set to True (if not already).
func (m *PodProbeManager) validate(box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus) bool {
	if len(box.Spec.Probes) == 0 {
		return true
	}
	if errs := validateProbes(box.Spec.Probes); len(errs) > 0 {
		klog.ErrorS(errs.ToAggregate(), "probe validation failed", "sandbox", klog.KObj(box))
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
		return false
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
// Spec.Probes take effect on the pod. If validation fails, the Condition is
// set to False and no further action is taken. If the probes are valid, the
// pod is patched (via RawPatch to avoid resourceVersion conflicts) so the
// runtime picks up any changes to Spec.Probes while the sandbox is Running.
// Finally, probe conditions are synced from Pod.Status.Conditions to
// Sandbox.Status.Conditions.
func (m *PodProbeManager) EnsureProbe(ctx context.Context, box *agentsv1alpha1.Sandbox, pod *corev1.Pod, newStatus *agentsv1alpha1.SandboxStatus) error {
	if !m.validate(box, newStatus) {
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
		klog.InfoS("ensured pod probe", "sandbox", klog.KObj(box), "pod", klog.KObj(pod))
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
		condType := agentsv1alpha1.ProbeConditionPrefix + probe.Name
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

		// Case 3: Normal sync from pod condition.
		// SetSandboxCondition is idempotent — it skips if status/reason/message all match.
		utils.SetSandboxCondition(newStatus, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionStatus(podCond.Status),
			Reason:             podCond.Reason,
			Message:            podCond.Message,
			LastTransitionTime: podCond.LastTransitionTime,
		})
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
func validateProbes(probes []agentsv1alpha1.Probe) field.ErrorList {
	allErrs := field.ErrorList{}
	for i := range probes {
		path := field.NewPath("spec", "probes").Index(i)
		allErrs = append(allErrs, validateProbe(&probes[i], path)...)
	}
	return allErrs
}

// validateProbe validates a single probe configuration.
func validateProbe(probe *agentsv1alpha1.Probe, path *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if probe.Name == "" {
		allErrs = append(allErrs, field.Required(path.Child("name"), "probe name is required"))
	}

	// Count how many handler types are set.
	handler := probe.ProbeHandler
	handlers := 0
	if handler.Exec != nil {
		handlers++
	}
	if handler.HTTPGet != nil {
		handlers++
	}
	if handler.TCPSocket != nil {
		handlers++
	}
	if handler.GRPC != nil {
		handlers++
	}

	if handlers == 0 {
		allErrs = append(allErrs, field.Required(path.Child("probe"), "must specify exactly one probe handler"))
	} else if handlers > 1 {
		allErrs = append(allErrs, field.Forbidden(path.Child("probe"), "only one probe handler can be specified"))
	}

	// Currently only Exec is supported.
	if handler.Exec == nil {
		allErrs = append(allErrs, field.NotSupported(path.Child("probe"), "non-exec", []string{"Exec"}))
	} else if len(handler.Exec.Command) == 0 {
		allErrs = append(allErrs, field.Required(path.Child("probe", "exec", "command"), "exec command is required"))
	}

	return allErrs
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
			PodConditionType: agentsv1alpha1.ProbeConditionPrefix + probe.Name,
			Probe:            probe.Probe,
		})
	}

	data, _ := json.Marshal(items)
	return string(data)
}
