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

package allocator

import (
	"context"
	"errors"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/openkruise/agents/pkg/sandboxroute"
	session "github.com/openkruise/agents/pkg/sessionservice"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
)

// Allocator applies Session affinity and capacity to shared, versioned observations.
// Its locks coordinate only requests within this process.
type Allocator struct {
	store        session.Store
	mu           sync.Mutex
	sessionLocks map[string]*sessionLock
}

// New assembles an allocator over the shared persistence contract.
func New(store session.Store) *Allocator { return &Allocator{store: store} }

func backendError(err error) error {
	if errors.Is(err, session.ErrSessionServiceNotFound) {
		return err
	}
	return &session.Error{Reason: "SessionManagerUnavailable", Cause: err}
}

type sessionLock struct {
	token chan struct{}
	users int
}

// Allocation is serialized only for one local SessionService UID and Session identity.
// Waiting callers keep their own cancellation and two-second operation budget.
func (a *Allocator) lockSessionServiceSession(ctx context.Context, key string) (func(), error) {
	a.mu.Lock()
	if a.sessionLocks == nil {
		a.sessionLocks = map[string]*sessionLock{}
	}
	lock := a.sessionLocks[key]
	if lock == nil {
		lock = &sessionLock{token: make(chan struct{}, 1)}
		a.sessionLocks[key] = lock
	}
	lock.users++
	a.mu.Unlock()
	releaseUser := func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		lock.users--
		if lock.users == 0 {
			delete(a.sessionLocks, key)
		}
	}
	select {
	case lock.token <- struct{}{}:
		return func() { <-lock.token; releaseUser() }, nil
	case <-ctx.Done():
		releaseUser()
		return nil, ctx.Err()
	}
}

func allocationUnavailable(reason string, cause error) error {
	return &session.Error{Reason: reason, Cause: cause}
}

// Conflicts, stale cache observations and uncertain writes require a new
// observation. Each allocation phase decides whether a target must remain pinned.
func retryableWriteError(err error) bool {
	return errors.Is(err, session.ErrSessionServiceConflict) || errors.Is(err, session.ErrSessionServiceUnavailable) || errors.Is(err, session.ErrSessionServiceWriteUnknown)
}

func allocationBackoff(ctx context.Context) error {
	timer := time.NewTimer(20 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func copySessionServiceBindings(bindings sessionbinding.Bindings) sessionbinding.Bindings {
	out := make(sessionbinding.Bindings, len(bindings))
	for id, r := range bindings {
		out[id] = r
	}
	return out
}

func servingCandidates(snapshot session.SessionServiceSnapshot, id string) []session.SessionServiceMember {
	out := []session.SessionServiceMember{}
	for _, member := range snapshot.Members {
		if _, ok := member.Bindings[id]; ok {
			out = append(out, member)
		}
	}
	return out
}

func servingMemberAvailable(member session.SessionServiceMember) bool {
	return !member.Deleting && member.Phase == "Running" && member.Ready && member.Route.IP != "" && member.Route.State == "running"
}

// allocationRoute excludes the member's runtime credential from admission results.
func allocationRoute(member session.SessionServiceMember) sandboxroute.Route {
	route := member.Route
	route.AccessToken = ""
	return route
}

// Create applies affinity and capacity to a newly generated Session identity.
// Any uncertain write pins this operation to verifying the original target.
func (a *Allocator) Create(ctx context.Context, parent session.SessionService, sessionID string) (sandboxroute.Route, error) {
	return a.allocate(ctx, parent, sessionID, true)
}

// Resolve never creates a missing binding. A verified BestEffort move retains
// permission to rebind the same identity within this operation only.
func (a *Allocator) Resolve(ctx context.Context, parent session.SessionService, sessionID string) (sandboxroute.Route, error) {
	return a.allocate(ctx, parent, sessionID, false)
}

func (a *Allocator) allocate(ctx context.Context, parent session.SessionService, sessionID string, mayCreate bool) (sandboxroute.Route, error) {
	if !sessionbinding.ValidSessionID(sessionID) {
		return sandboxroute.Route{}, session.ErrInvalidSession
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	admitted := parent

	unlock, err := a.lockSessionServiceSession(ctx, parent.UID+"/"+sessionID)
	if err != nil {
		return sandboxroute.Route{}, allocationUnavailable("SessionAllocationUnavailable", err)
	}
	defer unlock()
	newTarget := ""
	unknownNew := false
	storageFull := map[string]string{}
	mutationStarted := false
	for {
		// Every decision starts with a usable snapshot and the admitted policy.
		snapshot, err := a.readAllocationSnapshot(ctx, parent)
		if err != nil {
			return sandboxroute.Route{}, err
		}
		parent = snapshot.SessionService
		if err := validateAllocationSessionService(parent, admitted, mutationStarted); err != nil {
			return sandboxroute.Route{}, err
		}

		candidates := servingCandidates(snapshot, sessionID)
		// An uncertain new write must be observed on its original target first.
		if unknownNew {
			// The original target may have vanished or been replaced by a newer version.
			if !slices.ContainsFunc(candidates, func(member session.SessionServiceMember) bool { return member.UID == newTarget }) {
				return sandboxroute.Route{}, allocationUnavailable("SessionAllocationUnavailable", nil)
			}
			unknownNew = false
		}

		switch len(candidates) {
		case 1: // Reuse the binding, or unlink it before a BestEffort rebind.
			mayCreate = true
			member := candidates[0]
			if servingMemberAvailable(member) {
				return allocationRoute(member), nil
			}
			// Shared routing observes a requested pause before Phase catches up.
			if newTarget != "" || parent.Affinity == "Strict" || member.Phase == "Paused" || member.Phase == "Resuming" || member.Route.State == "paused" {
				return sandboxroute.Route{}, allocationUnavailable("BoundSandboxUnavailable", nil)
			}
			// BestEffort unlinks precisely the failed record observed in this snapshot.
			// Confirm the removal in a fresh snapshot before writing a new binding.
			bindings := copySessionServiceBindings(member.Bindings)
			delete(bindings, sessionID)
			mutationStarted = true
			_, err = a.store.ReplaceSessionServiceBindings(ctx, snapshot, member, bindings)
			if err != nil && !retryableWriteError(err) {
				return sandboxroute.Route{}, backendError(err)
			}

		case 0: // Select capacity and persist a new binding.
			if !mayCreate {
				return sandboxroute.Route{}, session.ErrSessionNotFound
			}
			if newTarget != "" {
				return sandboxroute.Route{}, allocationUnavailable("SessionAllocationUnavailable", nil)
			}
			target, err := selectAllocationMember(snapshot, storageFull)
			if err != nil {
				return sandboxroute.Route{}, err
			}
			bindings := copySessionServiceBindings(target.Bindings)
			bindings[sessionID] = sessionbinding.Record{LastRequestAt: time.Now().UTC().Format(time.RFC3339)}
			mutationStarted = true
			_, err = a.store.ReplaceSessionServiceBindings(ctx, snapshot, target, bindings)
			if err == nil || errors.Is(err, session.ErrSessionServiceWriteUnknown) {
				newTarget = target.UID
				unknownNew = errors.Is(err, session.ErrSessionServiceWriteUnknown)
			} else if errors.Is(err, session.ErrSessionServiceCapacity) {
				// A member can hit serialized storage budgets before its record
				// limit. Reconsider the full snapshot and try remaining capacity.
				storageFull[target.UID] = target.Version
			} else if !retryableWriteError(err) {
				return sandboxroute.Route{}, backendError(err)
			}

		default: // Ambiguous bindings must never select a route or mutate records.
			return sandboxroute.Route{}, allocationUnavailable("SessionBindingConflict", session.ErrSessionServiceDataAmbiguous)
		}

		// A successful write does not establish the final route. Observe all members again before
		// returning a route or choosing another target after an unlink.
		if err = allocationBackoff(ctx); err != nil {
			return sandboxroute.Route{}, allocationUnavailable("SessionAllocationUnavailable", err)
		}
	}
}

// readAllocationSnapshot waits for a usable cached snapshot within the caller's budget.
func (a *Allocator) readAllocationSnapshot(ctx context.Context, parent session.SessionService) (session.SessionServiceSnapshot, error) {
	for {
		if err := ctx.Err(); err != nil {
			return session.SessionServiceSnapshot{}, allocationUnavailable("SessionAllocationUnavailable", err)
		}
		snapshot, err := a.store.ReadSessionServiceSnapshot(ctx, parent)
		if err == nil {
			return snapshot, nil
		}
		if errors.Is(err, session.ErrSessionServiceNotFound) || !errors.Is(err, session.ErrSessionServiceUnavailable) {
			return session.SessionServiceSnapshot{}, backendError(err)
		}
		if err = allocationBackoff(ctx); err != nil {
			return session.SessionServiceSnapshot{}, allocationUnavailable("SessionAllocationUnavailable", err)
		}
	}
}

func validateAllocationSessionService(current, admitted session.SessionService, mutationStarted bool) error {
	if current.UID != admitted.UID {
		return backendError(session.ErrSessionServiceNotFound)
	}
	if current.Deleting {
		return allocationUnavailable("SessionServiceDeleting", nil)
	}
	if !sameAdmissionPolicy(current, admitted) {
		if mutationStarted {
			// A new admission must not forget this request's uncertain write target.
			return allocationUnavailable("SessionAllocationUnavailable", session.ErrPolicyChanged)
		}
		return session.ErrPolicyChanged
	}
	return nil
}

// Least-session selection counts every durable record. Lowering the user
// limit never evicts existing Sessions; encoding enforces platform budgets.
func selectAllocationMember(snapshot session.SessionServiceSnapshot, storageFull map[string]string) (session.SessionServiceMember, error) {
	eligible := []session.SessionServiceMember{}
	limit := snapshot.SessionService.MaxSessions
	if limit <= 0 || limit > sessionbinding.MaxRecords {
		limit = sessionbinding.MaxRecords
	}
	for _, member := range snapshot.Members {
		if servingMemberAvailable(member) && len(member.Bindings) < limit && storageFull[member.UID] != member.Version {
			eligible = append(eligible, member)
		}
	}
	sort.Slice(eligible, func(i, j int) bool {
		if len(eligible[i].Bindings) != len(eligible[j].Bindings) {
			return len(eligible[i].Bindings) < len(eligible[j].Bindings)
		}
		return eligible[i].UID < eligible[j].UID
	})
	if len(eligible) == 0 {
		return session.SessionServiceMember{}, allocationUnavailable("SessionCapacityExhausted", nil)
	}
	return eligible[0], nil
}

// Controller status and unrelated metadata updates do not change admission.
func sameAdmissionPolicy(current, admitted session.SessionService) bool {
	return current.UID == admitted.UID && current.Namespace == admitted.Namespace && current.Name == admitted.Name && current.Affinity == admitted.Affinity && current.MaxSessions == admitted.MaxSessions && slices.Equal(current.Ports, admitted.Ports)
}
