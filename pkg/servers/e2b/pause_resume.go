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

package e2b

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils/runtime"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

func (sc *Controller) PauseSandbox(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	id := r.PathValue("sandboxID")
	ctx := r.Context()
	log := klog.FromContext(ctx).WithValues("sandboxID", id)
	sbx, apiErr := sc.getSandboxOfUser(ctx, id)
	if apiErr != nil {
		return web.ApiResponse[struct{}]{}, apiErr
	}
	if state, reason := sbx.GetState(); state != v1alpha1.SandboxStateRunning {
		log.Info("skip pause sandbox: sandbox is not running", "state", state, "reason", reason)
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("Sandbox %s is not running", id),
		}
	}
	timeoutOptions := sc.buildPauseTimeoutOptions(sbx, time.Now())
	if err := sc.manager.PauseSandbox(ctx, sbx, infra.PauseOptions{
		Timeout: &timeoutOptions,
	}); err != nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Message: fmt.Sprintf("Failed to pause sandbox: %v", err),
		}
	}
	log.Info("sandbox paused", "timeout", timeoutOptions)
	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}

func (sc *Controller) buildPauseTimeoutOptions(sbx infra.Sandbox, now time.Time) timeout.Options {
	opts := sbx.GetTimeout()
	// Only set timeout if the sandbox has a timeout configured (not never-timeout)
	if !opts.ShutdownTime.IsZero() {
		// Paused sandboxes are kept indefinitely — there is no automatic deletion or time-to-live limit
		endAt := now.AddDate(1000, 0, 0)
		opts.ShutdownTime = endAt
		if !opts.PauseTime.IsZero() {
			opts.PauseTime = endAt
		}
	}
	return opts
}

// ResumeSandbox is DEPRECATED and kept only for old SDK compatibility.
//
// E2B exposes one "connect" behavior, but different SDK versions call different endpoints:
// - New SDK: calls ConnectSandbox directly.
// - Old SDK: first calls SetSandboxTimeout; that path returns 500 on this flow, then falls back to ResumeSandbox.
//
// Because ResumeSandbox is only used for the paused->running flow, it always applies the requested timeout directly.
// The running-sandbox "extend only" guard is intentionally implemented in ConnectSandbox only.
func (sc *Controller) ResumeSandbox(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	id := r.PathValue("sandboxID")
	ctx := r.Context()
	log := klog.FromContext(ctx).WithValues("sandboxID", id)

	request, apiErr := ParseSetTimeoutRequest(r, sc.maxTimeout)
	if apiErr != nil {
		apiErr.Code = http.StatusInternalServerError // E2B returns 500
		return web.ApiResponse[struct{}]{}, apiErr
	}

	sbx, apiErr := sc.getSandboxOfUser(ctx, id)
	if apiErr != nil {
		return web.ApiResponse[struct{}]{}, apiErr
	}
	autoPause, currentEndAt := ParseTimeout(sbx)
	baseline := sbx.GetTimeout()

	if state, reason := sbx.GetState(); state != v1alpha1.SandboxStatePaused {
		log.Info("skip resume sandbox: sandbox is not paused", "state", state, "reason", reason)
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("Sandbox %s is not paused", id),
		}
	}
	log.Info("resuming sandbox")
	if err := sc.manager.ResumeSandbox(ctx, sbx, infra.ResumeOptions{}); err != nil {
		code := http.StatusInternalServerError
		if errors.GetErrCode(err) == errors.ErrorBadRequest {
			code = http.StatusBadRequest
		}
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    code,
			Message: fmt.Sprintf("Failed to resume sandbox: %v", err),
		}
	}

	// Only set timeout if the sandbox has a timeout configured (not never-timeout).
	// After resume, the timeout is set strictly to the requested value (no extend-only merge).
	now := time.Now()
	if !currentEndAt.IsZero() {
		opts := sc.buildSetTimeoutOptions(autoPause, now, request.TimeoutSeconds)
		opts.Baseline = &baseline
		log.Info("sandbox resumed, resetting sandbox timeout", "timeout", opts)
		if _, err := sbx.SaveTimeoutWithPolicy(ctx, opts, timeout.UpdatePolicyBaselineAware); err != nil {
			return web.ApiResponse[struct{}]{}, &web.ApiError{
				Message: fmt.Sprintf("Failed to set sandbox timeout: %v", err),
			}
		}
	} else {
		log.Info("skip resetting timeout for never-timeout sandbox")
	}
	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}

func (sc *Controller) ConnectSandbox(r *http.Request) (web.ApiResponse[*models.Sandbox], *web.ApiError) {
	id := r.PathValue("sandboxID")
	ctx := r.Context()
	log := klog.FromContext(ctx).WithValues("sandboxID", id)
	log.Info("connecting sandbox")

	request, apiErr := ParseSetTimeoutRequest(r, sc.maxTimeout)
	if apiErr != nil {
		return web.ApiResponse[*models.Sandbox]{}, apiErr
	}

	sbx, apiErr := sc.getSandboxOfUser(ctx, id)
	if apiErr != nil {
		return web.ApiResponse[*models.Sandbox]{}, apiErr
	}
	// `state` is intentionally the pre-connect observation.
	// We only enforce the extend-only guard for sandboxes that were already running when Connect was called.
	// Paused->resume requests should always apply the requested timeout directly.
	state, pauseResumeReason := sbx.GetState()
	autoPause, currentEndAt := ParseTimeout(sbx)
	// Baseline reflects the pre-resume timeout observed in the same Get as `state`.
	// It is only consumed when preConnectState == Paused (BaselineAware policy).
	baseline := sbx.GetTimeout()

	// Step 1: Resuming the sandbox if it is paused
	statusCode := http.StatusOK
	if state == v1alpha1.SandboxStatePaused {
		log.Info("sandbox is paused, will resume it", "reason", pauseResumeReason)
		if err := sc.manager.ResumeSandbox(ctx, sbx, infra.ResumeOptions{}); err != nil {
			log.Error(err, "failed to resume sandbox")
			return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
				Code:    http.StatusInternalServerError,
				Message: fmt.Sprintf("Failed to resume sandbox: %v", err),
			}
		}
		statusCode = http.StatusCreated
		log.Info("sandbox resumed", "timeout", sbx.GetTimeout())
	} else {
		log.Info("sandbox is not paused, skip resuming", "state", state, "reason", pauseResumeReason)
	}

	// Step 2: Update the sandbox timeout
	log.Info("updating sandbox timeout")
	if err := sc.updateConnectTimeout(ctx, sbx, request.TimeoutSeconds, state, autoPause, currentEndAt, baseline); err != nil {
		log.Error(err, "failed to update sandbox timeout")
		return web.ApiResponse[*models.Sandbox]{}, err
	}
	log.Info("sandbox timeout updated")

	return web.ApiResponse[*models.Sandbox]{
		Code: statusCode,
		Body: sc.convertToE2BSandbox(sbx, runtime.GetAccessToken(sbx)),
	}, nil
}

// updateConnectTimeout updates the sandbox timeout after a Connect or Resume.
//
// Policy selection is based on preConnectState:
//   - Paused  → BaselineAware: the baseline captured at handler entry is guaranteed to be a
//     pre-resume value because SaveTimeoutWithPolicy is only reachable after Resume() returns,
//     and Resume() blocks until status.Phase == Running. Therefore any observation of
//     state ∈ {Paused, Resuming} proves no prior resumer has written a new timeout yet.
//     BaselineAware compares the current spec timeout against the baseline: if they match,
//     no concurrent writer has acted and the request may freely overwrite (Always semantics);
//     if they diverge, a concurrent writer already wrote, so we degrade to ExtendOnly to
//     preserve the longest timeout.
//   - Otherwise → ExtendOnly: the sandbox was already running, so we only allow extending
//     the existing timeout, never shrinking it.
func (sc *Controller) updateConnectTimeout(ctx context.Context, sbx infra.Sandbox, timeoutSeconds int, preConnectState string, autoPause bool, currentEndAt time.Time, baseline timeout.Options) *web.ApiError {
	log := klog.FromContext(ctx).WithValues("sandboxID", sbx.GetSandboxID())

	// Rule 1: Sandboxes without endAt are never-timeout, should not have their timeout updated
	if currentEndAt.IsZero() {
		log.Info("skip resetting timeout for never-timeout sandbox")
		return nil
	}

	now := time.Now()
	policy := timeout.UpdatePolicyExtendOnly
	opts := sc.buildSetTimeoutOptions(autoPause, now, timeoutSeconds)
	if preConnectState == v1alpha1.SandboxStatePaused {
		policy = timeout.UpdatePolicyBaselineAware
		opts.Baseline = &baseline
	}

	log.Info("saving timeout to sandbox", "timeout", opts, "currentEndAt", currentEndAt,
		"requestedEndAt", TimeAfterSeconds(now, timeoutSeconds), "requestedTimeoutSeconds", timeoutSeconds, "policy", policy)
	result, err := sbx.SaveTimeoutWithPolicy(ctx, opts, policy)
	if err != nil {
		return &web.ApiError{
			Message: fmt.Sprintf("Failed to set sandbox timeout: %v", err),
		}
	}
	if !result.Updated {
		log.Info("skip resetting timeout according to timeout update policy",
			"currentEndAt", currentEndAt,
			"requestedTimeoutSeconds", timeoutSeconds,
			"policy", policy)
	} else {
		log.Info("timeout updated", "requestedTimeoutSeconds", timeoutSeconds)
	}
	return nil
}
