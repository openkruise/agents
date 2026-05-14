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
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	timeoututils "github.com/openkruise/agents/pkg/utils/timeout"
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

	autoPause, timeout := ParseTimeout(sbx)
	if !timeout.IsZero() {
		opts := sc.buildSetTimeoutOptions(autoPause, now, request.TimeoutSeconds)
		if _, err := sbx.SaveTimeoutWithPolicy(ctx, opts, timeoututils.UpdatePolicyAlways); err != nil {
			return &web.ApiError{
				Message: fmt.Sprintf("Failed to set sandbox timeout: %v", err),
			}
		}
		log.Info("set sandbox timeout success", "id", id, "timeout", request.TimeoutSeconds, "options", opts)
	} else {
		log.Info("skip set sandbox timeout")
	}

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
