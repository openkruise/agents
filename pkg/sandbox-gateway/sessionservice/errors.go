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

package sessionservice

import (
	"errors"
	"net/http"
)

// Error is a safe Gateway HTTP error. Status is the HTTP status; Reason is the
// Envoy local-reply detail and normally the response body. Internal causes are
// logged separately and must not be exposed in the response.
// Treat the shared errors below as immutable.
type Error struct {
	Status int
	Reason string
}

func (e *Error) Error() string { return e.Reason }

// Internal SessionAllocationUnavailable and SessionServiceDeleting reasons map
// to ErrSessionManagerUnavailable. There is no separate timeout HTTP code:
// the Service/Allocator use a 10-second budget, shortened by the caller context.
// The HTTP filter creates its own 10-second context; it does not parse a client
// deadline header, and stream destruction cancels work without another reply.
// Timeout/cancellation ends waiting, not persisted bindings or Controller moves.
// ErrPolicyChanged may restart admission only outside a completed allocation
// failure and while context is live; a failure after a write must retain its target.

var (
	// ErrInvalidRequest rejects a malformed SessionService hostname, UID, or port,
	// including a direct Service call with a port outside 1..65535.
	// The filter uses the generic body "invalid SessionService request".
	ErrInvalidRequest = &Error{http.StatusBadRequest, "InvalidRequest"}

	// ErrInvalidSessionID rejects repeated Session headers or an invalid nonempty
	// Session ID. Invalid cookies instead act as absent. The filter uses the same
	// generic body as ErrInvalidRequest.
	ErrInvalidSessionID = &Error{http.StatusBadRequest, "InvalidSessionID"}

	// ErrPortNotAllowed means the requested port is absent from spec.ports.
	ErrPortNotAllowed = &Error{http.StatusForbidden, "PortNotAllowed"}

	// ErrSessionServiceNotFound means the requested UID is not visible, or the
	// admitted parent disappeared or was replaced. Check resource/cache scope.
	ErrSessionServiceNotFound = &Error{http.StatusNotFound, "SessionServiceNotFound"}

	// ErrSessionNotFound means a supplied ID has no visible binding or has expired.
	// Resolve does not create one; the caller must explicitly request a new Session.
	ErrSessionNotFound = &Error{http.StatusNotFound, "SessionNotFound"}

	// ErrSessionCapacityExhausted means no available member can accept a new
	// binding within the session limit and annotation budgets. Retry with backoff.
	ErrSessionCapacityExhausted = &Error{http.StatusServiceUnavailable, "SessionCapacityExhausted"}

	// ErrBoundSandboxUnavailable means a bound member is unavailable and this
	// request cannot migrate it: Strict affinity, pause/resume, or an unavailable
	// newly allocated target. Wait for recovery; Strict does not rebind.
	ErrBoundSandboxUnavailable = &Error{http.StatusServiceUnavailable, "BoundSandboxUnavailable"}

	// ErrSessionBindingConflict means multiple Active bindings exist without a
	// Moving binding. Retry with the same ID while Controller repair converges.
	ErrSessionBindingConflict = &Error{http.StatusServiceUnavailable, "SessionBindingConflict"}

	// ErrSessionRebinding means an observed/requested move was not confirmed before
	// waiting ended. Retry the same ID; Controller migration outlives this request.
	ErrSessionRebinding = &Error{http.StatusServiceUnavailable, "SessionRebinding"}

	// ErrSessionManagerUnavailable is the fallback for registry/cache/service
	// readiness, parent deletion, backend errors, unconfirmed creation, allocation
	// timeout/cancellation, policy change after mutation, invalid returned IP,
	// runtime-route lookup failure, ID generation failure, and internal panics.
	// Allocation is local; the historical name does not imply a Manager RPC.
	// Retry with backoff, but do not assume an unknown binding write failed.
	ErrSessionManagerUnavailable = &Error{http.StatusServiceUnavailable, "SessionManagerUnavailable"}

	errInvalidAuthority  = errors.New("invalid SessionService authority")
	errNilStore          = errors.New("SessionService Store is nil")
	errAlreadyConfigured = errors.New("SessionService Service is already configured")
)
