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

// Package sessionservice routes SessionService traffic through the local Session allocator.
package sessionservice

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"

	"github.com/google/uuid"

	"github.com/openkruise/agents/pkg/sandboxroute"
	session "github.com/openkruise/agents/pkg/sessionservice"
	"github.com/openkruise/agents/pkg/sessionservice/allocator"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
)

const SessionHeader = "x-kruise-agent-session-id"

// ParseAuthority identifies <port>-<SessionService UID>.svc.<domain> authorities.
// The second DNS label "svc" is reserved for SessionService routing, even when
// the remaining authority is malformed. Other positions retain legacy E2B handling.
// TODO: Pass the configured domain to the Gateway Deployment through a flag or
// environment variable and classify authorities by their full suffix. The current
// prefix-based classification rejects malformed SessionService authorities and
// leaves unrelated DNS label positions to E2B; explicit Sandbox headers also keep
// E2B handling in the filter. It cannot distinguish a legacy E2B authority that
// matches the same SessionService hostname shape. A configured domain would let
// routing distinguish <port>-<uid>.svc.<domain> from E2B hosts under <domain>.
func ParseAuthority(authority string) (uid string, port int, matched bool, err error) {
	host := strings.ToLower(authority)
	// Classify before transport-port validation so malformed SessionService ports
	// fail locally without claiming unrelated E2B authorities.
	hostname, _, _ := strings.Cut(host, ":")
	labels := strings.Split(strings.TrimSuffix(hostname, "."), ".")
	if len(labels) < 2 || labels[1] != "svc" {
		return "", 0, false, nil
	}
	invalid := func() (string, int, bool, error) {
		return "", 0, true, fmt.Errorf("invalid SessionService authority")
	}
	if strings.Contains(host, ":") {
		parsed, transportPort, splitErr := net.SplitHostPort(host)
		p, portErr := strconv.Atoi(transportPort)
		if splitErr != nil || portErr != nil || p < 1 || p > 65535 {
			return invalid()
		}
		host = parsed
	}
	host = strings.TrimSuffix(host, ".")
	labels = strings.Split(host, ".")
	if len(host) > 253 || len(labels) < 3 || labels[1] != "svc" {
		return invalid()
	}
	portText, uid, ok := strings.Cut(labels[0], "-")
	parsedUID, uidErr := uuid.Parse(uid)
	if !ok || len(labels[0]) > 63 || len(uid) != 36 || uidErr != nil || parsedUID.String() != uid {
		return invalid()
	}
	for _, label := range labels[2:] {
		if len(utilvalidation.IsDNS1123Label(label)) > 0 {
			return invalid()
		}
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
	return uid, port, true, nil
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
	resources    atomic.Pointer[servingResources]
	newSessionID func() (string, error)
}

// NewService creates the local SessionService routing service.
func NewService() *Service {
	return &Service{newSessionID: randomSessionID}
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

// ResolveRoute resolves SessionService traffic before allocating a Session. A policy
// change before any binding write restarts admission within the same budget.
func (s *Service) ResolveRoute(ctx context.Context, uid, sessionID string, port int) (Result, error) {
	if sessionID != "" && !sessionbinding.ValidSessionID(sessionID) || port < 1 || port > 65535 {
		return Result{}, &Error{400, "InvalidSessionID"}
	}
	resources := s.resources.Load()
	if resources == nil {
		return Result{}, unavailable()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	creating := sessionID == ""
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, unavailable()
		}
		parent, err := s.resolveSessionService(ctx, resources.servingStore, uid, port)
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

// resolveSessionService checks the parent and its exposed ports before allocation.
func (s *Service) resolveSessionService(ctx context.Context, store session.Store, uid string, port int) (session.SessionService, error) {
	parent, err := store.GetSessionServiceByUID(ctx, uid)
	if err != nil {
		return session.SessionService{}, mapAllocationError(err)
	}
	if parent.Deleting {
		return session.SessionService{}, unavailable()
	}
	for _, p := range parent.Ports {
		if int(p) == port {
			return parent, nil
		}
	}
	return session.SessionService{}, &Error{403, "PortNotAllowed"}
}

// allocateSession receives the current SessionService policy.
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
		klog.FromContext(ctx).Error(err, "SessionService allocation failed", "namespace", parent.Namespace, "sessionService", parent.Name, "sessionID", sessionID)
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
	if errors.As(err, &domain) && (domain.Reason == "SessionCapacityExhausted" || domain.Reason == "BoundSandboxUnavailable" || domain.Reason == "SessionBindingConflict" || domain.Reason == "SessionRebinding") {
		return &Error{503, domain.Reason}
	}
	return unavailable()
}
