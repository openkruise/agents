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
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/tracing"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"github.com/openkruise/agents/pkg/utils/fieldindex"

	"go.opentelemetry.io/otel/attribute"
)

// CheckpointControl manages Checkpoint CR lifecycle for sandbox pause/resume flows.
type CheckpointControl struct {
	client.Client
	recorder record.EventRecorder
}

const (
	EventCheckpointStarted   = "CheckpointStarted"
	EventCheckpointSucceeded = "CheckpointSucceeded"
	EventCheckpointFailed    = "CheckpointFailed"
)

// NewCheckpointControl creates a new CheckpointControl.
func NewCheckpointControl(cli client.Client, recorder record.EventRecorder) *CheckpointControl {
	return &CheckpointControl{Client: cli, recorder: recorder}
}

// CheckpointScope parameterizes the checkpoint behavior for different pause
// strategies. The scope is driven by PersistentContents: the derived label
// determines which existing checkpoints are queried, and the contents are
// passed to the checkpoint controller to decide what state to persist.
type CheckpointScope struct {
	// PersistentContents is the list of contents to persist (e.g. "podInfo",
	// "filesystem"). The checkpoint label is derived from the sorted contents.
	PersistentContents []string
	// ValidateImages controls whether container images are compared against
	// the sandbox template before allowing the pause to proceed.
	ValidateImages bool
}

// checkpointContentsForPause derives the checkpoint persistent contents for
// the pause flow from sandbox.spec.persistentContents: requested dump contents
// (filesystem/memory) are passed through as-is and podInfo is used only when
// no dump content is requested. Dump contents are never combined with podInfo
// because a dump checkpoint already carries the pod info needed to rebuild the
// pod template delta on resume. Without dump contents the result is a
// pod-info-only checkpoint carrying no dump data.
func checkpointContentsForPause(box *agentsv1alpha1.Sandbox) []string {
	var contents []string
	for _, c := range box.Spec.PersistentContents {
		switch c {
		case agentsv1alpha1.PersistentContentFilesystem:
			contents = append(contents, agentsv1alpha1.CheckpointPersistentContentFilesystem)
		case agentsv1alpha1.PersistentContentMemory:
			contents = append(contents, agentsv1alpha1.CheckpointPersistentContentMemory)
		}
	}
	if len(contents) == 0 {
		return []string{agentsv1alpha1.CheckpointPersistentContentPodInfo}
	}
	return contents
}

// checkpointLabelForContents derives the checkpoint type label from the sorted
// PersistentContents. For a single content the label is the content itself
// (e.g. "podInfo", "filesystem"); for multiple contents they are joined with
// a hyphen in sorted order (e.g. "filesystem-memory").
func checkpointLabelForContents(contents []string) string {
	if len(contents) == 0 {
		return ""
	}
	sorted := make([]string, len(contents))
	copy(sorted, contents)
	sort.Strings(sorted)
	return strings.Join(sorted, "-")
}

// AssumePodCheckpointed validates container images and manages the Checkpoint CR lifecycle.
// Returns true if the pause flow should wait (checkpoint in progress or image rejected).
func (c *CheckpointControl) AssumePodCheckpointed(ctx context.Context, pod *corev1.Pod, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus, cond *metav1.Condition, scope CheckpointScope) bool {
	// Allow-list of paused reasons that should drive the checkpoint flow.
	// Any other reason (e.g. CheckpointSucceeded already reached, or a reason
	// introduced in the future) skips this flow on purpose; new reasons that
	// need checkpointing must be added here explicitly.
	switch cond.Reason {
	case "",
		agentsv1alpha1.SandboxPausedReasonPending,
		agentsv1alpha1.SandboxPausedReasonCheckpointCreating,
		agentsv1alpha1.SandboxPausedReasonImageChanged,
		agentsv1alpha1.SandboxPausedReasonCheckpointFailed:
		// fall through to checkpoint handling below
	default:
		return false
	}

	if scope.ValidateImages {
		if err := validateContainerImages(pod, box); err != nil {
			cond.Status = metav1.ConditionFalse
			cond.Reason = agentsv1alpha1.SandboxPausedReasonImageChanged
			cond.Message = err.Error()
			utils.SetSandboxCondition(newStatus, *cond)
			c.recorder.Event(box, corev1.EventTypeWarning, agentsv1alpha1.SandboxPausedReasonImageChanged, err.Error())
			klog.FromContext(ctx).Error(err, "Image validation failed, pause rejected", "sandbox", klog.KObj(box))
			return true
		}
	}
	if cond.Reason == "" || cond.Reason == agentsv1alpha1.SandboxPausedReasonPending {
		cond.Reason = agentsv1alpha1.SandboxPausedReasonCheckpointCreating
		cond.Message = "Checkpoint created, waiting for completion"
		utils.SetSandboxCondition(newStatus, *cond)
	}

	cp, _, err := c.ensureCheckpointCR(ctx, box, scope.PersistentContents)
	if err != nil {
		klog.FromContext(ctx).Error(err, "Failed to ensure checkpoint", "sandbox", klog.KObj(box))
		cond.Reason = agentsv1alpha1.SandboxPausedReasonCheckpointFailed
		cond.Message = err.Error()
		utils.SetSandboxCondition(newStatus, *cond)
		c.recorder.Event(box, corev1.EventTypeWarning, agentsv1alpha1.SandboxPausedReasonCheckpointFailed, cond.Message)
		return true
	}
	if cp == nil {
		// Checkpoint just created, wait for the checkpoint controller to process it.
		return true
	}

	switch cp.Status.Phase {
	case agentsv1alpha1.CheckpointSucceeded:
		cond.Reason = agentsv1alpha1.SandboxPausedReasonCheckpointSucceeded
		cond.Message = ""
		utils.SetSandboxCondition(newStatus, *cond)
		c.recordCheckpointEvent(box, corev1.EventTypeNormal, EventCheckpointSucceeded, "Checkpoint %s succeeded", cp.Name)
		return false
	case agentsv1alpha1.CheckpointFailed:
		cond.Reason = agentsv1alpha1.SandboxPausedReasonCheckpointFailed
		cond.Message = fmt.Sprintf("Checkpoint failed: %s", cp.Status.Message)
		utils.SetSandboxCondition(newStatus, *cond)
		c.recorder.Event(box, corev1.EventTypeWarning, agentsv1alpha1.SandboxPausedReasonCheckpointFailed, cond.Message)
		return true
	default:
		cond.Message = fmt.Sprintf("Waiting for checkpoint %s", cp.Name)
		utils.SetSandboxCondition(newStatus, *cond)
		// Use klog.FromContext to automatically include traceID in logs,
		// enabling trace-log correlation during checkpoint polling.
		klog.FromContext(ctx).Info("Waiting for checkpoint to complete", "sandbox", klog.KObj(box), "checkpoint", cp.Name, "phase", cp.Status.Phase)
		return true
	}
}

// ensureCheckpointCR finds an existing checkpoint for the sandbox matching the
// given persistent contents, or creates a new one if none exists. It first
// queries by the new content-derived label, then falls back to the legacy
// v0.5.22 label for backward compatibility during controller upgrade.
// Returns (nil, cpName, nil) when a new checkpoint was just created (caller
// should wait). Returns a non-nil checkpoint when an existing one was found.
// Returns (nil, "", err) when an error occurred.
func (c *CheckpointControl) ensureCheckpointCR(ctx context.Context, box *agentsv1alpha1.Sandbox, persistentContents []string) (*agentsv1alpha1.Checkpoint, string, error) {
	label := checkpointLabelForContents(persistentContents)
	cpList, err := listCheckpointsForSandbox(ctx, c.Client, box, label)
	if err != nil {
		return nil, "", fmt.Errorf("failed to list checkpoints: %w", err)
	}
	if len(cpList) == 0 {
		// Fallback: try the legacy v0.5.22 label for backward compatibility.
		// TODO(legacy-compat): remove once all v0.5.22 checkpoints are garbage collected.
		if legacyLabel := agentsv1alpha1.LegacyCheckpointLabel(label); legacyLabel != "" {
			cpList, err = listCheckpointsForSandbox(ctx, c.Client, box, legacyLabel)
			if err != nil {
				return nil, "", fmt.Errorf("failed to list legacy checkpoints: %w", err)
			}
		}
	}
	if len(cpList) == 0 {
		cpName, err := c.createCheckpoint(ctx, box, persistentContents)
		if err != nil {
			return nil, "", fmt.Errorf("failed to create checkpoint: %w", err)
		}
		return nil, cpName, nil
	}
	return &cpList[0], cpList[0].Name, nil
}

// GetCheckpointResumeData retrieves the pod template delta and the checkpoint
// ID from the sandbox's checkpoints in a single list operation. Both fields
// come from the same checkpoint: the first one that recorded resume data, a
// non-empty pod template delta or a checkpoint ID. The delta may legitimately
// be empty when the pod carries no resource drift and no injected containers,
// so the checkpoint ID alone still qualifies the checkpoint. The delta rebuilds
// the pod on resume or a CheckpointRestore upgrade, and the checkpoint ID
// restores the pod's writable layer. A sandbox holds at most one active
// checkpoint at a time (created by the pause or the upgrade flow and cleaned
// up afterwards), so both fields always belong to a single checkpoint.
//
// Unlike the checkpoint creation path, reading here needs no feature gate
// check: a checkpoint only exists if the creation path created it in the
// first place.
func (c *CheckpointControl) GetCheckpointResumeData(ctx context.Context, box *agentsv1alpha1.Sandbox) (*runtime.RawExtension, string) {
	cpList, cpErr := listCheckpointsForSandbox(ctx, c.Client, box, "")
	if cpErr != nil {
		klog.FromContext(ctx).Error(cpErr, "Failed to list checkpoints for resume", "sandbox", klog.KObj(box))
		return nil, ""
	}
	for i := range cpList {
		hasDelta := len(cpList[i].Status.PodTemplateDelta.Raw) > 0
		hasID := cpList[i].Status.CheckpointId != ""
		if hasDelta || hasID {
			var delta *runtime.RawExtension
			if hasDelta {
				delta = &cpList[i].Status.PodTemplateDelta
			}
			return delta, cpList[i].Status.CheckpointId
		}
	}
	return nil, ""
}

// CleanupCheckpoints deletes all Checkpoint CRs for the given sandbox.
func (c *CheckpointControl) CleanupCheckpoints(ctx context.Context, box *agentsv1alpha1.Sandbox) {
	// Trace the cleanup so its latency is observable in Jaeger. Cleanup has
	// several exit points, so the span is closed once from a deferred closure.
	// Keep the closure: a direct defer tracing.EndSpan(ctx, span, err) would
	// evaluate err while still nil and record every failure as success. The
	// deletion failures below are best-effort and only recorded on the span so
	// they stay visible in traces.
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
	ctx, span := tracing.StartControllerSpan(ctx, tracing.SpanControllerCheckpointCleanup)
	var err error
	defer func() { tracing.EndSpan(ctx, span, err) }()
	cpList, cpErr := listCheckpointsForSandbox(ctx, c.Client, box, "")
	if cpErr != nil {
		err = cpErr
		log.Error(cpErr, "Failed to list checkpoints for cleanup")
		return
	}
	for i := range cpList {
		ScaleExpectation.ExpectScale(GetControllerKey(box), expectations.Delete, cpList[i].Name)
		delCtx, delSpan := tracing.StartControllerSpan(ctx, tracing.SpanControllerDeleteCheckpoint)
		delErr := c.Delete(delCtx, &cpList[i])
		tracing.EndSpan(delCtx, delSpan, client.IgnoreNotFound(delErr))
		if delErr != nil {
			// Settle the expectation here on any error, NotFound included. A
			// checkpoint that is already gone produces no further delete event,
			// so CheckpointEventHandler.Delete will never observe it and the
			// expectation would block the Sandbox reconcile until it times out.
			// The three other delete sites in the tree settle on any error for
			// the same reason.
			ScaleExpectation.ObserveScale(GetControllerKey(box), expectations.Delete, cpList[i].Name)
			if !errors.IsNotFound(delErr) {
				log.Error(delErr, "Failed to delete checkpoint after resume", "checkpoint", cpList[i].Name)
				err = delErr
				continue
			}
			// Logged apart from the delete below: the expectation was settled but
			// this call removed nothing, and a reader of these logs after the fact
			// needs the two cases to stay distinguishable.
			log.Info("Checkpoint already gone after resume, expectation settled", "checkpoint", cpList[i].Name)
			continue
		}
		log.Info("Deleted checkpoint after successful resume", "checkpoint", cpList[i].Name)
	}
}

// createCheckpoint creates a Checkpoint CR. The checkpoint controller is
// responsible for processing it and updating the status.
//
// The name carries a random suffix so each invocation produces a distinct
// checkpoint name. Idempotency within the same reconcile cycle is guaranteed
// by the caller, which only invokes this function when no existing checkpoint
// is found for the sandbox (see ensureCheckpointCR / AssumePodCheckpointed).
func (c *CheckpointControl) createCheckpoint(ctx context.Context, box *agentsv1alpha1.Sandbox, persistentContents []string) (string, error) {
	cpName := box.Name + "-" + utils.RandStringN(8)
	cp := &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cpName,
			Namespace: box.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(box, sandboxControllerKind),
			},
			Labels: map[string]string{
				agentsv1alpha1.CheckpointLabelSandboxName: box.Name,
				agentsv1alpha1.CheckpointLabelType:        checkpointLabelForContents(persistentContents),
			},
		},
		Spec: agentsv1alpha1.CheckpointSpec{
			SandboxName:        &box.Name,
			PersistentContents: persistentContents,
		},
	}
	ScaleExpectation.ExpectScale(GetControllerKey(box), expectations.Create, cpName)
	// The enclosing Reconcile span already carries the sandbox identity
	// attributes; only the checkpoint name is specific to this span.
	ctx, span := tracing.StartControllerSpan(ctx, tracing.SpanControllerCheckpoint,
		attribute.String(tracing.AttrCheckpointName, cpName),
	)
	err := c.Create(ctx, cp)
	tracing.EndSpan(ctx, span, err)
	if err != nil {
		ScaleExpectation.ObserveScale(GetControllerKey(box), expectations.Create, cpName)
		return "", fmt.Errorf("failed to create checkpoint CR: %w", err)
	}
	c.recordCheckpointEvent(box, corev1.EventTypeNormal, EventCheckpointStarted, "Checkpoint %s created, waiting for completion", cpName)
	klog.FromContext(ctx).Info("Created checkpoint CR", "sandbox", klog.KObj(box), "checkpoint", cpName)
	return cpName, nil
}

func (c *CheckpointControl) recordCheckpointEvent(box *agentsv1alpha1.Sandbox, eventType, reason, messageFmt string, args ...any) {
	if c.recorder == nil {
		return
	}
	c.recorder.Eventf(box, eventType, reason, messageFmt, args...)
}

// EnsureCheckpointForUpgrade ensures a Checkpoint CR exists for the sandbox and
// returns true once the checkpoint has succeeded. If no checkpoint exists, it
// creates one and returns false. If the checkpoint is still in progress, it
// returns false. If the checkpoint failed, it returns an error.
//
// The returned string is the checkpoint CR name (empty when short-circuited or
// on error before a checkpoint is found/created).
//
// If the sandbox does not use the CheckpointRestore upgrade strategy, this
// method short-circuits and returns (true, "", nil) so the caller can proceed to
// the UpgradePod step without any checkpointing.
func (c *CheckpointControl) EnsureCheckpointForUpgrade(ctx context.Context, box *agentsv1alpha1.Sandbox) (bool, string, error) {
	if box.Spec.UpgradePolicy == nil || box.Spec.UpgradePolicy.Type != agentsv1alpha1.SandboxUpgradePolicyCheckpointRestore {
		return true, "", nil
	}

	contents := []string{agentsv1alpha1.CheckpointPersistentContentFilesystem}
	cp, cpName, err := c.ensureCheckpointCR(ctx, box, contents)
	if err != nil {
		return false, "", err
	}
	if cp == nil {
		return false, cpName, nil
	}

	switch cp.Status.Phase {
	case agentsv1alpha1.CheckpointSucceeded:
		c.recordCheckpointEvent(box, corev1.EventTypeNormal, EventCheckpointSucceeded,
			"Checkpoint %s succeeded for upgrade", cp.Name)
		return true, cp.Name, nil
	case agentsv1alpha1.CheckpointFailed:
		c.recordCheckpointEvent(box, corev1.EventTypeWarning, EventCheckpointFailed,
			"Checkpoint %s failed during upgrade: %s", cp.Name, cp.Status.Message)
		return false, cp.Name, fmt.Errorf("checkpoint %s failed during upgrade: %s", cp.Name, cp.Status.Message)
	default:
		klog.InfoS("Waiting for checkpoint to complete before upgrade",
			"sandbox", klog.KObj(box), "checkpoint", cp.Name, "phase", cp.Status.Phase)
		return false, cp.Name, nil
	}
}

// validateContainerImages compares each user container's Image in the live Pod
// against the Image defined in sandbox.spec.template. If any image differs,
// the pause is rejected. A nil pod carries nothing to compare (e.g. the pod
// was already deleted), so validation trivially passes.
func validateContainerImages(pod *corev1.Pod, box *agentsv1alpha1.Sandbox) error {
	if pod == nil || box.Spec.Template == nil {
		return nil
	}
	for _, tc := range box.Spec.Template.Spec.Containers {
		for _, pc := range pod.Spec.Containers {
			if tc.Name == pc.Name && tc.Image != pc.Image {
				return fmt.Errorf("container %q image changed from %q to %q, pause is not allowed",
					tc.Name, tc.Image, pc.Image)
			}
		}
	}
	for _, tc := range box.Spec.Template.Spec.InitContainers {
		if tc.RestartPolicy == nil || *tc.RestartPolicy != corev1.ContainerRestartPolicyAlways {
			continue
		}
		for _, pc := range pod.Spec.InitContainers {
			if tc.Name == pc.Name && tc.Image != pc.Image {
				return fmt.Errorf("sidecar init container %q image changed from %q to %q, pause is not allowed",
					tc.Name, tc.Image, pc.Image)
			}
		}
	}
	return nil
}

// listCheckpointsForSandbox returns all Checkpoint CRs of the given type for the
// given sandbox, sorted newest-first by creation timestamp. When
// checkpointType is empty, all checkpoints for the sandbox are returned
// regardless of their type label.
func listCheckpointsForSandbox(ctx context.Context, cli client.Client, box *agentsv1alpha1.Sandbox, checkpointType string) ([]agentsv1alpha1.Checkpoint, error) {
	cpList := &agentsv1alpha1.CheckpointList{}
	listOpts := []client.ListOption{
		client.InNamespace(box.Namespace),
		client.MatchingFields{fieldindex.IndexNameForOwnerRefUID: string(box.UID)},
		client.UnsafeDisableDeepCopy,
	}
	if checkpointType != "" {
		listOpts = append(listOpts, client.MatchingLabels{agentsv1alpha1.CheckpointLabelType: checkpointType})
	}
	err := cli.List(ctx, cpList, listOpts...)
	if err != nil {
		return nil, err
	}
	if len(cpList.Items) == 0 {
		return nil, nil
	}
	// Filter out Checkpoints that are being deleted.
	items := make([]agentsv1alpha1.Checkpoint, 0, len(cpList.Items))
	for i := range cpList.Items {
		if cpList.Items[i].DeletionTimestamp.IsZero() {
			items = append(items, cpList.Items[i])
		}
	}
	if len(items) == 0 {
		return nil, nil
	}
	sort.Slice(items, func(i, j int) bool {
		return items[j].CreationTimestamp.Before(&items[i].CreationTimestamp)
	})
	return items, nil
}
