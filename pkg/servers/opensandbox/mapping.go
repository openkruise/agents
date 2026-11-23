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
	"errors"
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandboxstate"
	"github.com/openkruise/agents/pkg/servers/web"
)

// MapState projects the neutral lifecycle state for the create response.
// A newly claimed sandbox remains Pending until the backend reports it ready.
func MapState(state, _ string) SandboxState {
	switch state {
	case sandboxstate.Running:
		return SandboxStateRunning
	case sandboxstate.Paused:
		return SandboxStatePaused
	case sandboxstate.Expired, sandboxstate.Dead:
		return SandboxStateTerminated
	default:
		return SandboxStatePending
	}
}

// MapSandboxState refines the neutral state for list and describe. The phase
// distinguishes pause transitions and terminal outcomes. Deletion takes
// precedence over the last phase, as it does in the backend state observation.
func MapSandboxState(sbx infra.Sandbox) (SandboxState, error) {
	state, _ := sbx.GetState()
	return mapStatePhase(state, sbx.GetPhase(), sbx.GetDeletionTimestamp() != nil)
}

func mapStatePhase(state, phase string, deleting bool) (SandboxState, error) {
	switch state {
	case sandboxstate.Creating, sandboxstate.Available, sandboxstate.Provisioning:
		return SandboxStatePending, nil
	case sandboxstate.Running:
		return SandboxStateRunning, nil
	case sandboxstate.Paused:
		switch phase {
		case string(agentsv1alpha1.SandboxRunning):
			return SandboxStatePausing, nil
		case string(agentsv1alpha1.SandboxResuming):
			return SandboxStateResuming, nil
		case string(agentsv1alpha1.SandboxUpgrading):
			return SandboxStateRunning, nil
		default:
			return SandboxStatePaused, nil
		}
	case sandboxstate.Expired:
		return SandboxStateTerminated, nil
	case sandboxstate.Dead:
		if deleting {
			return SandboxStateTerminated, nil
		}
		switch phase {
		case string(agentsv1alpha1.SandboxTerminating):
			return SandboxStateStopping, nil
		case string(agentsv1alpha1.SandboxFailed):
			return SandboxStateFailed, nil
		case string(agentsv1alpha1.SandboxSucceeded):
			return SandboxStateTerminated, nil
		}
	}
	return "", fmt.Errorf("opensandbox: unmapped sandbox state %q (phase %q)", state, phase)
}

// mapLifecycleErrorToApiError converts a manager/infra error from a
// sandbox-scoped lifecycle operation (pause/resume/delete/renew) into an
// ApiError. It is deliberately separate from create.go's
// mapInfraErrorToApiError: the create path maps ErrorNotFound to 400 (a missing
// template is a bad request), whereas the lifecycle routes map it to 404 (a
// missing sandbox is not found), matching the OpenSandbox spec's per-route
// response codes.
//
// Mapping (specs/sandbox-lifecycle.yml lifecycle routes admit 400/401/403/
// 404/409/500):
//   - k8s NotFound, ErrorNotFound → 404
//   - ErrorBadRequest → 400
//   - ErrorConflict, wait-task conflict → 409
//   - ErrorQuotaExceeded → 403
//   - ErrorInternal, ErrorUnknown, untyped → 500
//
// 401 is produced by the CheckApiKey middleware, and sandbox-load failures are
// classified by getSandboxErrorCode (anti-enumeration), so neither appears here.
// 429 is not emitted: agents has no request-rate limiter (compatibility limit).
func mapLifecycleErrorToApiError(err error) *web.ApiError {
	if apierrors.IsNotFound(err) {
		return &web.ApiError{Code: http.StatusNotFound, Message: err.Error()}
	}
	if errors.Is(err, cacheutils.ErrWaitTaskConflict) {
		return &web.ApiError{Code: http.StatusConflict, Message: err.Error()}
	}
	switch managererrors.GetErrCode(err) {
	case managererrors.ErrorNotFound:
		return &web.ApiError{Code: http.StatusNotFound, Message: err.Error()}
	case managererrors.ErrorBadRequest:
		return &web.ApiError{Code: http.StatusBadRequest, Message: err.Error()}
	case managererrors.ErrorConflict:
		return &web.ApiError{Code: http.StatusConflict, Message: err.Error()}
	case managererrors.ErrorQuotaExceeded:
		return &web.ApiError{Code: http.StatusForbidden, Message: err.Error()}
	default:
		return &web.ApiError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
}
