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
)

// TODO for CRR based reuser
type noopSandboxReuser struct{}

func (n *noopSandboxReuser) Reuse(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *corev1.Pod) error {
	return nil
}

func (n *noopSandboxReuser) IsReuseComplete(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *corev1.Pod) (bool, error) {
	return true, nil
}

// SandboxReuseControl handles the sandbox reuse lifecycle (Reusing phase).
// It is designed to be used by any SandboxControl implementation
// (e.g. commonControl, acsControl) to share the reuse logic.
type SandboxReuseControl struct {
	client   client.Client
	recorder record.EventRecorder
	config   SandboxReuseConfig
}

const defaultFailureShutdownGrace = 5 * time.Minute

func NewSandboxReuseControl(c client.Client, recorder record.EventRecorder, config SandboxReuseConfig) *SandboxReuseControl {
	if config.Reuser == nil {
		config.Reuser = &noopSandboxReuser{}
	}
	if config.FailureShutdownGrace == 0 {
		config.FailureShutdownGrace = defaultFailureShutdownGrace
	}
	return &SandboxReuseControl{
		client:   c,
		recorder: recorder,
		config:   config,
	}
}

// ensureSandboxReused is the entry point for the reuse lifecycle. It delegates
// the core logic to doReuse and unifies error handling: retriable errors are
// returned directly so the controller retries; permanent failures are
// delegated to handleReuseFailed.
func (r *SandboxReuseControl) ensureSandboxReused(ctx context.Context, args EnsureFuncArgs) (time.Duration, error) {
	requeue, err := r.doReuse(ctx, args)
	if err == nil {
		return requeue, nil
	}
	if IsRetriable(err) {
		return 0, err
	}
	return r.handleReuseFailed(ctx, args.Box, args.NewStatus, err)
}

// doReuse contains the core reuse state-machine logic. Every error is returned
// directly — either as a RetriableError (transient, caller should retry) or a
// plain error (permanent). The caller (ensureSandboxReused) is responsible for
// classifying and acting on the error.
func (r *SandboxReuseControl) doReuse(ctx context.Context, args EnsureFuncArgs) (time.Duration, error) {
	box, newStatus := args.Box, args.NewStatus

	poolName := box.Labels[agentsv1alpha1.LabelSandboxPool]
	if poolName == "" {
		return 0, fmt.Errorf("sandbox %s has no sandbox-pool label", box.Name)
	}
	sbs := &agentsv1alpha1.SandboxSet{}
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: box.Namespace, Name: poolName}, sbs); err != nil {
		if apierrors.IsNotFound(err) {
			return 0, fmt.Errorf("SandboxSet %s not found", poolName)
		}
		return 0, &RetriableError{Err: fmt.Errorf("failed to get SandboxSet %s: %w", poolName, err)}
	}

	// A nil Pod during reuse is an abnormal state — the sandbox should always
	// have a running Pod. Fail immediately rather than waiting indefinitely.
	if args.Pod == nil {
		return 0, fmt.Errorf("pod not found during reuse")
	}

	reuseCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReusing))
	if reuseCond == nil {
		reuseCond = &metav1.Condition{
			Type:               string(agentsv1alpha1.SandboxConditionReusing),
			Status:             metav1.ConditionFalse,
			Reason:             agentsv1alpha1.SandboxReusingReasonStarted,
			LastTransitionTime: metav1.Now(),
		}
		utils.SetSandboxCondition(newStatus, *reuseCond)
		r.recorder.Event(box, corev1.EventTypeNormal, agentsv1alpha1.SandboxReusingReasonStarted,
			fmt.Sprintf("Reuse started for sandbox %s", box.Name))
		klog.InfoS("Reuse started", "sandbox", klog.KObj(box))

		if err := r.config.Reuser.Reuse(ctx, box, args.Pod); err != nil {
			return 0, err
		}
		return 0, nil
	}

	var requeue time.Duration
	var err error

	switch reuseCond.Reason {
	case agentsv1alpha1.SandboxReusingReasonStarted:
		requeue, err = r.handleReuseInProgress(ctx, args, reuseCond)
		if err != nil || reuseCond.Reason != agentsv1alpha1.SandboxReusingReasonCompleted {
			return requeue, err
		}
		// Reuse just transitioned to Completed; fall through to grace period
		// handling to avoid wasting a reconcile cycle (especially when GracePeriod == 0).
		fallthrough
	case agentsv1alpha1.SandboxReusingReasonCompleted:
		requeue, err = r.handleReuseGracePeriod(ctx, args, reuseCond, sbs)
	default:
		// no action needed for terminal states (Succeeded, Failed, Timeout)
	}

	return requeue, err
}

func (r *SandboxReuseControl) handleReuseInProgress(ctx context.Context, args EnsureFuncArgs, reuseCond *metav1.Condition) (time.Duration, error) {
	box, newStatus := args.Box, args.NewStatus

	if r.config.Timeout > 0 {
		elapsed := time.Since(reuseCond.LastTransitionTime.Time)
		if elapsed > r.config.Timeout {
			return 0, &reuseTimeoutError{timeout: r.config.Timeout}
		}
	}

	complete, err := r.config.Reuser.IsReuseComplete(ctx, box, args.Pod)
	if err != nil {
		return 0, err
	}
	if !complete {
		klog.InfoS("Reuse cleanup not yet complete, waiting", "sandbox", klog.KObj(box))
		return 0, nil
	}
	// Check whether the Pod is Ready before transitioning to the Completed phase.
	// The reuser reports cleanup completion, but the Pod may still be restarting
	// after reset. We wait for Pod Ready to ensure the sandbox is truly available.
	readyCond := utils.GetPodCondition(&args.Pod.Status, corev1.PodReady)
	if readyCond == nil || readyCond.Status != corev1.ConditionTrue {
		klog.InfoS("Reuse cleanup complete but pod not ready, waiting", "sandbox", klog.KObj(box))
		return 0, nil
	}
	reuseCond.Reason = agentsv1alpha1.SandboxReusingReasonCompleted
	reuseCond.LastTransitionTime = metav1.Now()
	reuseCond.Message = ""
	utils.SetSandboxCondition(newStatus, *reuseCond)
	klog.InfoS("Reuse cleanup completed, entering grace period", "sandbox", klog.KObj(box))
	return r.config.GracePeriod, nil
}

func (r *SandboxReuseControl) handleReuseGracePeriod(ctx context.Context, args EnsureFuncArgs, reuseCond *metav1.Condition, sbs *agentsv1alpha1.SandboxSet) (time.Duration, error) {
	box, newStatus := args.Box, args.NewStatus

	elapsed := time.Since(reuseCond.LastTransitionTime.Time)
	if elapsed < r.config.GracePeriod {
		return r.config.GracePeriod - elapsed, nil
	}

	if err := r.resetMetadataForPool(ctx, box, sbs); err != nil {
		return 0, &RetriableError{Err: err}
	}

	newStatus.ReuseCount++
	newStatus.Phase = agentsv1alpha1.SandboxRunning
	reuseCond.Reason = agentsv1alpha1.SandboxReusingReasonSucceeded
	reuseCond.Status = metav1.ConditionTrue
	reuseCond.LastTransitionTime = metav1.Now()
	utils.SetSandboxCondition(newStatus, *reuseCond)
	utils.SetSandboxCondition(newStatus, metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxConditionReady),
		Status:             metav1.ConditionTrue,
		Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
		LastTransitionTime: metav1.Now(),
	})
	r.recorder.Event(box, corev1.EventTypeNormal, agentsv1alpha1.SandboxReusingReasonSucceeded,
		fmt.Sprintf("Reuse succeeded, sandbox returned to pool (reuseCount: %d)", newStatus.ReuseCount))
	klog.InfoS("Sandbox returned to pool", "sandbox", klog.KObj(box), "reuseCount", newStatus.ReuseCount)
	return 0, nil
}

// reuseTimeoutError is a permanent error that carries the SandboxReusingReasonTimeout
// condition reason so handleReuseFailed can distinguish it from other failures.
type reuseTimeoutError struct {
	timeout time.Duration
}

func (e *reuseTimeoutError) Error() string {
	return fmt.Sprintf("reuse timed out after %s", e.timeout)
}

func (e *reuseTimeoutError) Reason() string {
	return agentsv1alpha1.SandboxReusingReasonTimeout
}

func (r *SandboxReuseControl) handleReuseFailed(ctx context.Context, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus, err error) (time.Duration, error) {
	reason := agentsv1alpha1.SandboxReusingReasonFailed
	var fr FailedReason
	if errors.As(err, &fr) {
		reason = fr.Reason()
	}
	msg := err.Error()
	r.recorder.Event(box, corev1.EventTypeWarning, reason, msg)
	utils.SetSandboxCondition(newStatus, metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxConditionReusing),
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
	})
	// Parse reuse-retain-on-failure annotation (duration string, e.g. "5m"):
	//   - not set: delete the sandbox immediately.
	//   - valid duration: set ShutdownTime = now + duration, let checkTimers handle deletion.
	//   - invalid: log a warning and delete the sandbox immediately.
	retainStr := box.Annotations[agentsv1alpha1.AnnotationReuseRetainOnFailure]
	if retainStr == "" {
		klog.InfoS("Reuse failed, deleting sandbox immediately", "sandbox", klog.KObj(box), "reason", msg)
		return 0, r.client.Delete(ctx, box)
	}
	retainDuration, parseErr := time.ParseDuration(retainStr)
	if parseErr != nil || retainDuration <= 0 {
		klog.InfoS("Reuse failed, invalid reuse-retain-on-failure value, deleting sandbox", "sandbox", klog.KObj(box), "value", retainStr, "reason", msg)
		return 0, r.client.Delete(ctx, box)
	}
	shutdownAt := metav1.NewTime(time.Now().Add(retainDuration))
	patch := client.MergeFrom(box.DeepCopy())
	box.Spec.ShutdownTime = &shutdownAt
	if err := r.client.Patch(ctx, box, patch); err != nil {
		return 0, fmt.Errorf("failed to set shutdownTime on reuse failure: %w", err)
	}
	klog.InfoS("Reuse failed, scheduled shutdown", "sandbox", klog.KObj(box), "shutdownTime", shutdownAt.Time, "retainDuration", retainDuration, "reason", msg)
	return retainDuration, nil
}

func (r *SandboxReuseControl) resetMetadataForPool(ctx context.Context, box *agentsv1alpha1.Sandbox, sbs *agentsv1alpha1.SandboxSet) error {
	patch := client.MergeFrom(box.DeepCopy())

	// Part 1: Reset fixed claim metadata
	box.Spec.ShutdownTime = nil
	box.Spec.PauseTime = nil
	box.OwnerReferences = []metav1.OwnerReference{
		*metav1.NewControllerRef(sbs, agentsv1alpha1.SandboxSetControllerKind),
	}
	box.Labels[agentsv1alpha1.LabelSandboxIsClaimed] = agentsv1alpha1.False
	delete(box.Labels, agentsv1alpha1.LabelSandboxClaimName)
	delete(box.Annotations, agentsv1alpha1.AnnotationClaimTime)
	delete(box.Annotations, agentsv1alpha1.AnnotationLock)
	delete(box.Annotations, agentsv1alpha1.AnnotationOwner)
	delete(box.Annotations, agentsv1alpha1.AnnotationInitRuntimeRequest)
	delete(box.Annotations, agentsv1alpha1.AnnotationRuntimeAccessToken)
	delete(box.Annotations, agentsv1alpha1.AnnotationReuse)
	delete(box.Annotations, agentsv1alpha1.AnnotationReuseRetainOnFailure)

	// Annotations set during the claim flow but were missing from the initial cleanup:
	delete(box.Annotations, agentsv1alpha1.AnnotationCSIVolumeConfig)
	delete(box.Annotations, agentsv1alpha1.SandboxAnnotationPriority)
	delete(box.Annotations, identity.AgentKeyTokenRefreshStatus)

	// Annotations that may carry user-session-specific data from post-claim steps:
	delete(box.Annotations, agentsv1alpha1.AnnotationEnvdAccessToken)
	delete(box.Annotations, agentsv1alpha1.AnnotationEnvdURL)
	delete(box.Annotations, agentsv1alpha1.AnnotationRuntimeURL)

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
	klog.InfoS("Reset sandbox for pool", "sandbox", klog.KObj(box), "sandboxSet", sbs.Name)
	return nil
}

const annotationResetRequest = "agents.kruise.io/reset-request"

// ResetRequest is the JSON payload written to the Pod's reset-request annotation.
type ResetRequest struct {
	ResetID     string `json:"resetID"`
	RequestTime string `json:"requestTime"`
}

// ResetResult is the JSON payload in the ResetComplete Pod condition message.
type ResetResult struct {
	ResetID    string `json:"resetID"`
	StartTime  string `json:"startTime"`
	FinishTime string `json:"finishTime"`
	Error      string `json:"error,omitempty"`
}

// PodResetReuser implements SandboxReuser by writing a reset-request annotation
// on the sandbox's Pod and polling a PodCondition for completion.
type PodResetReuser struct {
	client client.Client
}

func NewPodResetReuser(c client.Client) SandboxReuser {
	return &PodResetReuser{client: c}
}

func (r *PodResetReuser) Reuse(ctx context.Context, sandbox *agentsv1alpha1.Sandbox, pod *corev1.Pod) error {
	request := ResetRequest{
		ResetID:     fmt.Sprintf("%d", sandbox.Status.ReuseCount+1),
		RequestTime: time.Now().UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("failed to marshal reset request: %w", err)
	}

	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[annotationResetRequest] = string(raw)
	if err := r.client.Patch(ctx, pod, patch); err != nil {
		return &RetriableError{Err: fmt.Errorf("failed to patch pod with reset request: %w", err)}
	}

	klog.InfoS("Reset request sent to pod", "sandbox", klog.KObj(sandbox), "resetID", request.ResetID)
	return nil
}

func (r *PodResetReuser) IsReuseComplete(_ context.Context, _ *agentsv1alpha1.Sandbox, pod *corev1.Pod) (bool, error) {
	cond := utils.GetPodCondition(&pod.Status, PodConditionResetComplete)
	if cond == nil {
		return false, nil
	}

	var result ResetResult
	if err := json.Unmarshal([]byte(cond.Message), &result); err != nil {
		return false, nil
	}

	requestJSON := pod.Annotations[annotationResetRequest]
	if requestJSON == "" {
		return false, nil
	}
	var request ResetRequest
	if err := json.Unmarshal([]byte(requestJSON), &request); err != nil {
		return false, nil
	}

	if result.ResetID != request.ResetID {
		return false, nil
	}

	if cond.Status == corev1.ConditionTrue {
		return true, nil
	}
	return false, fmt.Errorf("reset %s: %s", cond.Reason, result.Error)
}
