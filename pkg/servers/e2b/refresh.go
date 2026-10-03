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

	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	timeoututils "github.com/openkruise/agents/pkg/utils/timeout"
)

func (sc *Controller) RefreshSandbox(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	ctx := r.Context()

	var req models.RefreshSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: err.Error(),
		}
	}

	if req.Duration < 0 || req.Duration > 3600 {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "duration should be between 0 and 3600",
		}
	}

	id := r.PathValue("sandboxID")

	sbx, apiErr := sc.getSandboxOfUser(ctx, id)
	if apiErr != nil {
		return web.ApiResponse[struct{}]{}, apiErr
	}

	autoPause, timeoutAt := ParseTimeout(sbx)

	// Preserve never-timeout behavior.
	if timeoutAt.IsZero() {
		return web.ApiResponse[struct{}]{
			Code: http.StatusNoContent,
		}, nil
	}

	opts := sc.buildSetTimeoutOptions(
		autoPause,
		time.Now(),
		req.Duration,
	)

	if _, err := sbx.SaveTimeoutWithPolicy(
		ctx,
		opts,
		timeoututils.UpdatePolicyExtendOnly,
	); err != nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Message: fmt.Sprintf("failed to refresh sandbox: %v", err),
		}
	}

	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}
