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
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/tracing"
	"github.com/openkruise/agents/pkg/utils"
)

const lifecycleHookNoSlotRequeue = time.Second

type lifecycleHookKind string

const (
	prePauseHook   lifecycleHookKind = "prePause"
	postResumeHook lifecycleHookKind = "postResume"
)

type LifecycleHookProgress struct {
	Terminal         bool
	ConditionChanged bool
	RequeueAfter     time.Duration
}

// LifecycleHookManager owns the bounded execution capacity shared by PrePause
// and PostResume hooks. Each hook executes once per lifecycle cycle; failed
// attempts are terminal and never retried. Retrying failed hooks is future
// work. Persisted Conditions are the source of truth for the cycle's result.
type LifecycleHookManager struct {
	hookFunc LifecycleHookFunc
	recorder record.EventRecorder
	slots    chan struct{}
}

func NewLifecycleHookManager(capacity int, hookFunc LifecycleHookFunc, recorder record.EventRecorder) *LifecycleHookManager {
	if capacity < 1 {
		capacity = 1
	}
	return &LifecycleHookManager{
		hookFunc: hookFunc,
		recorder: recorder,
		slots:    make(chan struct{}, capacity),
	}
}

func (m *LifecycleHookManager) BeginPrePause(box *agentsv1alpha1.Sandbox, status *agentsv1alpha1.SandboxStatus) {
	if m == nil || box == nil || status == nil {
		return
	}
	utils.RemoveSandboxCondition(status, string(agentsv1alpha1.SandboxConditionPrePauseHook))
	// A new Pause supersedes an unfinished PostResume, but retain a terminal
	// result until the next Resume replaces it.
	m.ClearActivePostResume(box, status)
	if box.Spec.Lifecycle != nil && box.Spec.Lifecycle.PrePause != nil {
		utils.SetSandboxCondition(status, newLifecycleHookCondition(agentsv1alpha1.SandboxConditionPrePauseHook,
			agentsv1alpha1.SandboxLifecycleHookReasonPending, metav1.ConditionFalse, box.Generation,
			"waiting to execute lifecycle hook"))
	}
}

func (m *LifecycleHookManager) ClearActivePostResume(box *agentsv1alpha1.Sandbox, status *agentsv1alpha1.SandboxStatus) {
	if m == nil || box == nil || status == nil {
		return
	}
	cond := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionPostResumeHook))
	if cond == nil || lifecycleHookConditionTerminal(cond) {
		return
	}
	m.removeCondition(box, status, postResumeHook)
}

func (m *LifecycleHookManager) ReconcilePrePause(ctx context.Context, box *agentsv1alpha1.Sandbox,
	pod *corev1.Pod, status *agentsv1alpha1.SandboxStatus) LifecycleHookProgress {
	var action *agentsv1alpha1.LifecycleAction
	if box.Spec.Lifecycle != nil {
		action = box.Spec.Lifecycle.PrePause
	}
	return m.reconcile(ctx, prePauseHook, box, pod, status, action)
}

func (m *LifecycleHookManager) reconcilePostResume(ctx context.Context, box *agentsv1alpha1.Sandbox,
	pod *corev1.Pod, status *agentsv1alpha1.SandboxStatus) LifecycleHookProgress {
	var action *agentsv1alpha1.LifecycleAction
	if box.Spec.Lifecycle != nil {
		action = box.Spec.Lifecycle.PostResume
	}
	return m.reconcile(ctx, postResumeHook, box, pod, status, action)
}

func (m *LifecycleHookManager) reconcile(ctx context.Context, kind lifecycleHookKind, box *agentsv1alpha1.Sandbox,
	pod *corev1.Pod, newStatus *agentsv1alpha1.SandboxStatus, action *agentsv1alpha1.LifecycleAction) LifecycleHookProgress {
	if m == nil || action == nil {
		changed := m.removeCondition(box, newStatus, kind)
		return LifecycleHookProgress{Terminal: true, ConditionChanged: changed}
	}

	conditionType := lifecycleHookConditionType(kind)
	if lifecycleHookConditionTerminal(utils.GetSandboxCondition(newStatus, string(conditionType))) {
		return LifecycleHookProgress{Terminal: true}
	}

	if kind == prePauseHook && (pod == nil || !pod.DeletionTimestamp.IsZero() || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded) {
		message := "hook skipped because the Sandbox Pod is not executable; pause continues"
		utils.SetSandboxCondition(newStatus, newLifecycleHookCondition(conditionType, agentsv1alpha1.SandboxLifecycleHookReasonSkipped,
			metav1.ConditionFalse, box.Generation, message))
		m.event(box, corev1.EventTypeWarning, string(conditionType)+agentsv1alpha1.SandboxLifecycleHookReasonSkipped, message)
		return LifecycleHookProgress{Terminal: true, ConditionChanged: true}
	}

	// The hook executes as soon as a non-terminal condition is observed: a
	// crash between execution and the terminal write loses the result and the
	// next controller re-runs the command. That replay is within the
	// proposal's at-least-once contract (hooks must be idempotent), and it
	// buys pause latency: no dedicated reconcile just to persist an
	// intermediate Running condition.
	if !m.tryAcquire() {
		return LifecycleHookProgress{RequeueAfter: lifecycleHookNoSlotRequeue}
	}
	defer m.release()

	spanName := tracing.SpanControllerPostResumeHook
	if kind == prePauseHook {
		spanName = tracing.SpanControllerPrePauseHook
	}
	hookCtx, span := tracing.StartControllerSpan(ctx, spanName)
	exitCode, _, _, hookErr := m.hookFunc(hookCtx, box, action)
	tracing.EndSpan(hookCtx, span, hookErr)

	if hookErr == nil && exitCode == 0 {
		// No success event: pools with auto-pause would emit one per cycle.
		// The Succeeded condition is the durable record; failures keep a
		// Warning event.
		utils.SetSandboxCondition(newStatus, newLifecycleHookCondition(conditionType, agentsv1alpha1.SandboxLifecycleHookReasonSucceeded,
			metav1.ConditionTrue, box.Generation, "lifecycle hook succeeded"))
		return LifecycleHookProgress{Terminal: true, ConditionChanged: true}
	}

	summary := lifecycleHookFailureSummary(exitCode, hookErr)
	// Log the underlying error (never the command or output) at manager level:
	// the condition and event carry only the safe summary, and the shared
	// executor logs results at debug verbosity at most.
	if hookErr != nil {
		klog.FromContext(ctx).Error(hookErr, "lifecycle hook failed",
			"sandbox", klog.KObj(box), "hook", string(kind), "exitCode", exitCode)
	}
	message := fmt.Sprintf("hook failed: %s; lifecycle transition continues", summary)
	utils.SetSandboxCondition(newStatus, newLifecycleHookCondition(conditionType, agentsv1alpha1.SandboxLifecycleHookReasonFailed,
		metav1.ConditionFalse, box.Generation, message))
	m.event(box, corev1.EventTypeWarning, string(conditionType)+agentsv1alpha1.SandboxLifecycleHookReasonFailed, message)
	return LifecycleHookProgress{Terminal: true, ConditionChanged: true}
}

func (m *LifecycleHookManager) tryAcquire() bool {
	select {
	case m.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (m *LifecycleHookManager) release() {
	<-m.slots
}

func (m *LifecycleHookManager) removeCondition(box *agentsv1alpha1.Sandbox, status *agentsv1alpha1.SandboxStatus, kind lifecycleHookKind) bool {
	if m == nil || box == nil || status == nil {
		return false
	}
	conditionType := string(lifecycleHookConditionType(kind))
	changed := utils.GetSandboxCondition(status, conditionType) != nil
	utils.RemoveSandboxCondition(status, conditionType)
	return changed
}

func (m *LifecycleHookManager) event(box *agentsv1alpha1.Sandbox, eventType, reason, message string) {
	if m != nil && m.recorder != nil {
		m.recorder.Event(box, eventType, reason, message)
	}
}

func newLifecycleHookCondition(conditionType agentsv1alpha1.SandboxConditionType, reason string,
	status metav1.ConditionStatus, generation int64, message string) metav1.Condition {
	return metav1.Condition{
		Type:               string(conditionType),
		Status:             status,
		Reason:             reason,
		Message:            utils.TruncateConditionMessage(message),
		ObservedGeneration: generation,
		LastTransitionTime: metav1.Now(),
	}
}

func lifecycleHookConditionType(kind lifecycleHookKind) agentsv1alpha1.SandboxConditionType {
	if kind == prePauseHook {
		return agentsv1alpha1.SandboxConditionPrePauseHook
	}
	return agentsv1alpha1.SandboxConditionPostResumeHook
}

func lifecycleHookConditionTerminal(cond *metav1.Condition) bool {
	if cond == nil {
		return false
	}
	switch cond.Reason {
	case agentsv1alpha1.SandboxLifecycleHookReasonSucceeded,
		agentsv1alpha1.SandboxLifecycleHookReasonFailed,
		agentsv1alpha1.SandboxLifecycleHookReasonSkipped:
		return true
	default:
		return false
	}
}

func lifecycleHookFailureSummary(exitCode int32, err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case connect.CodeOf(err) == connect.CodeDeadlineExceeded:
		// connect-go surfaces RPC deadlines as *connect.Error instead of the
		// context error.
		return "timeout"
	case err != nil:
		return "execution error"
	default:
		return fmt.Sprintf("process exited with code %d", exitCode)
	}
}
