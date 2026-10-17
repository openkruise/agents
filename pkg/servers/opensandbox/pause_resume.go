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

package opensandbox

import (
	"net/http"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/web"
)

// PauseSandbox handles `POST /v1/sandboxes/{sandboxId}/pause`. The spec models
// pause as asynchronous: persist the intent, return 202, then poll describe
// until suspension completes.
//
// Timeout and annotations are omitted: the OpenSandbox surface carries no
// paused-retention or auto-pause knobs, so the sandbox keeps its existing
// timeout. State guards live in the infra layer (IsSandboxPausable → 409, and an
// already-paused sandbox is a no-op), deliberately not duplicated here: a
// handler-level guard on a stale read would falsely 409 the second of two
// concurrent pauses, the bug fixed for the E2B path in PR #422.
func (s *Server) PauseSandbox(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	return s.submitPauseResume(r, true)
}

// ResumeSandbox handles `POST /v1/sandboxes/{sandboxId}/resume`. Like pause, the
// spec models resume as asynchronous: persist the intent, return 202, then poll
// describe until the sandbox is running and ready.
//
// Timeout is omitted so the sandbox keeps its existing timeout (the
// OpenSandbox surface has no auto-pause re-arm knob). State guards live in the
// infra layer (IsSandboxResumable → 409, repeated resume intent is a no-op) and are not
// duplicated here, for the same concurrency reason as PauseSandbox.
func (s *Server) ResumeSandbox(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	return s.submitPauseResume(r, false)
}

// submitPauseResume shares only OpenSandbox transport handling; native E2B
// handlers and the separate Manager submission entrypoints remain independent.
func (s *Server) submitPauseResume(r *http.Request, paused bool) (web.ApiResponse[struct{}], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)
	sandboxID := r.PathValue(pathValueSandboxID)
	sbx, apiErr := sandboxFromContextOrError(ctx)
	if apiErr != nil {
		return web.ApiResponse[struct{}]{}, apiErr
	}
	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{Code: http.StatusInternalServerError, Message: errMessageUserNotInContext}
	}
	operation := "resume"
	var err error
	if paused {
		operation = "pause"
		err = s.manager.SubmitSandboxPause(ctx, user.ID.String(), sbx, infra.PauseOptions{})
	} else {
		err = s.manager.SubmitSandboxResume(ctx, user.ID.String(), sbx, infra.ResumeOptions{})
	}
	if err != nil {
		log.Error(err, "failed to submit sandbox lifecycle intent", "sandboxID", sandboxID, "operation", operation)
		return web.ApiResponse[struct{}]{}, mapLifecycleErrorToApiError(err)
	}
	log.Info("opensandbox sandbox lifecycle intent accepted", "sandboxID", sandboxID, "operation", operation)
	return web.ApiResponse[struct{}]{Code: http.StatusAccepted}, nil
}
