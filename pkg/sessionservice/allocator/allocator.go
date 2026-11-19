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
// Waiting callers keep their own cancellation and ten-second operation budget.
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

// Resolve never creates a missing binding. BestEffort requests a durable
// controller-driven move and waits within the caller's allocation budget.
func (a *Allocator) Resolve(ctx context.Context, parent session.SessionService, sessionID string) (sandboxroute.Route, error) {
	return a.allocate(ctx, parent, sessionID, false)
}

func (a *Allocator) allocate(ctx context.Context, parent session.SessionService, sessionID string, mayCreate bool) (sandboxroute.Route, error) {
	if !sessionbinding.ValidSessionID(sessionID) {
		return sandboxroute.Route{}, session.ErrInvalidSession
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
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
	observedMoving := false
	for {
		// Every decision starts with a usable snapshot and the admitted policy.
		snapshot, err := a.readAllocationSnapshot(ctx, parent)
		if err != nil {
			if observedMoving && ctx.Err() != nil {
				return sandboxroute.Route{}, allocationUnavailable("SessionRebinding", err)
			}
			return sandboxroute.Route{}, err
		}
		parent = snapshot.SessionService
		if err := validateAllocationSessionService(parent, admitted, mutationStarted); err != nil {
			return sandboxroute.Route{}, err
		}

		candidates := servingCandidates(snapshot, sessionID)
		for _, member := range candidates {
			deadline, err := sessionbinding.ExpiresAt(member.Bindings[sessionID], parent.ExpireAfterCreation)
			if err != nil {
				return sandboxroute.Route{}, backendError(err)
			}
			if !deadline.IsZero() && !time.Now().Before(deadline) {
				return sandboxroute.Route{}, session.ErrSessionNotFound
			}
		}
		// An uncertain new write must be observed on its original target first.
		if unknownNew {
			// The original target may have vanished or been replaced by a newer version.
			if !slices.ContainsFunc(candidates, func(member session.SessionServiceMember) bool { return member.UID == newTarget }) {
				return sandboxroute.Route{}, allocationUnavailable("SessionAllocationUnavailable", nil)
			}
			unknownNew = false
		}

		switch len(candidates) {
		case 1: // Reuse Active bindings; only the controller completes Moving.
			mayCreate = false
			member := candidates[0]
			if member.Bindings[sessionID].Phase == sessionbinding.Moving {
				observedMoving = true
				break
			}
			if servingMemberAvailable(member) {
				return allocationRoute(member), nil
			}
			// Shared routing observes a requested pause before Phase catches up.
			if newTarget != "" || parent.Affinity == "Strict" || member.Phase == "Paused" || member.Phase == "Resuming" || member.Route.State == "paused" {
				return sandboxroute.Route{}, allocationUnavailable("BoundSandboxUnavailable", nil)
			}
			mutationStarted = true
			observedMoving = true
			if err := a.requestSessionMove(ctx, snapshot, member, sessionID); err != nil {
				return sandboxroute.Route{}, err
			}

		case 0: // Select capacity and persist a new binding.
			if !mayCreate {
				// Source deletion can precede target observation in another Gateway.
				// Wait before classifying an ID never observed Moving as unknown.
				if err := allocationBackoff(ctx); err != nil {
					if observedMoving {
						return sandboxroute.Route{}, allocationUnavailable("SessionRebinding", err)
					}
					return sandboxroute.Route{}, session.ErrSessionNotFound
				}
				continue
			}
			if newTarget != "" {
				return sandboxroute.Route{}, allocationUnavailable("SessionAllocationUnavailable", nil)
			}
			// A capacity rejection retries another observed member. Success or an
			// unknown result pins subsequent observations to the selected target.
			target, err := selectAllocationMember(snapshot, storageFull)
			if err != nil {
				return sandboxroute.Route{}, err
			}
			mutationStarted = true
			newTarget, unknownNew, err = a.createSessionBinding(ctx, snapshot, target, sessionID, storageFull)
			if err != nil {
				return sandboxroute.Route{}, err
			}

		default:
			if movingBindingPair(candidates, sessionID) {
				observedMoving = true
				break
			}
			return sandboxroute.Route{}, allocationUnavailable("SessionBindingConflict", session.ErrSessionServiceDataAmbiguous)
		}

		// Initial writes and Moving requests both require another observation.
		// Only the controller may create a replacement for an existing Session.
		if err = allocationBackoff(ctx); err != nil {
			reason := "SessionAllocationUnavailable"
			if observedMoving {
				reason = "SessionRebinding"
			}
			return sandboxroute.Route{}, allocationUnavailable(reason, err)
		}
	}
}

// requestSessionMove preserves the source binding until the controller finishes
// migration. Conflicts and unknown writes are resolved by another observation.
func (a *Allocator) requestSessionMove(ctx context.Context, snapshot session.SessionServiceSnapshot, member session.SessionServiceMember, id string) error {
	bindings := copySessionServiceBindings(member.Bindings)
	record := bindings[id]
	record.Phase = sessionbinding.Moving
	bindings[id] = record
	_, err := a.store.ReplaceSessionServiceBindings(ctx, snapshot, member, bindings)
	if err != nil && !retryableWriteError(err) {
		return backendError(err)
	}
	return nil
}

// createSessionBinding returns a pinned target after success or an unknown write.
// An empty target means retry from a new snapshot. Storage-budget rejections are
// recorded by UID/RV so selection can try other members without replaying them.
func (a *Allocator) createSessionBinding(ctx context.Context, snapshot session.SessionServiceSnapshot, target session.SessionServiceMember, id string, storageFull map[string]string) (targetUID string, writeUnknown bool, err error) {
	bindings := copySessionServiceBindings(target.Bindings)
	bindings[id] = sessionbinding.Record{CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	_, err = a.store.ReplaceSessionServiceBindings(ctx, snapshot, target, bindings)
	switch {
	case err == nil, errors.Is(err, session.ErrSessionServiceWriteUnknown):
		return target.UID, errors.Is(err, session.ErrSessionServiceWriteUnknown), nil
	case errors.Is(err, session.ErrSessionServiceCapacity):
		storageFull[target.UID] = target.Version
	case !retryableWriteError(err):
		return "", false, backendError(err)
	}
	return "", false, nil
}

// movingBindingPair recognizes the controller's temporary source/target overlap.
// Other duplicate bindings remain conflicts rather than being arbitrated here.
func movingBindingPair(candidates []session.SessionServiceMember, id string) bool {
	if len(candidates) != 2 {
		return false
	}
	first, second := candidates[0], candidates[1]
	a, b := first.Bindings[id], second.Bindings[id]
	return (a.Phase == sessionbinding.Moving && a.TargetSandboxUID == second.UID && b.Phase == "") ||
		(b.Phase == sessionbinding.Moving && b.TargetSandboxUID == first.UID && a.Phase == "")
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
	return current.UID == admitted.UID && current.Namespace == admitted.Namespace && current.Name == admitted.Name && current.Affinity == admitted.Affinity && current.MaxSessions == admitted.MaxSessions && current.ExpireAfterCreation == admitted.ExpireAfterCreation && slices.Equal(current.Ports, admitted.Ports)
}
