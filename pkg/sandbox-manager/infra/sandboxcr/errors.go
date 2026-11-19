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

package sandboxcr

import (
	"errors"

	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
	"github.com/openkruise/agents/pkg/lifecycle/claim"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
)

// classifyCreateError delegates classification to the neutral engine and
// translates the resulting *claim.Error into a manager error so the clone path
// keeps manager error codes for HTTP status mapping. claim.ErrorCode mirrors
// the managererrors code strings, so the conversion is direct. nil and
// context-interrupted errors pass through unchanged.
func classifyCreateError(err error, contextMsg string) error {
	classified := claim.ClassifyCreateError(err, contextMsg)
	var cErr *claim.Error
	if errors.As(classified, &cErr) {
		return managererrors.NewError(managererrors.ErrorCode(cErr.Code), "%s", cErr.Message)
	}
	return classified
}

// managerErrorFromClaim converts a neutral claim classification into a manager
// error, keeping the original chain as cause so wait types and context errors
// stay discoverable. Unclassified errors pass through unchanged.
func managerErrorFromClaim(err error) error {
	var cErr *claim.Error
	if !errors.As(err, &cErr) {
		return err
	}
	return managererrors.WrapError(managererrors.ErrorCode(cErr.Code), err, "%s", cErr.Message)
}

// sandboxWaitTimeoutError classifies a wait-task timeout with the sandbox action
// that did not complete. Classifying and wording it here keeps the HTTP layer on
// manager error codes alone instead of recognizing cache-level wait types. The
// original wait error stays in the cause chain, so callers can still tell an
// internal wait timeout from their own deadline or cancellation.
func sandboxWaitTimeoutError(err error, action, sandboxID string) error {
	var waitErr *cacheutils.WaitNotSatisfiedError
	if !errors.As(err, &waitErr) {
		return err
	}
	return managererrors.WrapError(managererrors.ErrorInternal, err,
		"timed out after %s waiting for sandbox %s to %s; completion was not confirmed, check the sandbox state before retrying",
		waitErr.Timeout, sandboxID, action)
}
