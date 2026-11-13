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
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/sandboxroute"
	session "github.com/openkruise/agents/pkg/servingsession"
	sessionbinding "github.com/openkruise/agents/pkg/servingsession/binding"
)

// Allocator applies Session affinity and capacity to shared, versioned observations.
// Its locks coordinate only requests within this process.
type Allocator struct {
	store        session.Store
	mu           sync.Mutex
	sessionLocks map[string]*sessionLock
	warnings     map[string]string
}

// New assembles an allocator over the shared persistence contract.
func New(store session.Store) *Allocator { return &Allocator{store: store} }

func backendError(err error) error {
	if errors.Is(err, session.ErrServingNotFound) {
		return err
	}
	return &session.Error{Reason: "SessionManagerUnavailable", Cause: err}
}

type sessionLock struct {
	token chan struct{}
	users int
}

// Allocation is serialized only for one local Serving UID and Session identity.
// Waiting callers keep their own cancellation and two-second operation budget.
func (a *Allocator) lockServingSession(ctx context.Context, key string) (func(), error) {
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
	return errors.Is(err, session.ErrServingConflict) || errors.Is(err, session.ErrServingUnavailable) || errors.Is(err, session.ErrServingWriteUnknown)
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

func copyServingBindings(bindings sessionbinding.Bindings) sessionbinding.Bindings {
	out := make(sessionbinding.Bindings, len(bindings))
	for id, r := range bindings {
		out[id] = r
	}
	return out
}

func servingCandidates(snapshot session.ServingSnapshot, id string) []session.ServingMember {
	out := []session.ServingMember{}
	for _, member := range snapshot.Members {
		if _, ok := member.Bindings[id]; ok {
			out = append(out, member)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

func servingMemberAvailable(member session.ServingMember) bool {
	return !member.Deleting && member.Phase == "Running" && member.Ready && member.Route.IP != "" && member.Route.State == "running"
}

// allocationRoute excludes the member's runtime credential from admission results.
func allocationRoute(member session.ServingMember) sandboxroute.Route {
	route := member.Route
	route.AccessToken = ""
	return route
}

// Allocate applies affinity and capacity to versioned observations.
// Any uncertain new write pins this request to verifying that original target;
// a later request can allocate afresh if no binding survived.
func (a *Allocator) Allocate(ctx context.Context, parent session.Serving, sessionID string) (sandboxroute.Route, error) {
	if !sessionbinding.ValidSessionID(sessionID) {
		return sandboxroute.Route{}, session.ErrInvalidSession
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	admitted := parent

	unlock, err := a.lockServingSession(ctx, parent.UID+"/"+sessionID)
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
		parent = snapshot.Serving
		if err := validateAllocationServing(parent, admitted, mutationStarted); err != nil {
			return sandboxroute.Route{}, err
		}

		candidates := servingCandidates(snapshot, sessionID)
		// An uncertain new write must be observed on its original target first.
		if unknownNew {
			// The original target may have vanished or been replaced by a newer version.
			if !slices.ContainsFunc(candidates, func(member session.ServingMember) bool { return member.UID == newTarget }) {
				return sandboxroute.Route{}, allocationUnavailable("SessionAllocationUnavailable", nil)
			}
			unknownNew = false
		}
		a.observeServingConvergence(ctx, snapshot, sessionID, candidates)

		switch len(candidates) {
		case 1: // Reuse the binding, or unlink it before a BestEffort rebind.
			member := candidates[0]
			if servingMemberAvailable(member) {
				return allocationRoute(member), nil
			}
			// Shared routing observes a requested pause before Phase catches up.
			if newTarget != "" || parent.Affinity == "Strict" || member.Phase == "Paused" || member.Phase == "Resuming" || member.Route.State == "paused" {
				return sandboxroute.Route{}, allocationUnavailable("BoundSandboxUnavailable", nil)
			}
			// BestEffort unlinks precisely the failed record observed in this snapshot.
			// An ACK or a later complete snapshot proving absence precedes any new write.
			bindings := copyServingBindings(member.Bindings)
			delete(bindings, sessionID)
			mutationStarted = true
			_, err = a.store.ReplaceServingBindings(ctx, snapshot, member, bindings)
			if err != nil && !retryableWriteError(err) {
				return sandboxroute.Route{}, backendError(err)
			}

		case 0: // Select capacity and persist a new binding.
			if newTarget != "" {
				return sandboxroute.Route{}, allocationUnavailable("SessionAllocationUnavailable", nil)
			}
			target, err := selectAllocationMember(snapshot, storageFull)
			if err != nil {
				return sandboxroute.Route{}, err
			}
			bindings := copyServingBindings(target.Bindings)
			bindings[sessionID] = sessionbinding.Record{State: sessionbinding.Active, LastRequestAt: time.Now().UTC().Format(time.RFC3339)}
			mutationStarted = true
			_, err = a.store.ReplaceServingBindings(ctx, snapshot, target, bindings)
			if err == nil || errors.Is(err, session.ErrServingWriteUnknown) {
				newTarget = target.UID
				unknownNew = errors.Is(err, session.ErrServingWriteUnknown)
			} else if errors.Is(err, session.ErrServingCapacity) {
				// A member can hit serialized storage budgets before its record
				// limit. Reconsider the full snapshot and try remaining capacity.
				storageFull[target.UID] = target.Version
			} else if !retryableWriteError(err) {
				return sandboxroute.Route{}, backendError(err)
			}

		default: // Repair duplicate bindings before returning any route.
			mutationStarted = true
			if err = a.convergeServingSession(ctx, snapshot, sessionID, candidates); err != nil {
				if retryableWriteError(err) {
					if err = allocationBackoff(ctx); err == nil {
						continue
					}
				}
				return sandboxroute.Route{}, backendError(err)
			}
		}

		// A write ACK is not the final route. Observe all members again before
		// returning a route or choosing another target after an unlink.
		if err = allocationBackoff(ctx); err != nil {
			return sandboxroute.Route{}, allocationUnavailable("SessionAllocationUnavailable", err)
		}
	}
}

// readAllocationSnapshot waits for a usable cached snapshot within the caller's budget.
func (a *Allocator) readAllocationSnapshot(ctx context.Context, parent session.Serving) (session.ServingSnapshot, error) {
	for {
		if err := ctx.Err(); err != nil {
			return session.ServingSnapshot{}, allocationUnavailable("SessionAllocationUnavailable", err)
		}
		snapshot, err := a.store.ReadServingSnapshot(ctx, parent)
		if err == nil {
			return snapshot, nil
		}
		if errors.Is(err, session.ErrServingNotFound) || !errors.Is(err, session.ErrServingUnavailable) {
			return session.ServingSnapshot{}, backendError(err)
		}
		if err = allocationBackoff(ctx); err != nil {
			return session.ServingSnapshot{}, allocationUnavailable("SessionAllocationUnavailable", err)
		}
	}
}

func validateAllocationServing(current, admitted session.Serving, mutationStarted bool) error {
	if current.UID != admitted.UID {
		return backendError(session.ErrServingNotFound)
	}
	if current.Deleting {
		return allocationUnavailable("ServingDeleting", nil)
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
func selectAllocationMember(snapshot session.ServingSnapshot, storageFull map[string]string) (session.ServingMember, error) {
	eligible := []session.ServingMember{}
	limit := snapshot.Serving.MaxSessions
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
		return session.ServingMember{}, allocationUnavailable("SessionCapacityExhausted", nil)
	}
	return eligible[0], nil
}

// Complete an observed conflict only after a full snapshot proves one binding.
// This may happen on a later retry or sweep, or after another allocator's cleanup.
func (a *Allocator) observeServingConvergence(ctx context.Context, snapshot session.ServingSnapshot, id string, candidates []session.ServingMember) {
	if len(candidates) > 1 {
		return
	}
	a.mu.Lock()
	_, observedConflict := a.warnings[snapshot.Serving.UID+"/"+id+"/SessionBindingConflict"]
	a.mu.Unlock()
	if len(candidates) == 1 && observedConflict {
		a.servingWarning(ctx, "SessionBindingConverged", snapshot.Serving, id, candidates, "unique binding observed after duplicate conflict")
	}
	// No remaining binding is not convergence; close/deletion can also remove it.
	a.ForgetSession(snapshot.Serving.UID, id)
}

func (a *Allocator) servingWarning(ctx context.Context, event string, parent session.Serving, id string, candidates []session.ServingMember, reason string) {
	uids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		uids = append(uids, candidate.UID)
	}
	signature := event + ":" + strings.Join(uids, ",") + ":" + reason
	key := parent.UID + "/" + id + "/" + event
	a.mu.Lock()
	if a.warnings == nil {
		a.warnings = map[string]string{}
	}
	previous := a.warnings[key]
	a.warnings[key] = signature
	a.mu.Unlock()
	if previous == signature {
		return
	}
	winner := ""
	if len(uids) > 0 {
		winner = uids[0]
	}
	klog.FromContext(ctx).Info("WARNING: "+event, "severity", "warning", "event", event, "namespace", parent.Namespace, "serving", parent.Name, "servingUID", parent.UID, "sessionID", id, "candidateUIDs", uids, "winnerUID", winner, "reason", reason)
}

// Each iteration conditionally confirms the winner without changing its record,
// then removes at most one loser against a fresh full snapshot. The next loser
// always receives a new winner validation.
func (a *Allocator) convergeServingSession(ctx context.Context, snapshot session.ServingSnapshot, id string, candidates []session.ServingMember) (err error) {
	a.servingWarning(ctx, "SessionBindingConflict", snapshot.Serving, id, candidates, "duplicate bindings observed")
	defer func() {
		if err != nil {
			a.servingWarning(ctx, "SessionBindingConvergenceFailed", snapshot.Serving, id, candidates, fmt.Sprint(err))
		}
	}()
	winner := candidates[0]
	confirmedVersion, err := a.store.ReplaceServingBindings(ctx, snapshot, winner, winner.Bindings)
	if err != nil {
		return err
	}
	fresh, err := a.store.ReadServingSnapshot(ctx, snapshot.Serving)
	if err != nil {
		return err
	}
	current := servingCandidates(fresh, id)
	// A vanished or replaced winner must never authorize deleting another copy.
	if len(current) == 0 || current[0].UID != winner.UID || current[0].Version != confirmedVersion {
		return session.ErrServingConflict
	}
	if len(current) == 1 {
		a.observeServingConvergence(ctx, fresh, id, current)
		return nil
	}
	loser := current[1]
	remaining := copyServingBindings(loser.Bindings)
	delete(remaining, id)
	if _, err = a.store.ReplaceServingBindings(ctx, fresh, loser, remaining); err != nil {
		return err
	}
	after, err := a.store.ReadServingSnapshot(ctx, snapshot.Serving)
	if err != nil {
		return err
	}
	a.observeServingConvergence(ctx, after, id, servingCandidates(after, id))
	return nil
}

// Maintain repairs duplicate bindings and prunes local warning history. A broken
// parent/session is isolated so the remainder of the sweep still runs.
func (a *Allocator) Maintain(ctx context.Context) error {
	parents, err := a.store.ListServings(ctx)
	if err != nil {
		return err
	}
	// A successful complete parent list bounds process-local warning state.
	live := map[string]bool{}
	for _, parent := range parents {
		if !parent.Deleting {
			live[parent.UID] = true
		}
	}
	a.mu.Lock()
	for key := range a.warnings {
		uid, _, _ := strings.Cut(key, "/")
		if !live[uid] {
			delete(a.warnings, key)
		}
	}
	a.mu.Unlock()
	for _, parent := range parents {
		if err := ctx.Err(); err != nil {
			return err
		}
		if parent.Deleting {
			continue
		}
		child, cancel := context.WithTimeout(ctx, 2*time.Second)
		snapshot, err := a.store.ReadServingSnapshot(child, parent)
		if err == nil && !snapshot.Serving.Deleting {
			counts := map[string]int{}
			for _, member := range snapshot.Members {
				for id := range member.Bindings {
					counts[id]++
				}
			}
			ids := map[string]struct{}{}
			for id, count := range counts {
				if count > 1 {
					ids[id] = struct{}{}
				}
			}
			// Warning history can be newer than this snapshot. Recheck those
			// Sessions under their local lock before deciding to clear it.
			a.mu.Lock()
			prefix := parent.UID + "/"
			for key := range a.warnings {
				if strings.HasPrefix(key, prefix) {
					id, _, _ := strings.Cut(strings.TrimPrefix(key, prefix), "/")
					ids[id] = struct{}{}
				}
			}
			a.mu.Unlock()
			for id := range ids {
				if child.Err() != nil {
					break
				}
				unlock, e := a.lockServingSession(child, parent.UID+"/"+id)
				if e != nil {
					err = e
					break
				}
				fresh, e := a.store.ReadServingSnapshot(child, parent)
				if e == nil {
					candidates := servingCandidates(fresh, id)
					if len(candidates) <= 1 {
						a.observeServingConvergence(child, fresh, id, candidates)
					}
					if len(candidates) > 1 {
						e = a.convergeServingSession(child, fresh, id, candidates)
					}
				}
				unlock()
				if e != nil {
					err = e
					klog.FromContext(child).Error(e, "repair Serving session", "servingUID", parent.UID, "sessionID", id)
				}
			}
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			klog.FromContext(ctx).Error(err, "maintain Serving sessions", "servingUID", parent.UID)
		}
	}
	return ctx.Err()
}

// ForgetSession drops local warning history after Close or an unambiguous observation.
func (a *Allocator) ForgetSession(uid, sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	prefix := uid + "/" + sessionID + "/"
	for key := range a.warnings {
		if strings.HasPrefix(key, prefix) {
			delete(a.warnings, key)
		}
	}
}

// Controller status and unrelated metadata updates do not change admission.
func sameAdmissionPolicy(current, admitted session.Serving) bool {
	return current.UID == admitted.UID && current.ID == admitted.ID && current.Affinity == admitted.Affinity && current.MaxSessions == admitted.MaxSessions && slices.Equal(current.Ports, admitted.Ports)
}
