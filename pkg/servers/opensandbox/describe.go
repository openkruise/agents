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
	"fmt"
	"net/http"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/servers/web"
)

// DescribeSandbox handles `GET /v1/sandboxes/{sandboxId}`. The loadOwnedSandbox
// middleware has already resolved and ownership-checked the sandbox, so this
// handler only applies the viewability rule and projects the response.
//
// A sandbox that is not viewable (creating, or dead with a terminal reason) is
// reported as 404 rather than its transitional state: agents physically removes
// terminated sandboxes, so "gone" is the honest answer and it keeps the
// anti-enumeration posture uniform with a missing sandbox.
func (s *Server) DescribeSandbox(r *http.Request) (web.ApiResponse[SandboxResponse], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)
	sandboxID := r.PathValue(pathValueSandboxID)

	sbx, apiErr := sandboxFromContextOrError(ctx)
	if apiErr != nil {
		return web.ApiResponse[SandboxResponse]{}, apiErr
	}
	if !isSandboxViewable(sbx) {
		log.Info("sandbox is not viewable, treating as not found", "sandboxID", sandboxID)
		return web.ApiResponse[SandboxResponse]{}, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: fmt.Sprintf("Cannot get sandbox %s: sandbox not found", sandboxID),
		}
	}

	body, err := projectSandbox(sbx)
	if err != nil {
		log.Error(err, "failed to project sandbox", "sandboxID", sandboxID)
		return web.ApiResponse[SandboxResponse]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: err.Error(),
		}
	}
	return web.ApiResponse[SandboxResponse]{Body: body}, nil
}
