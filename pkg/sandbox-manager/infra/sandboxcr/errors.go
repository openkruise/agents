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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"

	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
)

type retriableError struct {
	Message string
}

func (e retriableError) Error() string {
	return e.Message
}

func (e retriableError) Is(target error) bool {
	as := retriableError{}
	if !errors.As(target, &as) {
		return false
	}
	return as.Message == e.Message
}

func NoAvailableError(template, reason string) error {
	return retriableError{Message: fmt.Sprintf("no available sandboxes for template %s (%s)", template, reason)}
}

// classifyCreateError classifies a Kubernetes API error from a Create/Update
// operation into one of three categories:
// - retryable: transient server errors, conflicts, network errors -> retriableError
// - terminal bad request: schema/validation errors -> managererrors.Error{ErrorBadRequest}
// - terminal internal: Forbidden/auth/platform misconfig -> managererrors.Error{ErrorInternal}
func classifyCreateError(err error, contextMsg string) error {
	if err == nil {
		return nil
	}

	// Context interrupted - return as-is so the retry loop stops (non-retriable).
	if wait.Interrupted(err) {
		return err
	}

	// Transient server errors - retryable
	if apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsInternalError(err) {
		return retriableError{Message: fmt.Sprintf("%s: %s", contextMsg, err)}
	}

	// Conflict / AlreadyExists - retryable (optimistic locking or create name collision)
	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		return retriableError{Message: fmt.Sprintf("%s: %s", contextMsg, err)}
	}

	// Invalid / BadRequest - terminal user error
	if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
		return managererrors.NewError(managererrors.ErrorBadRequest, "%s: %s", contextMsg, err)
	}

	// Forbidden - always a platform issue. The sandbox-manager service account
	// lacks the required RBAC permissions, or a cluster-level policy (quota,
	// admission webhook, PodSecurity, etc.) denied the operation. Either way
	// this is a server-side misconfiguration, not the HTTP caller's fault.
	if apierrors.IsForbidden(err) {
		return newPlatformCreateError(err, contextMsg)
	}

	// Unauthorized - platform issue
	if apierrors.IsUnauthorized(err) {
		return newPlatformCreateError(err, contextMsg)
	}

	// NotFound (CRD/namespace missing) - platform issue
	if apierrors.IsNotFound(err) {
		return newPlatformCreateError(err, contextMsg)
	}

	// MethodNotSupported - platform issue
	if apierrors.IsMethodNotSupported(err) {
		return newPlatformCreateError(err, contextMsg)
	}

	// Non-StatusError (network errors, etc.) - retryable (conservative)
	return retriableError{Message: fmt.Sprintf("%s: %s", contextMsg, err)}
}

func newPlatformCreateError(err error, contextMsg string) error {
	return managererrors.NewError(
		managererrors.ErrorInternal,
		"%s: sandbox creation failed due to platform configuration issue: %s",
		contextMsg,
		err,
	)
}
