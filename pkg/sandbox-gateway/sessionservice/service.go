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

// Package sessionservice authenticates SessionService traffic and invokes the local shared
// Session allocator without retaining request credentials.
package sessionservice

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/identity/oidc"
	"github.com/openkruise/agents/pkg/sandbox-gateway/jwtauth"
	"github.com/openkruise/agents/pkg/sandboxroute"
	session "github.com/openkruise/agents/pkg/sessionservice"
	"github.com/openkruise/agents/pkg/sessionservice/allocator"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
)

const SessionHeader = "x-kruise-agent-session-id"

var shortSessionServiceID = regexp.MustCompile(`^[a-z2-7]{13}$`)

// ParseAuthority identifies SessionService traffic before legacy Sandbox extraction.
// A matching but malformed SessionService authority must fail closed.
func ParseAuthority(authority string) (id string, port int, matched bool, err error) {
	// Classify the boundary before validating so malformed SessionService requests never
	// fall through to ordinary Sandbox authentication.
	if !strings.Contains(strings.ToLower(authority), ".svc.") {
		return "", 0, false, nil
	}
	invalid := func() (string, int, bool, error) { return "", 0, true, fmt.Errorf("invalid SessionService authority") }
	host := authority
	if strings.Contains(host, ":") {
		parsed, transportPort, splitErr := net.SplitHostPort(host)
		p, portErr := strconv.Atoi(transportPort)
		if splitErr != nil || portErr != nil || p < 1 || p > 65535 {
			return invalid()
		}
		host = parsed
	}
	boundary := strings.Index(strings.ToLower(host), ".svc.")
	if boundary < 0 {
		return invalid()
	}
	label, domain := host[:boundary], host[boundary+5:]
	portText, id, ok := strings.Cut(label, "-")
	if !ok || !shortSessionServiceID.MatchString(id) || len(utilvalidation.IsDNS1123Subdomain(strings.ToLower(strings.TrimSuffix(domain, ".")))) > 0 {
		return invalid()
	}
	for _, ch := range portText {
		if ch < '0' || ch > '9' {
			return invalid()
		}
	}
	port, err = strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return invalid()
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
	newSessionID    func() (string, error)
}

// NewService shares the process verifier without retaining request credentials.
func NewService(current func() oidc.Verifier) *Service {
	return &Service{currentVerifier: current, newSessionID: randomSessionID}
}

func randomSessionID() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(entropy[:]), nil
}

// Configure installs the Store once at the Gateway composition boundary.
func (s *Service) Configure(store session.Store) error {
	if store == nil {
		return errors.New("SessionService Store is nil")
	}
	if !s.resources.CompareAndSwap(nil, &servingResources{servingStore: store, allocator: allocator.New(store)}) {
		return errors.New("SessionService Service is already configured")
	}
	return nil
}

// Stop prevents new admissions from acquiring the configured resources.
func (s *Service) Stop() { s.resources.Store(nil) }

// Result contains a route and the Session identity confirmed by the allocator.
type Result struct {
	sandboxroute.Route
	SessionID string
}

// ResolveRoute authorizes SessionService traffic before allocating a Session. A policy
// change before any binding write restarts admission within the same budget.
func (s *Service) ResolveRoute(ctx context.Context, sessionServiceID, sessionID string, port int, rawJWT string) (Result, error) {
	if rawJWT == "" {
		return Result{}, &Error{401, "Unauthorized"}
	}
	if sessionID != "" && !sessionbinding.ValidSessionID(sessionID) || port < 1 || port > 65535 {
		return Result{}, &Error{400, "InvalidSessionID"}
	}
	resources := s.resources.Load()
	if resources == nil || s.currentVerifier == nil {
		return Result{}, unavailable()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	creating := sessionID == ""
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, unavailable()
		}
		parent, err := s.authorizeSessionService(ctx, resources.servingStore, sessionServiceID, port, rawJWT)
		if err != nil {
			return Result{}, err
		}
		if creating && sessionID == "" {
			var generationErr error
			sessionID, generationErr = s.newSessionID()
			if generationErr != nil {
				return Result{}, unavailable()
			}
		}
		route, err := allocateSession(ctx, resources.allocator, parent, sessionID, creating)
		if errors.Is(err, session.ErrPolicyChanged) {
			timer := time.NewTimer(20 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return Result{}, unavailable()
			case <-timer.C:
			}
			continue
		}
		if err != nil {
			return Result{}, err
		}
		return Result{Route: route, SessionID: sessionID}, nil
	}
}

// authorizeSessionService resolves trusted identity and checks JWT and port permission.
// It never reads member bindings or calls the Session allocator.
func (s *Service) authorizeSessionService(ctx context.Context, store session.Store, sessionServiceID string, port int, rawJWT string) (session.SessionService, error) {
	parent, err := store.GetSessionService(ctx, "", sessionServiceID)
	if err != nil {
		return session.SessionService{}, mapAllocationError(err)
	}
	if parent.Deleting {
		return session.SessionService{}, unavailable()
	}
	if err := jwtauth.VerifySessionServiceToken(s.currentVerifier(), rawJWT, parent.ID, parent.UID); err != nil {
		if errors.Is(err, jwtauth.ErrVerifierUnavailable) {
			return session.SessionService{}, unavailable()
		}
		if errors.Is(err, jwtauth.ErrSessionServiceIncarnation) {
			return session.SessionService{}, &Error{404, "SessionServiceNotFound"}
		}
		return session.SessionService{}, &Error{401, "Unauthorized"}
	}
	for _, p := range parent.Ports {
		if int(p) == port {
			return parent, nil
		}
	}
	return session.SessionService{}, &Error{403, "PortNotAllowed"}
}

// allocateSession receives only authorized SessionService state, never credentials.
func allocateSession(ctx context.Context, sessionAllocator *allocator.Allocator, parent session.SessionService, sessionID string, creating bool) (sandboxroute.Route, error) {
	var route sandboxroute.Route
	var err error
	if creating {
		route, err = sessionAllocator.Create(ctx, parent, sessionID)
	} else {
		route, err = sessionAllocator.Resolve(ctx, parent, sessionID)
	}
	// Only a policy change before mutation may restart admission. After a write,
	// the allocator wraps the same cause as unavailable to retain target pinning.
	if errors.Is(err, session.ErrPolicyChanged) && !errors.Is(err, session.ErrSessionServiceUnavailable) {
		return sandboxroute.Route{}, session.ErrPolicyChanged
	}
	if err != nil {
		klog.FromContext(ctx).Error(err, "SessionService allocation failed", "sessionServiceID", parent.ID, "sessionID", sessionID)
		return sandboxroute.Route{}, mapAllocationError(err)
	}
	if ctx.Err() != nil || net.ParseIP(route.IP) == nil {
		return sandboxroute.Route{}, unavailable()
	}
	return route, nil
}

func mapAllocationError(err error) *Error {
	if errors.Is(err, session.ErrSessionServiceNotFound) {
		return &Error{404, "SessionServiceNotFound"}
	}
	if errors.Is(err, session.ErrSessionNotFound) {
		return &Error{404, "SessionNotFound"}
	}
	if errors.Is(err, session.ErrInvalidSession) {
		return &Error{400, "InvalidSessionID"}
	}
	var domain *session.Error
	if errors.As(err, &domain) && (domain.Reason == "SessionCapacityExhausted" || domain.Reason == "BoundSandboxUnavailable") {
		return &Error{503, domain.Reason}
	}
	return unavailable()
}
