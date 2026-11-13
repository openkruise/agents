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

// Package servingsession defines backend-neutral Serving identities, observations
// and persistence contracts shared by Manager and Gateway.
package servingsession

import (
	"context"
	"errors"

	"github.com/openkruise/agents/pkg/sandboxroute"
	sessionbinding "github.com/openkruise/agents/pkg/servingsession/binding"
)

var (
	ErrServingNotFound      = errors.New("serving not found")
	ErrServingUnavailable   = errors.New("serving session backend unavailable")
	ErrServingConflict      = errors.New("serving observation conflicts with persisted state")
	ErrServingDataAmbiguous = errors.New("serving session data is ambiguous")
	ErrServingWriteUnknown  = errors.New("serving write outcome unknown")
	ErrServingCapacity      = errors.New("serving binding storage capacity exhausted")
	ErrPolicyChanged        = errors.New("Serving policy changed after admission")
	ErrInvalidSession       = errors.New("invalid Session ID")
)

// Serving carries a backend-neutral identity. Version is opaque to callers.
type Serving struct {
	Namespace, Name, UID, ID, Version string
	Ports                             []int32
	Deleting                          bool
	Affinity                          string
	MaxSessions                       int
}

// ServingSession is a durable binding returned by management queries.
type ServingSession struct{ SessionID, SandboxID, State, LastRequestAt string }

// ServingSessionProvider is optional; legacy backends need not implement it.
type ServingSessionProvider interface {
	GetServingSessionBackend() ServingSessionBackend
}

// ServingSessionBackend isolates Serving persistence from Manager use-case logic.
// It covers Serving identity as well as Session inspection and release, keeping
// Kubernetes resources and annotation encoding inside the backend implementation.
// The current sandboxcr implementation stores bindings in Sandbox annotations.
//
// TODO: Before adding a database backend, coordinate binding storage and release
// guarantees across Manager, Gateway and controllers. Replacing the Manager's
// backend alone does not migrate the shared binding state.
type ServingSessionBackend interface {
	GetServing(context.Context, string, string) (Serving, error)
	ListServings(context.Context) ([]Serving, error)
	AssignServingID(context.Context, Serving, string) (Serving, error)
	ReadServingSessions(context.Context, Serving) ([]ServingSession, error)
	CloseServingSession(context.Context, Serving, string) error
}

// ServingMember is one complete, versioned binding observation. Backend metadata
// and encoding remain private.
type ServingMember struct {
	UID, Version, Phase string
	Route               sandboxroute.Route
	Ready, Deleting     bool
	Bindings            sessionbinding.Bindings
}

// ServingSnapshot collects cached observations, not an atomic read of the parent
// and all members. Conditional writes must validate those observations again.
type ServingSnapshot struct {
	Serving Serving
	Members []ServingMember
}

// Store is the storage port consumed by the allocation domain.
type Store interface {
	GetServing(context.Context, string, string) (Serving, error)
	ListServings(context.Context) ([]Serving, error)
	ReadServingSnapshot(context.Context, Serving) (ServingSnapshot, error)
	// ReplaceServingBindings rechecks the visible snapshot before a conditional
	// write to one member. It is not a transaction over the parent/member set.
	// Callers reread after a conflict; the returned version identifies the ACK.
	ReplaceServingBindings(context.Context, ServingSnapshot, ServingMember, sessionbinding.Bindings) (string, error)
}

// Error classifies domain failures without exposing a transport protocol.
type Error struct {
	Reason string
	Cause  error
}

func (e *Error) Error() string { return e.Reason }

func (e *Error) Unwrap() error { return e.Cause }

func (e *Error) Is(target error) bool { return target == ErrServingUnavailable }
