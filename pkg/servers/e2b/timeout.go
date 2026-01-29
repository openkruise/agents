package e2b

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"k8s.io/klog/v2"
)

// SetSandboxTimeout sets the timeout of a claimed sandbox
func (sc *Controller) SetSandboxTimeout(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	err := sc.setSandboxTimeout(r)
	if err != nil {
		if err.Code != http.StatusNotFound {
			// Just to follow E2B spec, I don't know why it is designed
			err.Code = http.StatusInternalServerError
		}
		return web.ApiResponse[struct{}]{}, err
	}
	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}

func (sc *Controller) setSandboxTimeout(r *http.Request) *web.ApiError {
	ctx := r.Context()
	log := klog.FromContext(ctx)
	now := time.Now()

	request, apiError := ParseSetTimeoutRequest(r, sc.maxTimeout)
	if apiError != nil {
		return apiError
	}

	id := r.PathValue("sandboxID")
	sbx, apiErr := sc.getSandboxOfUser(ctx, id)
	if apiErr != nil {
		return apiErr
	}

	state, reason := sbx.GetState()
	if state != v1alpha1.SandboxStateRunning {
		log.Info("cannot set sandbox timeout for sandbox not running", "name", sbx.GetName(), "state", state, "reason", reason)
		return &web.ApiError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("sandbox %s is not running", sbx.GetName()),
		}
	}

	autoPause, _ := ParseTimeout(sbx)
	opts := sc.buildSetTimeoutOptions(autoPause, now, request.TimeoutSeconds)
	if err := sbx.SaveTimeout(ctx, opts); err != nil {
		return &web.ApiError{
			Message: fmt.Sprintf("Failed to set sandbox timeout: %v", err),
		}
	}

	log.Info("set sandbox timeout success", "id", id, "timeout", request.TimeoutSeconds, "options", opts)
	return nil
}

func ParseSetTimeoutRequest(r *http.Request, maxTimeout int) (models.SetTimeoutRequest, *web.ApiError) {
	var request models.SetTimeoutRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return request, &web.ApiError{
			Message: err.Error(),
		}
	}
	if request.TimeoutSeconds <= 0 || request.TimeoutSeconds > maxTimeout {
		return request, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("timeout should between 0 and %d", maxTimeout),
		}
	}
	return request, nil
}
