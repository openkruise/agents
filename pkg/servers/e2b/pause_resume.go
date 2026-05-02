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
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils/runtime"
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

func (sc *Controller) buildPauseTimeoutOptions(sbx infra.Sandbox, now time.Time) infra.TimeoutOptions {
	opts := sbx.GetTimeout()
	// Only set timeout if the sandbox has a timeout configured (not never-timeout)
	if !opts.ShutdownTime.IsZero() {
		// Paused sandboxes are kept indefinitely — there is no automatic deletion or time-to-live limit
		timeout := now.AddDate(1000, 0, 0)
		opts.ShutdownTime = timeout
		if !opts.PauseTime.IsZero() {
			opts.PauseTime = timeout
		}
	}
	return opts
}

// buildResumeTimeoutOptions returns the TimeoutOptions to be applied after a
// successful resume.
//
// When the sandbox was never-timeout before resume (currentEndAt is zero),
// this intentionally returns an empty infra.TimeoutOptions{}. Downstream,
// SaveTimeout / setTimeout treat zero time.Time fields as "clear this field"
// (see setTimeout's contract), which keeps the post-resume sandbox in the
// never-timeout state. In other words, propagating zero values down the stack
// is the deliberate way for this layer to express "this sandbox has no
// timeout"; do not replace the empty struct with nil-skip semantics.
func (sc *Controller) buildResumeTimeoutOptions(autoPause bool, now time.Time, timeoutSeconds int, currentEndAt time.Time) infra.TimeoutOptions {
	if currentEndAt.IsZero() { // no end at means never timeout
		return infra.TimeoutOptions{}
	}
	return sc.buildSetTimeoutOptions(autoPause, now, timeoutSeconds)
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

	if state, reason := sbx.GetState(); state != v1alpha1.SandboxStatePaused {
		log.Info("skip resume sandbox: sandbox is not paused", "state", state, "reason", reason)
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("Sandbox %s is not paused", id),
		}
	}
	now := time.Now()
	timeoutOptions := sc.buildResumeTimeoutOptions(autoPause, now, request.TimeoutSeconds, currentEndAt)
	log.Info("resuming sandbox", "timeout", timeoutOptions)
	if err := sc.manager.ResumeSandbox(ctx, sbx, infra.ResumeOptions{Timeout: &timeoutOptions}); err != nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Message: fmt.Sprintf("Failed to resume sandbox: %v", err),
		}
	}

	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}

func (sc *Controller) ConnectSandbox(r *http.Request) (web.ApiResponse[*models.Sandbox], *web.ApiError) {
	id := r.PathValue("sandboxID")
	ctx := r.Context()
	log := klog.FromContext(ctx).WithValues("sandboxID", id)

	request, apiErr := ParseSetTimeoutRequest(r, sc.maxTimeout)
	if apiErr != nil {
		return web.ApiResponse[*models.Sandbox]{}, apiErr
	}

	sbx, apiErr := sc.getSandboxOfUser(ctx, id)
	if apiErr != nil {
		return web.ApiResponse[*models.Sandbox]{}, apiErr
	}
	// `state` is intentionally the pre-connect snapshot.
	// We only enforce the extend-only guard for sandboxes that were already running when Connect was called.
	// Paused->resume requests should always apply the requested timeout directly.
	state, pauseResumeReason := sbx.GetState()
	autoPause, currentEndAt := ParseTimeout(sbx)

	// Step 1: Resuming the sandbox if it is paused
	statusCode := http.StatusOK
	if state == v1alpha1.SandboxStatePaused {
		// TODO: remove the check after Resume refactor is finished
		log.Info("sandbox is paused, will resume it", "reason", pauseResumeReason)
		timeoutOptions := sc.buildResumeTimeoutOptions(autoPause, time.Now(), request.TimeoutSeconds, currentEndAt)
		if err := sc.manager.ResumeSandbox(ctx, sbx, infra.ResumeOptions{Timeout: &timeoutOptions}); err != nil {
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
	if state != v1alpha1.SandboxStatePaused {
		log.Info("updating sandbox timeout")
		if err := sc.updateConnectTimeout(ctx, sbx, request.TimeoutSeconds, state, autoPause, currentEndAt); err != nil {
			log.Error(err, "failed to update sandbox timeout")
			return web.ApiResponse[*models.Sandbox]{}, err
		}
		log.Info("sandbox timeout updated")
	}

	return web.ApiResponse[*models.Sandbox]{
		Code: statusCode,
		Body: sc.convertToE2BSandbox(sbx, runtime.GetAccessToken(sbx)),
	}, nil
}

func (sc *Controller) updateConnectTimeout(ctx context.Context, sbx infra.Sandbox, timeoutSeconds int, preConnectState string, autoPause bool, currentEndAt time.Time) *web.ApiError {
	log := klog.FromContext(ctx).WithValues("sandboxID", sbx.GetSandboxID())

	// Rule 1: Sandboxes without endAt are never-timeout, should not have their timeout updated
	if currentEndAt.IsZero() {
		log.Info("skip resetting timeout for never-timeout sandbox")
		return nil
	}

	now := time.Now()
	requestedEndAt := TimeAfterSeconds(now, timeoutSeconds)
	// Rule 2: For running sandboxes, the timeout will update only if the new timeout is longer than the existing one.
	if preConnectState == v1alpha1.SandboxStateRunning && currentEndAt.After(requestedEndAt) {
		log.Info("skip resetting timeout for running sandbox for shorter timeout",
			"currentEndAt", currentEndAt,
			"requestedEndAt", requestedEndAt,
			"requestedTimeoutSeconds", timeoutSeconds)
		return nil
	}

	opts := sc.buildSetTimeoutOptions(autoPause, now, timeoutSeconds)
	log.Info("saving timeout to sandbox", "timeout", opts, "currentEndAt", currentEndAt,
		"requestedEndAt", requestedEndAt, "requestedTimeoutSeconds", timeoutSeconds)
	if err := sbx.SaveTimeout(ctx, opts); err != nil {
		return &web.ApiError{
			Message: fmt.Sprintf("Failed to set sandbox timeout: %v", err),
		}
	}
	return nil
}
