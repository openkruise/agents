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

package proxy

import (
	"context"
	"fmt"
	"net/http"

	"github.com/openkruise/agents/pkg/servers/web"
)

type WakeFunc func(ctx context.Context, id string) (WakeResult, error)

func (s *Server) handleWake(r *http.Request) (web.ApiResponse[WakeResult], *web.ApiError) {
	if s.wake == nil {
		return web.ApiResponse[WakeResult]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: "wake function is not configured",
		}
	}

	result, err := s.wake(r.Context(), r.PathValue("sandboxID"))
	if err != nil {
		return web.ApiResponse[WakeResult]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: fmt.Sprintf("wake sandbox failed: %v", err),
		}
	}

	status, ok := wakeActionHTTPStatus(result.Action)
	if !ok {
		return web.ApiResponse[WakeResult]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: fmt.Sprintf("unsupported wake action: %s", result.Action),
		}
	}
	return web.ApiResponse[WakeResult]{
		Code: status,
		Body: result,
	}, nil
}

func wakeActionHTTPStatus(action WakeAction) (int, bool) {
	switch action {
	case WakeActionResumed, WakeActionAlreadyRunning:
		return http.StatusOK, true
	case WakeActionAutoResumeDisabled, WakeActionInvalidAutoResumePolicy:
		return http.StatusUnprocessableEntity, true
	case WakeActionPausing, WakeActionBadState:
		return http.StatusConflict, true
	case WakeActionGone:
		return http.StatusGone, true
	case WakeActionNotFound:
		return http.StatusNotFound, true
	default:
		return 0, false
	}
}
