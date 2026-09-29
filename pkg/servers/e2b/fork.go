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
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/api/v1alpha1"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils"
)

const (
	defaultForkTimeoutSeconds = 15
	maxForkCount              = 100
)

// ForkSandbox checkpoints a running sandbox once and creates one or more
// independent sandboxes from that checkpoint.
func (sc *Controller) ForkSandbox(r *http.Request) (web.ApiResponse[[]models.ForkSandboxResult], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)
	request, apiErr := sc.parseForkSandboxRequest(r)
	if apiErr != nil {
		return web.ApiResponse[[]models.ForkSandboxResult]{}, apiErr
	}

	domain, apiErr := sc.resolveSandboxDomain(r)
	if apiErr != nil {
		return web.ApiResponse[[]models.ForkSandboxResult]{}, apiErr
	}

	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[[]models.ForkSandboxResult]{}, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "User is empty",
		}
	}

	sandboxID := r.PathValue("sandboxID")
	source, apiErr := sc.getSandboxOfUser(ctx, sandboxID, claimedSandboxStates)
	if apiErr != nil {
		return web.ApiResponse[[]models.ForkSandboxResult]{}, apiErr
	}
	state, reason := source.GetState()
	if state == v1alpha1.SandboxStateDead && reason != "RunningResourceClaimedButNotReady" {
		return web.ApiResponse[[]models.ForkSandboxResult]{}, withSandboxResourceContext(&web.ApiError{
			Code:    http.StatusNotFound,
			Message: fmt.Sprintf("Sandbox %s not found", source.GetSandboxID()),
		}, source)
	}
	if state != v1alpha1.SandboxStateRunning {
		return web.ApiResponse[[]models.ForkSandboxResult]{}, withSandboxResourceContext(&web.ApiError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("Sandbox %s cannot be forked while in %s state: %s", source.GetSandboxID(), state, reason),
		}, source)
	}

	autoPause, _ := ParseTimeout(source)
	pausedRetention := time.Duration(0)
	pausedRetentionAnnotation := ""
	if autoPause {
		var backfill *string
		pausedRetention, backfill = resolvePersistedPauseRetention(ctx, source.GetSandboxID(), source.GetAnnotations())
		pausedRetentionAnnotation = source.GetAnnotations()[v1alpha1.AnnotationReservePausedSandboxDuration]
		if backfill != nil {
			pausedRetentionAnnotation = *backfill
		}
	}
	quotaSpec := user.QuotaSpec
	if quotaSpec != nil {
		quotaSpec = quotaSpec.DeepCopy()
	}

	forks, err := sc.manager.ForkSandbox(ctx, sandboxmanager.ForkSandboxOptions{
		SourceID:                  source.GetSandboxID(),
		Namespace:                 source.GetNamespace(),
		User:                      user.ID.String(),
		Count:                     request.count,
		Quota:                     quotaSpec,
		TimeoutSeconds:            request.timeout,
		AutoPause:                 autoPause,
		PausedRetention:           pausedRetention,
		PausedRetentionAnnotation: pausedRetentionAnnotation,
	})
	if err != nil {
		log.Error(err, "failed to fork sandbox", "sandboxID", sandboxID)
		return web.ApiResponse[[]models.ForkSandboxResult]{}, withSandboxResourceContext(&web.ApiError{
			Code:    forkRequestErrorCode(err),
			Message: err.Error(),
		}, source)
	}

	results := make([]models.ForkSandboxResult, len(forks))
	for index, fork := range forks {
		if fork.Err != nil {
			results[index].Error = &models.Error{
				Code:    int32(forkItemErrorCode(fork.Err)),
				Message: fork.Err.Error(),
			}
			continue
		}
		results[index].Sandbox = sc.convertToE2BSandbox(fork.Sandbox, utils.GetAccessToken(fork.Sandbox), domain)
	}

	return web.ApiResponse[[]models.ForkSandboxResult]{
		Code: http.StatusCreated,
		Body: results,
	}, nil
}

type forkSandboxRequest struct {
	timeout int
	count   int
}

func (sc *Controller) parseForkSandboxRequest(r *http.Request) (forkSandboxRequest, *web.ApiError) {
	var request models.ForkSandboxRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil && !errors.Is(err, io.EOF) {
		return forkSandboxRequest{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("failed to decode fork request: %v", err),
		}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return forkSandboxRequest{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "fork request must contain a single JSON object",
		}
	}

	parsed := forkSandboxRequest{timeout: defaultForkTimeoutSeconds, count: 1}
	if request.Timeout != nil {
		parsed.timeout = *request.Timeout
	}
	if request.Count != nil {
		parsed.count = *request.Count
	}
	if parsed.timeout < 1 || parsed.timeout > sc.maxTimeout {
		return forkSandboxRequest{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("timeout should be between 1 and %d", sc.maxTimeout),
		}
	}
	if parsed.count < 1 || parsed.count > maxForkCount {
		return forkSandboxRequest{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("count should be between 1 and %d", maxForkCount),
		}
	}
	return parsed, nil
}

func forkRequestErrorCode(err error) int {
	switch managererrors.GetErrCode(err) {
	case managererrors.ErrorBadRequest:
		return http.StatusBadRequest
	case managererrors.ErrorNotFound, managererrors.ErrorNotAllowed:
		return http.StatusNotFound
	case managererrors.ErrorConflict:
		return http.StatusConflict
	case managererrors.ErrorUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func forkItemErrorCode(err error) int {
	switch managererrors.GetErrCode(err) {
	case managererrors.ErrorQuotaExceeded:
		return http.StatusTooManyRequests
	case managererrors.ErrorConflict:
		return http.StatusConflict
	case managererrors.ErrorUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
