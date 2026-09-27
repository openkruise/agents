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
// pause as asynchronous (202, then poll describe through Pausing → Paused);
// agents pauses synchronously, so by the time this returns the transition is
// already complete. This inherited implementation returns 202 but still blocks
// on the backend operation; asynchronous acceptance remains a compatibility gap.
//
// Pause options are intentionally empty: the OpenSandbox surface carries no
// paused-retention or auto-pause knobs, so the sandbox keeps its existing
// timeout. State guards live in the infra layer (IsSandboxPausable → 409, and an
// already-paused sandbox is a no-op), deliberately not duplicated here: a
// handler-level guard on a stale read would falsely 409 the second of two
// concurrent pauses, the bug fixed for the E2B path in PR #422.
func (s *Server) PauseSandbox(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)
	sandboxID := r.PathValue(pathValueSandboxID)

	sbx, apiErr := sandboxFromContextOrError(ctx)
	if apiErr != nil {
		return web.ApiResponse[struct{}]{}, apiErr
	}

	if err := s.manager.PauseSandbox(ctx, sbx, infra.PauseOptions{}); err != nil {
		log.Error(err, "failed to pause sandbox", "sandboxID", sandboxID)
		return web.ApiResponse[struct{}]{}, mapLifecycleErrorToApiError(err)
	}

	log.Info("opensandbox sandbox paused", "sandboxID", sandboxID)
	return web.ApiResponse[struct{}]{Code: http.StatusAccepted}, nil
}

// ResumeSandbox handles `POST /v1/sandboxes/{sandboxId}/resume`. Like pause, the
// spec models resume as asynchronous (202, then poll through Resuming → Running)
// while agents resumes synchronously; 202 is returned per spec.
//
// Resume options are empty so the sandbox keeps its existing timeout (the
// OpenSandbox surface has no auto-pause re-arm knob). State guards live in the
// infra layer (IsSandboxResumable → 409, already-ready is a no-op) and are not
// duplicated here, for the same concurrency reason as PauseSandbox.
func (s *Server) ResumeSandbox(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)
	sandboxID := r.PathValue(pathValueSandboxID)

	sbx, apiErr := sandboxFromContextOrError(ctx)
	if apiErr != nil {
		return web.ApiResponse[struct{}]{}, apiErr
	}

	if err := s.manager.ResumeSandbox(ctx, sbx, infra.ResumeOptions{}); err != nil {
		log.Error(err, "failed to resume sandbox", "sandboxID", sandboxID)
		return web.ApiResponse[struct{}]{}, mapLifecycleErrorToApiError(err)
	}

	log.Info("opensandbox sandbox resumed", "sandboxID", sandboxID)
	return web.ApiResponse[struct{}]{Code: http.StatusAccepted}, nil
}
