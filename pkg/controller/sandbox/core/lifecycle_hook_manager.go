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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/tracing"
	"github.com/openkruise/agents/pkg/utils"
)

const (
	lifecycleHookMaxAttempts       = 3
	lifecycleHookNoSlotRequeue     = time.Second
	lifecycleHookFirstRetryBackoff = time.Second
)

type lifecycleHookKind string

const (
	prePauseHook   lifecycleHookKind = "prePause"
	postResumeHook lifecycleHookKind = "postResume"
)

type lifecycleHookRetryState struct {
	attempts    int
	nextRetryAt time.Time
}

type LifecycleHookProgress struct {
	Terminal         bool
	ConditionChanged bool
	RequeueAfter     time.Duration
}

// LifecycleHookManager owns the bounded execution capacity and in-memory retry
// bookkeeping shared by PrePause and PostResume. Persisted Conditions remain
// the source of truth for terminal operation results; the local map only keeps
// retry timing and counts while this controller process stays alive.
type LifecycleHookManager struct {
	hookFunc LifecycleHookFunc
	recorder record.EventRecorder
	slots    chan struct{}
	now      func() time.Time

	mu      sync.Mutex
	retries map[types.UID]lifecycleHookRetryState
}

func NewLifecycleHookManager(capacity int, hookFunc LifecycleHookFunc, recorder record.EventRecorder) *LifecycleHookManager {
	if capacity < 1 {
		capacity = 1
	}
	return &LifecycleHookManager{
		hookFunc: hookFunc,
		recorder: recorder,
		slots:    make(chan struct{}, capacity),
		now:      time.Now,
		retries:  make(map[types.UID]lifecycleHookRetryState),
	}
}

func (m *LifecycleHookManager) Reset(uid types.UID) {
	if m == nil || uid == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.retries, uid)
}

func (m *LifecycleHookManager) BeginPrePause(box *agentsv1alpha1.Sandbox, status *agentsv1alpha1.SandboxStatus) {
	if m == nil || box == nil || status == nil {
		return
	}
	m.Reset(box.UID)
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

func (m *LifecycleHookManager) BeginPostResume(box *agentsv1alpha1.Sandbox, status *agentsv1alpha1.SandboxStatus) {
	if m == nil || box == nil || status == nil {
		return
	}
	m.Reset(box.UID)
	utils.RemoveSandboxCondition(status, string(agentsv1alpha1.SandboxConditionPostResumeHook))
	if box.Spec.Lifecycle != nil && box.Spec.Lifecycle.PostResume != nil {
		utils.SetSandboxCondition(status, newLifecycleHookCondition(agentsv1alpha1.SandboxConditionPostResumeHook,
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
	m.removeConditionAndState(box, status, postResumeHook)
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
		changed := m.removeConditionAndState(box, newStatus, kind)
		return LifecycleHookProgress{Terminal: true, ConditionChanged: changed}
	}

	conditionType := lifecycleHookConditionType(kind)
	cond := utils.GetSandboxCondition(newStatus, string(conditionType))
	if cond == nil {
		utils.SetSandboxCondition(newStatus, newLifecycleHookCondition(conditionType, agentsv1alpha1.SandboxLifecycleHookReasonPending,
			metav1.ConditionFalse, box.Generation, "waiting to execute lifecycle hook"))
		return LifecycleHookProgress{ConditionChanged: true}
	}
	if lifecycleHookConditionTerminal(cond) {
		m.Reset(box.UID)
		return LifecycleHookProgress{Terminal: true}
	}

	if kind == prePauseHook && (pod == nil || !pod.DeletionTimestamp.IsZero() || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded) {
		message := "hook skipped because the Sandbox Pod is not executable; pause continues"
		utils.SetSandboxCondition(newStatus, newLifecycleHookCondition(conditionType, agentsv1alpha1.SandboxLifecycleHookReasonSkipped,
			metav1.ConditionFalse, box.Generation, message))
		m.Reset(box.UID)
		m.event(box, corev1.EventTypeWarning, string(conditionType)+agentsv1alpha1.SandboxLifecycleHookReasonSkipped, message)
		return LifecycleHookProgress{Terminal: true, ConditionChanged: true}
	}

	state := m.retryState(box.UID)
	now := m.now()
	if now.Before(state.nextRetryAt) {
		return LifecycleHookProgress{RequeueAfter: state.nextRetryAt.Sub(now)}
	}

	if cond.Reason != agentsv1alpha1.SandboxLifecycleHookReasonRunning {
		attempt := state.attempts + 1
		utils.SetSandboxCondition(newStatus, newLifecycleHookCondition(conditionType, agentsv1alpha1.SandboxLifecycleHookReasonRunning,
			metav1.ConditionFalse, box.Generation, fmt.Sprintf("executing attempt %d", attempt)))
		return LifecycleHookProgress{ConditionChanged: true}
	}

	if !m.tryAcquire() {
		return LifecycleHookProgress{RequeueAfter: lifecycleHookNoSlotRequeue}
	}
	defer m.release()

	state = m.retryState(box.UID)
	state.attempts++
	spanName := tracing.SpanControllerPostResumeHook
	if kind == prePauseHook {
		spanName = tracing.SpanControllerPrePauseHook
	}
	hookCtx, span := tracing.StartControllerSpan(ctx, spanName)
	exitCode, _, _, hookErr := m.hookFunc(hookCtx, box, action)
	tracing.EndSpan(hookCtx, span, hookErr)
	if hookErr == nil && exitCode == 0 {
		message := fmt.Sprintf("attempt %d succeeded", state.attempts)
		utils.SetSandboxCondition(newStatus, newLifecycleHookCondition(conditionType, agentsv1alpha1.SandboxLifecycleHookReasonSucceeded,
			metav1.ConditionTrue, box.Generation, message))
		m.Reset(box.UID)
		m.event(box, corev1.EventTypeNormal, string(conditionType)+agentsv1alpha1.SandboxLifecycleHookReasonSucceeded, message)
		return LifecycleHookProgress{Terminal: true, ConditionChanged: true}
	}

	summary := lifecycleHookFailureSummary(exitCode, hookErr)
	if state.attempts >= lifecycleHookMaxAttempts {
		message := fmt.Sprintf("attempt %d failed: %s; lifecycle transition continues", state.attempts, summary)
		utils.SetSandboxCondition(newStatus, newLifecycleHookCondition(conditionType, agentsv1alpha1.SandboxLifecycleHookReasonFailed,
			metav1.ConditionFalse, box.Generation, message))
		m.Reset(box.UID)
		m.event(box, corev1.EventTypeWarning, string(conditionType)+agentsv1alpha1.SandboxLifecycleHookReasonFailed, message)
		return LifecycleHookProgress{Terminal: true, ConditionChanged: true}
	}

	now = m.now()
	backoff := lifecycleHookFirstRetryBackoff * time.Duration(state.attempts)
	nextRetryAt := now.Add(backoff)
	state.nextRetryAt = nextRetryAt
	m.setRetryState(box.UID, state)
	message := fmt.Sprintf("attempt %d failed: %s; retrying", state.attempts, summary)
	utils.SetSandboxCondition(newStatus, newLifecycleHookCondition(conditionType, agentsv1alpha1.SandboxLifecycleHookReasonPending,
		metav1.ConditionFalse, box.Generation, message))
	return LifecycleHookProgress{ConditionChanged: true, RequeueAfter: nextRetryAt.Sub(now)}
}

func (m *LifecycleHookManager) retryState(uid types.UID) lifecycleHookRetryState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.retries[uid]
}

func (m *LifecycleHookManager) setRetryState(uid types.UID, state lifecycleHookRetryState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retries[uid] = state
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

func (m *LifecycleHookManager) removeConditionAndState(box *agentsv1alpha1.Sandbox, status *agentsv1alpha1.SandboxStatus, kind lifecycleHookKind) bool {
	if m == nil || box == nil || status == nil {
		return false
	}
	conditionType := string(lifecycleHookConditionType(kind))
	changed := utils.GetSandboxCondition(status, conditionType) != nil
	utils.RemoveSandboxCondition(status, conditionType)
	m.Reset(box.UID)
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
	case err != nil:
		return "execution error"
	default:
		return fmt.Sprintf("process exited with code %d", exitCode)
	}
}
