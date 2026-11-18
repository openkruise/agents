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

// Package kubernetes implements informer-backed SessionService observations and
// conditional binding writes using Sandbox annotations.
package kubernetes

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agents "github.com/openkruise/agents/api/v1alpha1"
	session "github.com/openkruise/agents/pkg/sessionservice"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

// Store reads member bindings from the informer cache and uses the
// shared RV expectations to detect observations older than local writes. Local
// write watermarks also protect a captured List after asynchronous cache
// observation clears a shared expectation. The mutex serializes binding writes and reads
// while a conditional write is in flight and protects the watermarks.
type Store struct {
	client       client.Client
	healthy      func() bool
	sessionLock  chan struct{}
	memberWrites map[types.UID]servingMemberWrite
}

// A watermark belongs to one SessionService membership and retains no Sandbox object.
// Like shared expectations, its timeout starts at the first stale observation.
type servingMemberWrite struct {
	sessionServiceUID string
	version           string
	firstUnsatisfied  time.Time
}

// NewStore uses the process's cached client and informer health signal.
func NewStore(c client.Client, healthy func() bool) *Store {
	return &Store{client: c, healthy: healthy, sessionLock: make(chan struct{}, 1)}
}

// A single permit preserves the full read/write critical section while letting
// callers stop waiting for an unrelated member operation that holds the lock.
func (s *Store) lockSessions(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.sessionLock <- struct{}{}:
		return nil
	}
}

func (s *Store) unlockSessions() { <-s.sessionLock }

func (s *Store) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.healthy == nil || !s.healthy() {
		return session.ErrSessionServiceUnavailable
	}
	return nil
}

func servingValue(parent *agents.SessionService) session.SessionService {
	maxSessions := sessionbinding.MaxRecords
	if limit := parent.Spec.SessionPolicy.MaxSessionsPerSandbox; limit != nil {
		maxSessions = int(*limit)
	}
	return session.SessionService{Namespace: parent.Namespace, Name: parent.Name, UID: string(parent.UID), Version: parent.ResourceVersion, Ports: append([]int32(nil), parent.Spec.Ports...), Deleting: !parent.DeletionTimestamp.IsZero(), Affinity: parent.Spec.SessionPolicy.Affinity, MaxSessions: maxSessions}
}

func (s *Store) GetSessionService(ctx context.Context, namespace, name string) (session.SessionService, error) {
	if err := s.ready(ctx); err != nil {
		return session.SessionService{}, err
	}
	var parent agents.SessionService
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &parent); err != nil {
		if apierrors.IsNotFound(err) {
			return session.SessionService{}, session.ErrSessionServiceNotFound
		}
		return session.SessionService{}, fmt.Errorf("lookup SessionService: %w", err)
	}
	return servingValue(&parent), nil
}

// membersForParent reads members of an already validated parent snapshot.
func (s *Store) membersForParent(ctx context.Context, parent *agents.SessionService) ([]*agents.Sandbox, error) {
	serving := servingValue(parent)
	var list agents.SandboxList
	if err := s.client.List(ctx, &list, client.InNamespace(serving.Namespace), client.MatchingLabels{sessionbinding.SessionServiceUIDLabel: serving.UID}); err != nil {
		return nil, err
	}
	out := make([]*agents.Sandbox, 0, len(list.Items))
	currentUIDs := make(map[types.UID]struct{}, len(list.Items))
	for i := range list.Items {
		box := &list.Items[i]
		owner := metav1.GetControllerOf(box)
		if owner == nil || string(owner.UID) != serving.UID || owner.Name != serving.Name || owner.Kind != "SessionService" || owner.APIVersion != agents.GroupVersion.String() {
			continue
		}
		out = append(out, box)
		currentUIDs[box.UID] = struct{}{}
	}
	for uid, write := range s.memberWrites {
		_, current := currentUIDs[uid]
		if (write.sessionServiceUID == serving.UID && !current) || (!write.firstUnsatisfied.IsZero() && time.Since(write.firstUnsatisfied) > expectations.ExpectationTimeout) {
			delete(s.memberWrites, uid)
		}
	}
	for _, box := range out {
		sharedSatisfied := expectations.ResourceVersionExpectationSatisfied(box)
		if write, exists := s.memberWrites[box.UID]; exists {
			if !expectations.IsResourceVersionNewer(write.version, box.ResourceVersion) {
				if write.firstUnsatisfied.IsZero() {
					write.firstUnsatisfied = time.Now()
					s.memberWrites[box.UID] = write
				}
				return nil, fmt.Errorf("%w: waiting for fresh member %s", session.ErrSessionServiceUnavailable, box.Name)
			}
			// Reads and writes hold the session lock across List and this check. Once this
			// backend accepts a fresh snapshot, it has no older request still
			// awaiting validation. Revisit this if the lock scope changes.
			delete(s.memberWrites, box.UID)
		}
		// Skipping a stale member would silently omit its other durable bindings.
		// Reject the entire snapshot until the shared expectation is satisfied.
		if !sharedSatisfied {
			return nil, fmt.Errorf("%w: waiting for fresh member %s", session.ErrSessionServiceUnavailable, box.Name)
		}
	}
	return out, nil
}
