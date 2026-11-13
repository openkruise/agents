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

package claim

import (
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
)

// ErrorCode is a neutral, transport-independent classification of a claim
// failure. The sandbox-manager adapter maps it onto the manager error codes that
// drive HTTP status; the SandboxClaim controller maps it onto status/events. It
// intentionally mirrors the subset of pkg/sandbox-manager/errors codes the claim
// engine produces, without importing that package.
type ErrorCode string

const (
	CodeBadRequest = ErrorCode("BadRequest")
	CodeInternal   = ErrorCode("Internal")
	CodeConflict   = ErrorCode("Conflict")
	CodeNotFound   = ErrorCode("NotFound")
)

// Error is a classified claim error. Unwrap preserves the underlying cause so
// errors.Is/errors.As keep working across the neutral boundary.
type Error struct {
	Code    ErrorCode
	Message string
	cause   error
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error {
	return e.cause
}

// NewError builds a classified error with a formatted message.
func NewError(code ErrorCode, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...)}
}

// WrapError builds a classified error that keeps cause available to Unwrap.
func WrapError(code ErrorCode, cause error, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...), cause: cause}
}

// GetErrCode returns the classification of err, or CodeUnknown-equivalent empty
// code when err is not a classified claim error.
func GetErrCode(err error) ErrorCode {
	var e *Error
	if !errors.As(err, &e) {
		return ""
	}
	return e.Code
}

// RetriableError marks an attempt the outer claim/clone retry loop may safely
// repeat. Is matches on message so errors.As(&RetriableError{}) and
// errors.Is against an equal-message value both classify it as retriable.
type RetriableError struct {
	Message string
}

func (e RetriableError) Error() string {
	return e.Message
}

func (e RetriableError) Is(target error) bool {
	as := RetriableError{}
	if !errors.As(target, &as) {
		return false
	}
	return as.Message == e.Message
}

// NoAvailableError reports that no sandbox could be secured for a template. It
// is retriable: the pool changes over time (rollout, scale-up).
func NoAvailableError(template, reason string) error {
	return RetriableError{Message: fmt.Sprintf("no available sandboxes for template %s (%s)", template, reason)}
}

// TerminalMutationError marks a user-supplied mutation callback as terminal at
// the outer claim/clone retry boundary. Unwrap keeps the original callback cause
// available to errors.Is/errors.As without allowing a retriable cause to repeat a
// callback or a completed claim/clone attempt.
type TerminalMutationError struct {
	Stage string
	Err   error
}

func (e TerminalMutationError) Error() string {
	return fmt.Sprintf("%s callback failed: %v", e.Stage, e.Err)
}

func (e TerminalMutationError) Unwrap() error {
	return e.Err
}

// TerminalValidationError marks an input-validation failure during sandbox
// mutation (e.g. memory downscale) as terminal at the outer claim/clone retry
// boundary. Unwrap keeps the underlying classified error available to errors.As.
type TerminalValidationError struct {
	Err error
}

func (e TerminalValidationError) Error() string {
	return fmt.Sprintf("sandbox claim validation failed: %v", e.Err)
}

func (e TerminalValidationError) Unwrap() error {
	return e.Err
}

// candidateResizeIncompatibleError marks a candidate that cannot serve the
// requested resize. pickAnAvailableSandbox records the skip reason so it can be
// surfaced in the no-available error; skips never terminate the claim.
type candidateResizeIncompatibleError struct {
	err error
}

func (e *candidateResizeIncompatibleError) Error() string {
	return e.err.Error()
}

func (e *candidateResizeIncompatibleError) Unwrap() error {
	return e.err
}

// ClassifyCreateError classifies a Kubernetes API error from a Create operation
// into one of three categories:
//   - ambiguous: transient server errors, conflicts, network errors -> CodeInternal
//   - terminal bad request: schema/validation errors -> CodeBadRequest
//   - terminal internal: Forbidden/auth/platform misconfig -> CodeInternal
//
// Context-interrupted errors are returned as-is so the retry loop stops.
func ClassifyCreateError(err error, contextMsg string) error {
	if err == nil {
		return nil
	}

	// Context interrupted - return as-is so the retry loop stops (non-retriable).
	if wait.Interrupted(err) {
		return err
	}

	// Transient server errors are ambiguous for create: the apiserver may have
	// persisted the CR before the client observed failure, so do not retry with
	// a new object/lockstring.
	if apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsInternalError(err) {
		return NewError(CodeInternal, "%s: %s", contextMsg, err)
	}

	// Conflict is also terminal for Create. There is no existing sandbox object
	// safely locked by this attempt that the retry loop can reuse.
	if apierrors.IsConflict(err) {
		return NewError(CodeInternal, "%s: %s", contextMsg, err)
	}

	// AlreadyExists - terminal conflict (create name collision)
	if apierrors.IsAlreadyExists(err) {
		return NewError(CodeConflict, "%s: %s", contextMsg, err)
	}

	// Invalid / BadRequest - terminal user error
	if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
		return NewError(CodeBadRequest, "%s: %s", contextMsg, err)
	}

	// Forbidden - always a platform issue. The service account lacks the required
	// RBAC permissions, or a cluster-level policy (quota, admission webhook,
	// PodSecurity, etc.) denied the operation: a server-side misconfiguration, not
	// the caller's fault.
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

	// Non-StatusError (network errors, etc.) - ambiguous create outcome.
	return NewError(CodeInternal, "%s: %s", contextMsg, err)
}

func newPlatformCreateError(err error, contextMsg string) error {
	return NewError(
		CodeInternal,
		"%s: sandbox creation failed due to platform configuration issue: %s",
		contextMsg,
		err,
	)
}
