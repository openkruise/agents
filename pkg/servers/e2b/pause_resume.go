package e2b

import (
	"fmt"
	"net/http"
	"time"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
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

// shouldSkipRunningSandboxTimeoutUpdate implements the E2B rule for already-running sandboxes:
// only persist a new timeout when it extends the current effective deadline (strictly later than currentEndAt).
// currentEndAt is the effective EndAt from ParseTimeout (PauseTime when auto-pause, else ShutdownTime).
func shouldSkipRunningSandboxTimeoutUpdate(currentEndAt, now time.Time, requestTimeoutSeconds int) (skip bool, requestedEndAt time.Time) {
	requestedEndAt = TimeAfterSeconds(now, requestTimeoutSeconds)
	if currentEndAt.IsZero() {
		return false, requestedEndAt
	}
	return !requestedEndAt.After(currentEndAt), requestedEndAt
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
	autoPause, timeout := ParseTimeout(sbx)

	if state, reason := sbx.GetState(); state != v1alpha1.SandboxStatePaused {
		log.Info("skip resume sandbox: sandbox is not paused", "state", state, "reason", reason)
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("Sandbox %s is not paused", id),
		}
	}
	log.Info("resuming sandbox")
	if err := sc.manager.ResumeSandbox(ctx, sbx); err != nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Message: fmt.Sprintf("Failed to resume sandbox: %v", err),
		}
	}

	// Only set timeout if the sandbox has a timeout configured (not never-timeout).
	// After resume, the timeout is set strictly to the requested value (no extend-only merge).
	now := time.Now()
	if !timeout.IsZero() {
		opts := sc.buildSetTimeoutOptions(autoPause, now, request.TimeoutSeconds)
		log.Info("sandbox resumed, resetting sandbox timeout", "timeout", opts)
		if err := sbx.SaveTimeout(ctx, opts); err != nil {
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

	request, apiErr := ParseSetTimeoutRequest(r, sc.maxTimeout)
	if apiErr != nil {
		return web.ApiResponse[*models.Sandbox]{}, apiErr
	}

	sbx, apiErr := sc.getSandboxOfUser(ctx, id)
	if apiErr != nil {
		return web.ApiResponse[*models.Sandbox]{}, apiErr
	}
	autoPause, currentEndAt := ParseTimeout(sbx)
	state, pauseResumeReason := sbx.GetState()
	// `state` is intentionally the pre-connect snapshot.
	// We only enforce the extend-only guard for sandboxes that were already running when Connect was called.
	// Paused->resume requests should always apply the requested timeout directly.

	statusCode := http.StatusOK
	if state == v1alpha1.SandboxStatePaused {
		log.Info("sandbox is paused, will resume it", "reason", pauseResumeReason)
		if err := sc.manager.ResumeSandbox(ctx, sbx); err != nil {
			log.Error(err, "failed to resume sandbox")
			return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
				Message: fmt.Sprintf("Failed to resume sandbox: %v", err),
			}
		}
		statusCode = http.StatusCreated
		log.Info("sandbox resumed", "timeout", sbx.GetTimeout())
	} else {
		log.Info("sandbox is not paused, skip resuming", "state", state, "reason", pauseResumeReason)
	}

	// Only set timeout if the sandbox has a timeout configured (not never-timeout).
	// Running: do not shorten or keep-equal TTL on connect (see shouldSkipRunningSandboxTimeoutUpdate).
	// Paused→resumed: use the requested timeout directly (no extend-only merge).
	timeoutEnabled := !currentEndAt.IsZero()
	now := time.Now()
	if timeoutEnabled && state == v1alpha1.SandboxStateRunning {
		skip, requestedEndAt := shouldSkipRunningSandboxTimeoutUpdate(currentEndAt, now, request.TimeoutSeconds)
		if skip {
			timeoutEnabled = false
			log.Info("skip resetting timeout for running sandbox",
				"currentEndAt", currentEndAt,
				"requestedEndAt", requestedEndAt,
				"requestedTimeoutSeconds", request.TimeoutSeconds)
		}
	}

	if timeoutEnabled {
		opts := sc.buildSetTimeoutOptions(autoPause, now, request.TimeoutSeconds)
		log.Info("resetting timeout", "timeout", opts)
		if err := sbx.SaveTimeout(ctx, opts); err != nil {
			return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
				Message: fmt.Sprintf("Failed to set sandbox timeout: %v", err),
			}
		}
	} else if currentEndAt.IsZero() {
		log.Info("skip resetting timeout for never-timeout sandbox")
	}
	return web.ApiResponse[*models.Sandbox]{
		Code: statusCode,
		Body: sc.convertToE2BSandbox(sbx, sbx.GetAccessToken()),
	}, apiErr
}
