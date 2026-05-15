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

package sandbox_manager

import (
	"context"
	"errors"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
	sandboxutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
)

const wakeMinimumTimeout = 5 * time.Minute

func (m *SandboxManager) WakeSandbox(ctx context.Context, sandboxID string) (result proxy.WakeResult, err error) {
	start := time.Now()
	defer func() {
		action := string(result.Action)
		if action == "" {
			action = "error"
		}
		sandboxWakeResponses.WithLabelValues(action).Inc()
		sandboxWakeDuration.Observe(time.Since(start).Seconds())
	}()

	// Wake uses only the canonical sandbox ID (`namespace--name`) from the URL.
	// The request does not carry or trust a separate namespace field.
	sbx, err := m.infra.GetClaimedSandbox(ctx, infra.GetClaimedSandboxOptions{SandboxID: sandboxID})
	if err != nil {
		if isSandboxNotFound(err) {
			return proxy.WakeResult{Action: proxy.WakeActionNotFound, Message: err.Error()}, nil
		}
		return proxy.WakeResult{}, err
	}

	policy, err := ParseWakeOnTrafficPolicy(sbx.GetAnnotations())
	if err != nil {
		if errors.Is(err, ErrAutoResumeDisabled) {
			return wakeResult(sbx, proxy.WakeActionAutoResumeDisabled, err.Error()), nil
		}
		if errors.Is(err, ErrInvalidAutoResumePolicy) {
			return wakeResult(sbx, proxy.WakeActionInvalidAutoResumePolicy, err.Error()), nil
		}
		return proxy.WakeResult{}, err
	}

	state, reason := sbx.GetState()
	if state == v1alpha1.SandboxStateRunning {
		return wakeResult(sbx, proxy.WakeActionAlreadyRunning, reason), nil
	}
	if state == v1alpha1.SandboxStateDead || sbx.GetDeletionTimestamp() != nil {
		return wakeResult(sbx, proxy.WakeActionGone, reason), nil
	}
	if state != v1alpha1.SandboxStatePaused {
		return wakeResult(sbx, proxy.WakeActionBadState, reason), nil
	}

	action, ok := classifyPausedWakeSandbox(sbx)
	if !ok {
		return wakeResult(sbx, action, reason), nil
	}

	baseline := sbx.GetTimeout()
	autoPause := !baseline.PauseTime.IsZero()
	currentEndAt := baseline.ShutdownTime
	if autoPause {
		currentEndAt = baseline.PauseTime
	}
	newEndAt := wakeNewEndAt(policy, currentEndAt, time.Now())

	err = m.ConnectOrWake(ctx, sbx, ConnectOrWakeInput{
		PreState:  v1alpha1.SandboxStatePaused,
		AutoPause: autoPause,
		PreEndAt:  currentEndAt,
		Baseline:  baseline,
		NewEndAt:  newEndAt,
	})
	if err != nil {
		action, actionErr := wakeActionForConnectError(ctx, err, sbx)
		if actionErr != nil {
			return proxy.WakeResult{}, actionErr
		}
		if action != "" {
			return wakeResult(sbx, action, err.Error()), nil
		}
		return proxy.WakeResult{}, err
	}
	if err = m.syncRoute(ctx, sbx, true); err != nil {
		klog.FromContext(ctx).Error(err, "failed to sync route with peers after wake", "sandbox", klog.KObj(sbx))
	}
	return wakeResult(sbx, proxy.WakeActionResumed, ""), nil
}

func isSandboxNotFound(err error) bool {
	return apierrors.IsNotFound(err) ||
		managererrors.GetErrCode(err) == managererrors.ErrorNotFound ||
		strings.Contains(err.Error(), "not found in cache")
}

func classifyPausedWakeSandbox(sbx infra.Sandbox) (proxy.WakeAction, bool) {
	raw := sbx.GetSandboxCR()
	if raw == nil {
		return proxy.WakeActionBadState, false
	}
	if raw.DeletionTimestamp != nil {
		return proxy.WakeActionGone, false
	}
	if raw.Status.Phase == v1alpha1.SandboxResuming {
		return proxy.WakeActionBadState, false
	}
	if raw.Status.Phase == v1alpha1.SandboxPaused {
		pauseCond := utils.GetSandboxCondition(&raw.Status, string(v1alpha1.SandboxConditionPaused))
		if pauseCond == nil || pauseCond.Status != metav1.ConditionTrue {
			return proxy.WakeActionPausing, false
		}
	}
	if resumable, _ := sandboxutils.IsSandboxResumable(raw); !resumable {
		return proxy.WakeActionBadState, false
	}
	return "", true
}

func wakeNewEndAt(policy Policy, currentEndAt time.Time, now time.Time) time.Time {
	if policy.Form == PolicyFormNever || currentEndAt.IsZero() {
		return time.Time{}
	}
	duration := policy.Duration
	if duration < wakeMinimumTimeout {
		duration = wakeMinimumTimeout
	}
	return now.Add(duration)
}

func wakeActionForConnectError(ctx context.Context, err error, sbx infra.Sandbox) (proxy.WakeAction, error) {
	if managererrors.GetErrCode(err) != managererrors.ErrorConflict {
		return "", err
	}
	if refreshErr := sbx.InplaceRefresh(ctx, false); refreshErr != nil {
		return "", refreshErr
	}
	state, _ := sbx.GetState()
	if state == v1alpha1.SandboxStateRunning {
		return proxy.WakeActionAlreadyRunning, nil
	}
	if state == v1alpha1.SandboxStateDead || sbx.GetDeletionTimestamp() != nil {
		return proxy.WakeActionGone, nil
	}
	action, ok := classifyPausedWakeSandbox(sbx)
	if !ok && action != "" {
		return action, nil
	}
	return proxy.WakeActionBadState, nil
}

func wakeResult(sbx infra.Sandbox, action proxy.WakeAction, message string) proxy.WakeResult {
	state, _ := sbx.GetState()
	return proxy.WakeResult{
		Action:          action,
		State:           state,
		ResourceVersion: sbx.GetResourceVersion(),
		Message:         message,
	}
}
