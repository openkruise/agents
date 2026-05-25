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
	"errors"
	"fmt"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/api/v1alpha1"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
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
	timeoutOptions := sc.buildPauseTimeoutOptions(sbx, time.Now())
	if err := sc.manager.PauseSandbox(ctx, sbx, infra.PauseOptions{
		Timeout: &timeoutOptions,
	}); err != nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    pauseSandboxErrorCode(err),
			Message: fmt.Sprintf("Failed to pause sandbox: %v", err),
		}
	}
	log.Info("sandbox paused", "timeout", timeoutOptions)
	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}

func pauseSandboxErrorCode(err error) int {
	if apierrors.IsNotFound(err) {
		return http.StatusNotFound
	}
	if managererrors.GetErrCode(err) == managererrors.ErrorConflict ||
		errors.Is(err, cacheutils.ErrWaitTaskConflict) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// resumeSandboxErrorCode mirrors pauseSandboxErrorCode. The E2B /resume spec
// permits 401 / 404 / 409 / 500 (no 400), so non-pausable / non-resumable
// state errors from the infra layer surface as 409 rather than 400.
func resumeSandboxErrorCode(err error) int {
	if apierrors.IsNotFound(err) {
		return http.StatusNotFound
	}
	if managererrors.GetErrCode(err) == managererrors.ErrorConflict ||
		errors.Is(err, cacheutils.ErrWaitTaskConflict) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
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
// The post-Resume timeout write reuses updateConnectTimeout with UpdatePolicyExtendOnly,
// so the running-sandbox "extend only" semantics apply here as well: a shorter
// requested timeout never shrinks an existing later deadline.
//
// No handler-level state guard is enforced — the infra layer (IsSandboxResumable +
// first-writer-wins retryUpdate + Ready-cond idempotent short-circuit) is
// authoritative. A handler guard based on a stale Get would falsely 409 the
// second of two concurrent Resume requests, mirroring the bug fixed for
// PauseSandbox in PR #422.
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
	state, _ := sbx.GetState()

	effectiveTimeout := sc.getEffectivePauseTimeSeconds(log, request.TimeoutSeconds, true, !currentEndAt.IsZero())
	resumeOpts := sc.buildResumeOpts(autoPause, time.Now(), effectiveTimeout, !currentEndAt.IsZero())
	log.Info("resuming sandbox")
	if err := sc.manager.ResumeSandbox(ctx, sbx, resumeOpts); err != nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    resumeSandboxErrorCode(err),
			Message: fmt.Sprintf("Failed to resume sandbox: %v", err),
		}
	}

	if apiErr := sc.updateConnectTimeout(ctx, sbx, effectiveTimeout, state, autoPause, currentEndAt); apiErr != nil {
		return web.ApiResponse[struct{}]{}, apiErr
	}
	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}

// getEffectivePauseTimeSeconds enforces a minimum timeout floor when the request
// will Resume a timed Paused sandbox. Without the floor, a very short timeout
// could expire while the sandbox is still resuming, causing it to be deleted or
// hibernated mid-Resume. The floor is skipped for never-timeout sandboxes
// (hasDeadline == false) since they carry no deadline.
func (sc *Controller) getEffectivePauseTimeSeconds(log klog.Logger, requested int, paused, hasDeadline bool) int {
	if !paused || !hasDeadline || requested >= sc.minResumeTimeoutValue {
		return requested
	}
	log.Info("connect-on-paused timeout floor applied",
		"requestedSeconds", requested,
		"effectiveSeconds", sc.minResumeTimeoutValue,
		"reason", "request shorter than --e2b-min-resume-timeout")
	return sc.minResumeTimeoutValue
}

// buildResumeOpts builds ResumeOptions whose placeholder Timeout is written
// atomically with Spec.Paused=false inside Resume() — closing the auto-pause
// race. Never-timeout sandboxes (hasDeadline == false) get nil so Resume does
// not convert them into timed sandboxes.
func (sc *Controller) buildResumeOpts(autoPause bool, now time.Time, effectiveTimeout int, hasDeadline bool) infra.ResumeOptions {
	if !hasDeadline {
		return infra.ResumeOptions{}
	}
	placeholder := sc.buildSetTimeoutOptions(autoPause, now, effectiveTimeout)
	return infra.ResumeOptions{Timeout: &placeholder}
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
	// `state` is the pre-connect observation: the extend-only guard at
	// updateConnectTimeout applies to sandboxes that were already running,
	// while Paused→resume requests apply the requested timeout (post-floor)
	// atomically inside Resume.
	state, pauseResumeReason := sbx.GetState()
	autoPause, currentEndAt := ParseTimeout(sbx)

	paused := state == v1alpha1.SandboxStatePaused
	effectiveTimeout := sc.getEffectivePauseTimeSeconds(log, request.TimeoutSeconds, paused, !currentEndAt.IsZero())

	// Step 1: Resume the sandbox if it is paused, atomically writing the
	// placeholder timeout for timed sandboxes.
	statusCode := http.StatusOK
	if paused {
		log.Info("sandbox is paused, will resume it", "reason", pauseResumeReason)
		resumeOpts := sc.buildResumeOpts(autoPause, time.Now(), effectiveTimeout, !currentEndAt.IsZero())
		if err := sc.manager.ResumeSandbox(ctx, sbx, resumeOpts); err != nil {
			log.Error(err, "failed to resume sandbox")
			code := http.StatusInternalServerError
			if managererrors.GetErrCode(err) == managererrors.ErrorConflict {
				code = http.StatusBadRequest
			}
			return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
				Code:    code,
				Message: fmt.Sprintf("Failed to resume sandbox: %v", err),
			}
		}
		statusCode = http.StatusCreated
		log.Info("sandbox resumed", "timeout", sbx.GetTimeout())
	} else {
		log.Info("sandbox is not paused, skip resuming", "state", state, "reason", pauseResumeReason)
	}

	// Step 2: Update the sandbox timeout with the effective (post-floor) value.
	log.Info("updating sandbox timeout")
	if err := sc.updateConnectTimeout(ctx, sbx, effectiveTimeout, state, autoPause, currentEndAt); err != nil {
		log.Error(err, "failed to update sandbox timeout")
		return web.ApiResponse[*models.Sandbox]{}, err
	}
	log.Info("sandbox timeout updated")

	return web.ApiResponse[*models.Sandbox]{
		Code: statusCode,
		Body: sc.convertToE2BSandbox(sbx, sandboxutils.GetAccessToken(sbx)),
	}, nil
}

// updateConnectTimeout writes the post-Resume / running-sandbox timeout under
// ExtendOnly so the placeholder written by Resume() naturally extends to the
// post-Resume value, and concurrent-writer races resolve to the longer
// timeout. Short-circuits for never-timeout sandboxes.
func (sc *Controller) updateConnectTimeout(ctx context.Context, sbx infra.Sandbox, timeoutSeconds int, preConnectState string, autoPause bool, currentEndAt time.Time) *web.ApiError {
	log := klog.FromContext(ctx).WithValues("sandboxID", sbx.GetSandboxID())

	if currentEndAt.IsZero() {
		log.Info("skip resetting timeout for never-timeout sandbox")
		return nil
	}

	now := time.Now()
	opts := sc.buildSetTimeoutOptions(autoPause, now, timeoutSeconds)

	log.Info("saving timeout to sandbox", "timeout", opts, "currentEndAt", currentEndAt,
		"requestedEndAt", TimeAfterSeconds(now, timeoutSeconds), "requestedTimeoutSeconds", timeoutSeconds,
		"policy", timeout.UpdatePolicyExtendOnly, "preConnectState", preConnectState)
	result, err := sbx.SaveTimeoutWithPolicy(ctx, opts, timeout.UpdatePolicyExtendOnly)
	if err != nil {
		return &web.ApiError{
			Message: fmt.Sprintf("Failed to set sandbox timeout: %v", err),
		}
	}
	if !result.Updated {
		log.Info("skip resetting timeout according to ExtendOnly policy",
			"currentEndAt", currentEndAt,
			"requestedTimeoutSeconds", timeoutSeconds)
	} else {
		log.Info("timeout updated", "requestedTimeoutSeconds", timeoutSeconds)
	}
	return nil
}
