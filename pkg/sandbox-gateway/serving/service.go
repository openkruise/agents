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

// Package serving authenticates Serving traffic and invokes the local shared
// Session allocator without retaining request credentials.
package serving

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/identity/oidc"
	"github.com/openkruise/agents/pkg/sandbox-gateway/jwtauth"
	"github.com/openkruise/agents/pkg/sandboxroute"
	session "github.com/openkruise/agents/pkg/servingsession"
	"github.com/openkruise/agents/pkg/servingsession/allocator"
	sessionbinding "github.com/openkruise/agents/pkg/servingsession/binding"
)

const SessionHeader = "x-kruise-agent-session-id"

// ParseAuthority identifies Serving traffic before legacy Sandbox extraction.
// A matching but malformed Serving authority must fail closed.
func ParseAuthority(authority string) (id string, port int, matched bool, err error) {
	host := authority
	if strings.Contains(host, ":") {
		if parsed, _, splitErr := net.SplitHostPort(host); splitErr == nil {
			host = parsed
		}
	}
	label, domain, ok := strings.Cut(host, ".svc.")
	if !ok {
		return "", 0, false, nil
	}
	portText, id, ok := strings.Cut(label, "-")
	if !ok || !strings.HasPrefix(id, "serving-") {
		return "", 0, false, nil
	}
	port, err = strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 || domain == "" || id == "serving-" || !sessionbinding.ValidSessionID(id) {
		return "", 0, true, fmt.Errorf("invalid Serving authority")
	}
	return id, port, true, nil
}

// Error contains only locally selected, safe status and reason values. Tokens,
// request bodies and internal storage errors are never echoed.
type Error struct {
	Status int
	Reason string
}

func (e *Error) Error() string { return e.Reason }

func unavailable() *Error { return &Error{Status: 503, Reason: "SessionManagerUnavailable"} }

type servingResources struct {
	servingStore session.Store
	allocator    *allocator.Allocator
}

// Service is assembled once per Gateway. It owns no network transport to Manager.
type Service struct {
	currentVerifier func() oidc.Verifier
	resources       atomic.Pointer[servingResources]
}

// NewService shares the process verifier without retaining request credentials.
func NewService(current func() oidc.Verifier) *Service { return &Service{currentVerifier: current} }

// Configure installs the Store once at the Gateway composition boundary.
func (s *Service) Configure(store session.Store) error {
	if store == nil {
		return errors.New("Serving Store is nil")
	}
	if !s.resources.CompareAndSwap(nil, &servingResources{servingStore: store, allocator: allocator.New(store)}) {
		return errors.New("Serving Service is already configured")
	}
	return nil
}

// Stop prevents new admissions from acquiring the configured resources.
func (s *Service) Stop() { s.resources.Store(nil) }

// ResolveRoute authorizes Serving traffic before allocating a Session. A policy
// change before any binding write restarts admission within the same budget.
func (s *Service) ResolveRoute(ctx context.Context, servingID, sessionID string, port int, rawJWT string) (sandboxroute.Route, error) {
	if rawJWT == "" {
		return sandboxroute.Route{}, &Error{401, "Unauthorized"}
	}
	if !sessionbinding.ValidSessionID(sessionID) || port < 1 || port > 65535 {
		return sandboxroute.Route{}, &Error{400, "InvalidRequest"}
	}
	resources := s.resources.Load()
	if resources == nil || s.currentVerifier == nil {
		return sandboxroute.Route{}, unavailable()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return sandboxroute.Route{}, unavailable()
		}
		parent, err := s.authorizeServing(ctx, resources.servingStore, servingID, port, rawJWT)
		if err != nil {
			return sandboxroute.Route{}, err
		}
		route, err := allocateSession(ctx, resources.allocator, parent, sessionID)
		if errors.Is(err, session.ErrPolicyChanged) {
			timer := time.NewTimer(20 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return sandboxroute.Route{}, unavailable()
			case <-timer.C:
			}
			continue
		}
		return route, err
	}
}

// authorizeServing resolves trusted identity and checks JWT and port permission.
// It never reads member bindings or calls the Session allocator.
func (s *Service) authorizeServing(ctx context.Context, store session.Store, servingID string, port int, rawJWT string) (session.Serving, error) {
	parent, err := store.GetServing(ctx, "", servingID)
	if err != nil {
		return session.Serving{}, mapAllocationError(err)
	}
	if parent.Deleting {
		return session.Serving{}, unavailable()
	}
	if err := jwtauth.VerifyServingToken(s.currentVerifier(), rawJWT, parent.ID, parent.UID); err != nil {
		if errors.Is(err, jwtauth.ErrVerifierUnavailable) {
			return session.Serving{}, unavailable()
		}
		if errors.Is(err, jwtauth.ErrServingIncarnation) {
			return session.Serving{}, &Error{404, "ServingNotFound"}
		}
		return session.Serving{}, &Error{401, "Unauthorized"}
	}
	for _, p := range parent.Ports {
		if int(p) == port {
			return parent, nil
		}
	}
	return session.Serving{}, &Error{403, "PortNotAllowed"}
}

// allocateSession receives only authorized Serving state, never credentials.
func allocateSession(ctx context.Context, sessionAllocator *allocator.Allocator, parent session.Serving, sessionID string) (sandboxroute.Route, error) {
	route, err := sessionAllocator.Allocate(ctx, parent, sessionID)
	// Only a policy change before mutation may restart admission. After a write,
	// the allocator wraps the same cause as unavailable to retain target pinning.
	if errors.Is(err, session.ErrPolicyChanged) && !errors.Is(err, session.ErrServingUnavailable) {
		return sandboxroute.Route{}, session.ErrPolicyChanged
	}
	if err != nil {
		klog.FromContext(ctx).Error(err, "Serving allocation failed", "servingID", parent.ID, "sessionID", sessionID)
		return sandboxroute.Route{}, mapAllocationError(err)
	}
	if ctx.Err() != nil || net.ParseIP(route.IP) == nil {
		return sandboxroute.Route{}, unavailable()
	}
	return route, nil
}

func mapAllocationError(err error) *Error {
	if errors.Is(err, session.ErrServingNotFound) {
		return &Error{404, "ServingNotFound"}
	}
	if errors.Is(err, session.ErrInvalidSession) {
		return &Error{400, "InvalidRequest"}
	}
	var domain *session.Error
	if errors.As(err, &domain) && (domain.Reason == "SessionCapacityExhausted" || domain.Reason == "BoundSandboxUnavailable") {
		return &Error{503, domain.Reason}
	}
	return unavailable()
}
