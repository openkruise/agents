package e2b

import (
	"fmt"
	"net/http"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"k8s.io/klog/v2"
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
	if err := sbx.Pause(ctx, infra.PauseOptions{
		TimeoutOptions: timeoutOptions,
	}); err != nil {
	// FIXME
		if err := sc.manager.PauseSandbox(ctx, sbx); err != nil {
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
	timeout := TimeAfterSeconds(now, sc.maxTimeout)
	opts := sbx.GetTimeout()
	opts.ShutdownTime = timeout
	if !opts.PauseTime.IsZero() {
		opts.PauseTime = timeout
	}
	return opts
}

func (sc *Controller) ResumeSandbox(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	id := r.PathValue("sandboxID")
	ctx := r.Context()
	log := klog.FromContext(ctx).WithValues("sandboxID", id)

	log.Info("resetting sandbox timeout")
	apiError := sc.setSandboxTimeout(r, true)
	if apiError != nil {
		if apiError.Code != http.StatusNotFound && apiError.Code != http.StatusConflict {
			// Just to follow E2B spec, I don't know why it is designed
			apiError.Code = http.StatusInternalServerError
		}
		return web.ApiResponse[struct{}]{}, apiError
	}

	sbx, apiErr := sc.getSandboxOfUser(ctx, id)
	if apiErr != nil {
		return web.ApiResponse[struct{}]{}, apiErr
	}
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
	log.Info("sandbox resumed")
	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}

func (sc *Controller) ConnectSandbox(r *http.Request) (web.ApiResponse[*models.Sandbox], *web.ApiError) {
	id := r.PathValue("sandboxID")
	ctx := r.Context()
	log := klog.FromContext(ctx).WithValues("sandboxID", id)

	log.Info("resetting sandbox timeout")
	apiError := sc.setSandboxTimeout(r, true)
	if apiError != nil {
		return web.ApiResponse[*models.Sandbox]{}, apiError
	}

	sbx, apiErr := sc.getSandboxOfUser(ctx, id)
	if apiErr != nil {
		return web.ApiResponse[*models.Sandbox]{}, apiErr
	}
	var statusCode = http.StatusOK
	if state, reason := sbx.GetState(); state == v1alpha1.SandboxStatePaused {
		log.Info("sandbox is paused, will resume it", "reason", reason)
		if err := sc.manager.ResumeSandbox(ctx, sbx); err != nil {
			log.Error(err, "failed to resume sandbox")
			return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
				Message: fmt.Sprintf("Failed to resume sandbox: %v", err),
			}
		}
		statusCode = http.StatusCreated
		log.Info("sandbox resumed", "timeout", sbx.GetTimeout())
	} else {
		log.Info("sandbox is not paused, skip resuming", "state", state, "reason", reason)
	}

	return web.ApiResponse[*models.Sandbox]{
		Code: statusCode,
		Body: sc.convertToE2BSandbox(sbx, sbx.GetAnnotations()[v1alpha1.AnnotationEnvdAccessToken]),
	}, nil
}
