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
	"errors"
	"fmt"
	"path"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/utils"
	agentsruntime "github.com/openkruise/agents/pkg/utils/runtime"
)

const (
	// csiResetSignalWriteTimeout bounds a single write attempt of the reset signal file.
	csiResetSignalWriteTimeout = 3 * time.Second
	// csiResetSignalMaxRetries is the number of write attempts for the reset signal
	// file before giving up. Transient runtime unavailability right at recycle start
	// is common, so a few inline retries smooth over it.
	csiResetSignalMaxRetries = 3
)

// csiResetSignalRetryInterval is the backoff between reset signal write attempts.
// It is a var rather than a const so tests can shorten it.
var csiResetSignalRetryInterval = 1 * time.Second

// writeRuntimeFileFunc is a package-level seam over agentsruntime.WriteFileWithRuntime
// so tests can stub the runtime file write without a live sandbox.
var writeRuntimeFileFunc = agentsruntime.WriteFileWithRuntime

// TODO for CRR based recycler
type noopSandboxRecycler struct{}

func (n *noopSandboxRecycler) Recycle(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *corev1.Pod) error {
	return nil
}

func (n *noopSandboxRecycler) IsRecycleComplete(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *corev1.Pod) (bool, error) {
	return true, nil
}

// SandboxRecycleControl handles the sandbox recycle lifecycle (Recycling phase).
// It is designed to be used by any SandboxControl implementation
// (e.g. commonControl, acsControl) to share the recycle logic.
type SandboxRecycleControl struct {
	client   client.Client
	recorder record.EventRecorder
	config   SandboxRecycleConfig
}

const defaultFailureShutdownGrace = 5 * time.Minute

func NewSandboxRecycleControl(c client.Client, recorder record.EventRecorder, config SandboxRecycleConfig) *SandboxRecycleControl {
	if config.Recycler == nil {
		config.Recycler = &noopSandboxRecycler{}
	}
	if config.FailureShutdownGrace == 0 {
		config.FailureShutdownGrace = defaultFailureShutdownGrace
	}
	return &SandboxRecycleControl{
		client:   c,
		recorder: recorder,
		config:   config,
	}
}

// ensureSandboxRecycled is the entry point for the recycle lifecycle. It delegates
// the core logic to doRecycle and unifies error handling: retriable errors are
// returned directly so the controller retries; permanent failures are
// delegated to handleRecycleFailed.
func (r *SandboxRecycleControl) ensureSandboxRecycled(ctx context.Context, args EnsureFuncArgs) (time.Duration, error) {
	requeue, err := r.doRecycle(ctx, args)
	if err == nil {
		return requeue, nil
	}
	if IsRetriable(err) {
		return 0, err
	}
	return r.handleRecycleFailed(ctx, args.Box, args.NewStatus, err)
}

// doRecycle contains the core recycle state-machine logic. Every error is returned
// directly — either as a RetriableError (transient, caller should retry) or a
// plain error (permanent). The caller (ensureSandboxRecycled) is responsible for
// classifying and acting on the error.
func (r *SandboxRecycleControl) doRecycle(ctx context.Context, args EnsureFuncArgs) (time.Duration, error) {
	box, newStatus := args.Box, args.NewStatus

	sbs, err := r.validateRecyclePreconditions(ctx, args)
	if err != nil {
		return 0, err
	}

	recycleCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionRecycling))
	if recycleCond == nil {
		recycleCond = &metav1.Condition{
			Type:               string(agentsv1alpha1.SandboxConditionRecycling),
			Status:             metav1.ConditionFalse,
			Reason:             agentsv1alpha1.SandboxRecyclingReasonStarted,
			LastTransitionTime: metav1.Now(),
		}
		utils.SetSandboxCondition(newStatus, *recycleCond)
		r.recorder.Event(box, corev1.EventTypeNormal, agentsv1alpha1.SandboxRecyclingReasonStarted,
			fmt.Sprintf("Recycling started for sandbox %s", box.Name))
		klog.FromContext(ctx).Info("Recycling started", "sandbox", klog.KObj(box))

		// Signal the csi-sidecar to unmount stale CSI volumes when it stops
		// (prestop/SIGTERM) before handing the sandbox back for recycle. Must run
		// while the runtime is still reachable, i.e. before the recycler resets the
		// sandbox.
		if err := r.ensureCSIResetSignal(ctx, box); err != nil {
			return 0, err
		}

		if err := r.config.Recycler.Recycle(ctx, box, args.Pod); err != nil {
			return 0, err
		}
		return 0, nil
	}

	var requeue time.Duration

	switch recycleCond.Reason {
	case agentsv1alpha1.SandboxRecyclingReasonStarted:
		requeue, err = r.handleRecycleInProgress(ctx, args, recycleCond)
		if err != nil || recycleCond.Reason != agentsv1alpha1.SandboxRecyclingReasonCompleted {
			return requeue, err
		}
		// Recycle just transitioned to Completed; fall through to grace period
		// handling to avoid wasting a reconcile cycle (especially when GracePeriod == 0).
		fallthrough
	case agentsv1alpha1.SandboxRecyclingReasonCompleted:
		requeue, err = r.handleRecycleGracePeriod(ctx, args, recycleCond, sbs)
	default:
		// no action needed for terminal states (Succeeded, Failed, Timeout)
	}

	return requeue, err
}

// ensureCSIResetSignal writes an empty reset signal file into the sandbox's
// configured CSI reset directory so that a stopping csi-sidecar can detect it
// during prestop/SIGTERM and unmount stale CSI volumes before the sandbox is
// returned to the pool.
//
// It is a no-op when the sandbox carries no CSI mount annotation. When the sandbox
// does carry CSI mounts but the reset directory is not configured, it fails the
// recycle rather than silently returning stale mounts to the pool. The write goes
// through the agent-runtime files API, so it only depends on the runtime sidecar
// being reachable, not on any binary inside the sandbox. Transient failures are
// retried a few times inline.
func (r *SandboxRecycleControl) ensureCSIResetSignal(ctx context.Context, box *agentsv1alpha1.Sandbox) error {
	if box.Annotations[agentsv1alpha1.AnnotationCSIVolumeConfig] == "" {
		return nil
	}
	if r.config.CSIResetSignalDir == "" {
		return fmt.Errorf("sandbox carries CSI mounts but csi-reset-signal-dir is not configured")
	}

	resetFile := path.Join(r.config.CSIResetSignalDir, r.config.CSIResetSignalFileName)
	var lastErr error
	for attempt := 1; attempt <= csiResetSignalMaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, lastErr = writeRuntimeFileFunc(ctx, agentsruntime.WriteFileArgs{
			Sbx:         box,
			FilePath:    resetFile,
			Content:     []byte{},
			Permissions: 0644,
			Timeout:     csiResetSignalWriteTimeout,
		})
		if lastErr == nil {
			klog.FromContext(ctx).Info("Wrote CSI reset signal for recycle", "sandbox", klog.KObj(box), "file", resetFile, "attempt", attempt)
			return nil
		}
		if attempt < csiResetSignalMaxRetries {
			klog.FromContext(ctx).Info("Failed to write CSI reset signal, will retry", "sandbox", klog.KObj(box),
				"file", resetFile, "attempt", attempt, "error", lastErr)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(csiResetSignalRetryInterval):
			}
		}
	}
	return fmt.Errorf("failed to write csi reset signal to %s after %d attempts: %w", resetFile, csiResetSignalMaxRetries, lastErr)
}

// validateRecyclePreconditions performs all pre-condition checks before the
// recycle state machine begins: pool label, SandboxSet existence, template-hash
// currency, and Pod health. It returns the SandboxSet (needed by later stages)
// and an error if any check fails.
func (r *SandboxRecycleControl) validateRecyclePreconditions(ctx context.Context, args EnsureFuncArgs) (*agentsv1alpha1.SandboxSet, error) {
	box := args.Box

	poolName := box.Labels[agentsv1alpha1.LabelSandboxPool]
	if poolName == "" {
		return nil, fmt.Errorf("sandbox %s has no sandbox-pool label", box.Name)
	}
	sbs := &agentsv1alpha1.SandboxSet{}
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: box.Namespace, Name: poolName}, sbs); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("SandboxSet %s not found", poolName)
		}
		return nil, &RetriableError{Err: fmt.Errorf("failed to get SandboxSet %s: %w", poolName, err)}
	}

	// If the sandbox's template-hash does not match the SandboxSet's
	// updateRevision, the template has been updated since the sandbox was
	// created. There is no point continuing the recycle — the sandbox is
	// outdated and should not be returned to the pool.
	// When the SandboxSet's updateRevision is empty (not yet reconciled),
	// the check is skipped to avoid false failures during initial setup.
	if sbs.Status.UpdateRevision != "" &&
		box.Labels[agentsv1alpha1.LabelTemplateHash] != sbs.Status.UpdateRevision {
		return nil, fmt.Errorf("sandbox template-hash %q does not match SandboxSet %s updateRevision %q, sandbox is outdated and cannot be recycled",
			box.Labels[agentsv1alpha1.LabelTemplateHash], sbs.Name, sbs.Status.UpdateRevision)
	}

	// A nil Pod during recycle is an abnormal state — the sandbox should always
	// have a running Pod. Fail immediately rather than waiting indefinitely.
	if args.Pod == nil {
		return nil, fmt.Errorf("pod not found during recycle")
	}

	// If the Pod has already entered a terminal phase (Succeeded/Failed), the
	// runtime can never complete the reset. Fail the recycle immediately instead
	// of waiting until the timeout fires.
	if args.Pod.Status.Phase == corev1.PodSucceeded || args.Pod.Status.Phase == corev1.PodFailed {
		return nil, fmt.Errorf("pod entered terminal phase %s during recycle", args.Pod.Status.Phase)
	}

	return sbs, nil
}

func (r *SandboxRecycleControl) handleRecycleInProgress(ctx context.Context, args EnsureFuncArgs, recycleCond *metav1.Condition) (time.Duration, error) {
	box, newStatus := args.Box, args.NewStatus

	var remaining time.Duration
	if r.config.Timeout > 0 {
		elapsed := time.Since(recycleCond.LastTransitionTime.Time)
		if elapsed > r.config.Timeout {
			return 0, &recycleTimeoutError{timeout: r.config.Timeout}
		}
		remaining = r.config.Timeout - elapsed
	}

	complete, err := r.config.Recycler.IsRecycleComplete(ctx, box, args.Pod)
	if err != nil {
		return 0, err
	}
	if !complete {
		klog.FromContext(ctx).Info("Recycling not yet complete, waiting", "sandbox", klog.KObj(box))
		// Return a requeue duration to guarantee the controller re-reconciles
		// even if no external events arrive (e.g. runtime not responding).
		// This ensures the timeout check is eventually executed.
		return r.recyclePollingInterval(remaining), nil
	}
	// Check whether the Pod is Ready before transitioning to the Completed phase.
	// The recycler reports recycle completion, but the Pod may still be restarting
	// after reset. We wait for Pod Ready to ensure the sandbox is truly available.
	readyCond := utils.GetPodCondition(&args.Pod.Status, corev1.PodReady)
	if readyCond == nil || readyCond.Status != corev1.ConditionTrue {
		klog.FromContext(ctx).Info("Recycling complete but pod not ready, waiting", "sandbox", klog.KObj(box))
		return r.recyclePollingInterval(remaining), nil
	}
	recycleCond.Reason = agentsv1alpha1.SandboxRecyclingReasonCompleted
	recycleCond.LastTransitionTime = metav1.Now()
	recycleCond.Message = ""
	utils.SetSandboxCondition(newStatus, *recycleCond)
	klog.FromContext(ctx).Info("Recycling completed, entering grace period", "sandbox", klog.KObj(box))
	return r.config.GracePeriod, nil
}

// recyclePollingInterval returns the requeue duration when waiting for recycle to
// complete. If a timeout is configured, it returns the remaining time so the
// timeout fires on time. Otherwise it returns a default polling interval.
const defaultRecyclePollingInterval = 5 * time.Second

func (r *SandboxRecycleControl) recyclePollingInterval(remaining time.Duration) time.Duration {
	if remaining > 0 {
		if remaining < defaultRecyclePollingInterval {
			return remaining
		}
		return defaultRecyclePollingInterval
	}
	// No timeout configured; poll periodically anyway to avoid stalling.
	return defaultRecyclePollingInterval
}

func (r *SandboxRecycleControl) handleRecycleGracePeriod(ctx context.Context, args EnsureFuncArgs, recycleCond *metav1.Condition, sbs *agentsv1alpha1.SandboxSet) (time.Duration, error) {
	box, newStatus := args.Box, args.NewStatus

	elapsed := time.Since(recycleCond.LastTransitionTime.Time)
	if elapsed < r.config.GracePeriod {
		return r.config.GracePeriod - elapsed, nil
	}

	if err := r.resetMetadataForPool(ctx, box, sbs); err != nil {
		return 0, &RetriableError{Err: err}
	}

	newStatus.RecycledCount++
	newStatus.Phase = agentsv1alpha1.SandboxRunning
	recycleCond.Reason = agentsv1alpha1.SandboxRecyclingReasonSucceeded
	recycleCond.Status = metav1.ConditionTrue
	recycleCond.LastTransitionTime = metav1.Now()
	utils.SetSandboxCondition(newStatus, *recycleCond)
	utils.SetSandboxCondition(newStatus, metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxConditionReady),
		Status:             metav1.ConditionTrue,
		Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
		LastTransitionTime: metav1.Now(),
	})
	r.recorder.Event(box, corev1.EventTypeNormal, agentsv1alpha1.SandboxRecyclingReasonSucceeded,
		fmt.Sprintf("Recycling succeeded, sandbox returned to pool (recycledCount: %d)", newStatus.RecycledCount))
	klog.FromContext(ctx).Info("Sandbox returned to pool", "sandbox", klog.KObj(box), "recycledCount", newStatus.RecycledCount)
	return 0, nil
}

// recycleTimeoutError is a permanent error that carries the SandboxRecyclingReasonTimeout
// condition reason so handleRecycleFailed can distinguish it from other failures.
type recycleTimeoutError struct {
	timeout time.Duration
}

func (e *recycleTimeoutError) Error() string {
	return fmt.Sprintf("recycle timed out after %s", e.timeout)
}

func (e *recycleTimeoutError) Reason() string {
	return agentsv1alpha1.SandboxRecyclingReasonTimeout
}

func (r *SandboxRecycleControl) handleRecycleFailed(ctx context.Context, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus, err error) (time.Duration, error) {
	reason := agentsv1alpha1.SandboxRecyclingReasonFailed
	var fr FailedReason
	if errors.As(err, &fr) {
		reason = fr.Reason()
	}
	msg := err.Error()
	r.recorder.Event(box, corev1.EventTypeWarning, reason, msg)
	utils.SetSandboxCondition(newStatus, metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxConditionRecycling),
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
	})
	// Parse cleanup-retain-on-failure annotation (duration string, e.g. "5m"):
	//   - not set: delete the sandbox immediately.
	//   - valid duration: set ShutdownTime = now + duration, let checkTimers handle deletion.
	//   - invalid: log a warning and delete the sandbox immediately.
	retainStr := box.Annotations[agentsv1alpha1.AnnotationCleanupRetainOnFailure]
	if retainStr == "" {
		klog.FromContext(ctx).Info("Recycling failed, deleting sandbox immediately", "sandbox", klog.KObj(box), "reason", msg)
		return 0, r.client.Delete(ctx, box)
	}
	retainDuration, parseErr := time.ParseDuration(retainStr)
	if parseErr != nil || retainDuration <= 0 {
		klog.FromContext(ctx).Info("Recycling failed, invalid cleanup-retain-on-failure value, deleting sandbox", "sandbox", klog.KObj(box), "value", retainStr, "reason", msg)
		return 0, r.client.Delete(ctx, box)
	}
	shutdownAt := metav1.NewTime(time.Now().Add(retainDuration))
	patch := client.MergeFrom(box.DeepCopy())
	box.Spec.ShutdownTime = &shutdownAt
	if err := r.client.Patch(ctx, box, patch); err != nil {
		return 0, fmt.Errorf("failed to set shutdownTime on recycle failure: %w", err)
	}
	klog.FromContext(ctx).Info("Recycling failed, scheduled shutdown", "sandbox", klog.KObj(box), "shutdownTime", shutdownAt.Time, "retainDuration", retainDuration, "reason", msg)
	return retainDuration, nil
}

func (r *SandboxRecycleControl) resetMetadataForPool(ctx context.Context, box *agentsv1alpha1.Sandbox, sbs *agentsv1alpha1.SandboxSet) error {
	patch := client.MergeFrom(box.DeepCopy())

	// Part 1: Reset fixed claim metadata
	box.Spec.ShutdownTime = nil
	box.Spec.PauseTime = nil
	box.OwnerReferences = []metav1.OwnerReference{
		*metav1.NewControllerRef(sbs, agentsv1alpha1.SandboxSetControllerKind),
	}
	box.Labels[agentsv1alpha1.LabelSandboxIsClaimed] = agentsv1alpha1.False
	delete(box.Labels, agentsv1alpha1.LabelSandboxClaimName)
	for _, ann := range agentsv1alpha1.AnnotationsClearedOnRecycle {
		delete(box.Annotations, ann)
	}
	// Annotations from other packages not included in AnnotationsClearedOnRecycle:
	delete(box.Annotations, identity.AgentKeyTokenRefreshStatus)

	// Part 2: Delete user-specified metadata keys
	metadataJSON := box.Annotations[agentsv1alpha1.AnnotationUpdatedMetadataInClaim]
	if metadataJSON != "" {
		var updated agentsv1alpha1.UpdatedMetadataInClaim
		if err := json.Unmarshal([]byte(metadataJSON), &updated); err != nil {
			return fmt.Errorf("failed to unmarshal updated-metadata-in-claim: %w", err)
		}
		for _, key := range updated.Labels {
			delete(box.Labels, key)
		}
		for _, key := range updated.Annotations {
			delete(box.Annotations, key)
		}
	}
	delete(box.Annotations, agentsv1alpha1.AnnotationUpdatedMetadataInClaim)

	if err := r.client.Patch(ctx, box, patch); err != nil {
		return fmt.Errorf("failed to reset sandbox for pool: %w", err)
	}
	klog.FromContext(ctx).Info("Reset sandbox for pool", "sandbox", klog.KObj(box), "sandboxSet", sbs.Name)
	return nil
}
