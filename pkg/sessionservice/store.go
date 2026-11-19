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

// Package sessionservice defines backend-neutral SessionService identities, observations
// and persistence contracts used by Gateway.
package sessionservice

import (
	"context"
	"errors"

	"github.com/openkruise/agents/pkg/sandboxroute"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
)

var (
	ErrSessionServiceNotFound      = errors.New("session service not found")
	ErrSessionServiceUnavailable   = errors.New("session service session backend unavailable")
	ErrSessionServiceConflict      = errors.New("session service observation conflicts with persisted state")
	ErrSessionServiceDataAmbiguous = errors.New("session service session data is ambiguous")
	ErrSessionServiceWriteUnknown  = errors.New("session service write outcome unknown")
	ErrSessionServiceCapacity      = errors.New("session service binding storage capacity exhausted")
	ErrPolicyChanged               = errors.New("SessionService policy changed after admission")
	ErrInvalidSession              = errors.New("invalid Session ID")
	ErrSessionNotFound             = errors.New("session not found")
)

// SessionService carries a backend-neutral identity. Version is opaque to callers.
type SessionService struct {
	Namespace, Name, UID, Version string
	Ports                         []int32
	Deleting                      bool
	Affinity                      string
	MaxSessions                   int

	ExpireAfterCreation string
}

// SessionServiceMember is one complete, versioned binding observation. Backend metadata
// and encoding remain private.
type SessionServiceMember struct {
	UID, Version, Phase string
	Route               sandboxroute.Route
	Ready, Deleting     bool
	Bindings            sessionbinding.Bindings
}

// SessionServiceSnapshot collects cached observations, not an atomic read of the parent
// and all members. Conditional writes must validate those observations again.
type SessionServiceSnapshot struct {
	SessionService SessionService
	Members        []SessionServiceMember
}

// Store is the storage port consumed by the allocation domain.
type Store interface {
	GetSessionServiceByUID(context.Context, string) (SessionService, error)
	GetSessionService(context.Context, string, string) (SessionService, error)
	ReadSessionServiceSnapshot(context.Context, SessionService) (SessionServiceSnapshot, error)
	// ReplaceSessionServiceBindings rechecks the visible snapshot before a conditional
	// write to one member. It is not a transaction over the parent/member set.
	// Callers reread after a conflict; the returned version identifies the persisted write.
	ReplaceSessionServiceBindings(context.Context, SessionServiceSnapshot, SessionServiceMember, sessionbinding.Bindings) (string, error)
}

// Error classifies domain failures without exposing a transport protocol.
type Error struct {
	Reason string
	Cause  error
}

func (e *Error) Error() string { return e.Reason }

func (e *Error) Unwrap() error { return e.Cause }

func (e *Error) Is(target error) bool { return target == ErrSessionServiceUnavailable }
