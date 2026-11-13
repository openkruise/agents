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

// Package kubernetes implements informer-backed Serving observations and
// conditional binding writes using Sandbox annotations.
package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agents "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandboxid"
	session "github.com/openkruise/agents/pkg/servingsession"
	sessionbinding "github.com/openkruise/agents/pkg/servingsession/binding"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

// Options preserves the process's Sandbox cache scope during Session operations.
type Options struct {
	LabelSelector string
	// RequireMatchingTemplateScope rejects allocation when template labels cannot
	// establish visibility of all members. Manager queries retain warning-only behavior.
	RequireMatchingTemplateScope bool
}

// Store reads member bindings from the informer cache and uses the
// shared RV expectations to detect observations older than local writes. Local
// write watermarks also protect a captured List after asynchronous cache
// observation clears a shared expectation. The mutex serializes Close and reads
// while a conditional write is in flight and protects the watermarks.
type Store struct {
	requireMatchingTemplateScope bool
	client                       client.Client
	healthy                      func() bool
	labelSelector                string
	sessionLock                  chan struct{}
	memberWrites                 map[types.UID]servingMemberWrite
}

// A watermark belongs to one Serving membership and retains no Sandbox object.
// Like shared expectations, its timeout starts at the first stale observation.
type servingMemberWrite struct {
	servingUID       string
	version          string
	firstUnsatisfied time.Time
}

// NewStore uses the process's cached client and informer health signal.
func NewStore(c client.Client, healthy func() bool, opts Options) *Store {
	return &Store{client: c, healthy: healthy, labelSelector: opts.LabelSelector, requireMatchingTemplateScope: opts.RequireMatchingTemplateScope, sessionLock: make(chan struct{}, 1)}
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
		return session.ErrServingUnavailable
	}
	return nil
}

func servingValue(parent *agents.SandboxServing) session.Serving {
	maxSessions := sessionbinding.MaxRecords
	if limit := parent.Spec.SessionPolicy.MaxSessionsPerSandbox; limit != nil {
		maxSessions = int(*limit)
	}
	return session.Serving{Namespace: parent.Namespace, Name: parent.Name, UID: string(parent.UID), ID: parent.Labels[sessionbinding.ServingIDLabel], Version: parent.ResourceVersion, Ports: append([]int32(nil), parent.Spec.Ports...), Deleting: !parent.DeletionTimestamp.IsZero(), Affinity: parent.Spec.SessionPolicy.Affinity, MaxSessions: maxSessions}
}

func (s *Store) GetServing(ctx context.Context, namespace, id string) (session.Serving, error) {
	if err := s.ready(ctx); err != nil {
		return session.Serving{}, err
	}
	var parents agents.SandboxServingList
	if err := s.client.List(ctx, &parents, client.InNamespace(namespace), client.MatchingFields{sessionbinding.ServingIDIndex: id}); err != nil {
		return session.Serving{}, fmt.Errorf("lookup Serving: %w", err)
	}
	if len(parents.Items) == 0 {
		return session.Serving{}, session.ErrServingNotFound
	}
	if len(parents.Items) != 1 {
		return session.Serving{}, session.ErrServingDataAmbiguous
	}
	return servingValue(&parents.Items[0]), nil
}

func (s *Store) ListServings(ctx context.Context) ([]session.Serving, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	var parents agents.SandboxServingList
	if err := s.client.List(ctx, &parents); err != nil {
		return nil, err
	}
	out := make([]session.Serving, 0, len(parents.Items))
	for i := range parents.Items {
		out = append(out, servingValue(&parents.Items[i]))
	}
	return out, nil
}

func (s *Store) AssignServingID(ctx context.Context, serving session.Serving, id string) (session.Serving, error) {
	if err := s.ready(ctx); err != nil {
		return session.Serving{}, err
	}
	var parent agents.SandboxServing
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: serving.Namespace, Name: serving.Name}, &parent); err != nil {
		if apierrors.IsNotFound(err) {
			return session.Serving{}, session.ErrServingNotFound
		}
		return session.Serving{}, err
	}
	if string(parent.UID) != serving.UID {
		return session.Serving{}, session.ErrServingNotFound
	}
	if current := parent.Labels[sessionbinding.ServingIDLabel]; current != "" {
		return servingValue(&parent), nil
	}
	if parent.ResourceVersion != serving.Version {
		return session.Serving{}, session.ErrServingConflict
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": parent.UID, "resourceVersion": parent.ResourceVersion, "labels": map[string]string{sessionbinding.ServingIDLabel: id}}})
	if err != nil {
		return session.Serving{}, err
	}
	updated := parent.DeepCopy()
	// Only metadata is sent: the typed parent must never round-trip unknown
	// embedded Pod/PVC fields while assigning a Manager-owned identity.
	if err := s.client.Patch(ctx, updated, client.RawPatch(types.MergePatchType, patch)); err != nil {
		if apierrors.IsConflict(err) {
			return session.Serving{}, fmt.Errorf("%w: %v", session.ErrServingConflict, err)
		}
		return session.Serving{}, err
	}
	return servingValue(updated), nil
}

// Session queries observe only members within the configured cache scope. Users
// must configure matching labels on both the Serving and its member template.
func (s *Store) members(ctx context.Context, serving session.Serving) ([]*agents.Sandbox, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	var parent agents.SandboxServing
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: serving.Namespace, Name: serving.Name}, &parent); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, session.ErrServingNotFound
		}
		return nil, err
	}
	if string(parent.UID) != serving.UID || parent.Labels[sessionbinding.ServingIDLabel] != serving.ID {
		return nil, session.ErrServingNotFound
	}
	return s.membersForParent(ctx, &parent)
}

// membersForParent reads members of an already validated parent snapshot.
func (s *Store) membersForParent(ctx context.Context, parent *agents.SandboxServing) ([]*agents.Sandbox, error) {
	serving := servingValue(parent)
	template := parent.Spec.Provision.SandboxTemplate
	if s.requireMatchingTemplateScope && s.labelSelector != "" && (template == nil || template.Template == nil) {
		return nil, fmt.Errorf("%w: Serving template cannot establish member cache scope", session.ErrServingUnavailable)
	}
	if s.labelSelector != "" && template != nil && template.Template != nil {
		selector, err := labels.Parse(s.labelSelector)
		if err != nil {
			return nil, fmt.Errorf("%w: parse Sandbox cache selector: %v", session.ErrServingUnavailable, err)
		}
		if !selector.Matches(labels.Set(template.Template.Labels)) {
			if s.requireMatchingTemplateScope {
				return nil, fmt.Errorf("%w: Serving template is outside the member cache scope", session.ErrServingUnavailable)
			}
			// The contextual logger has no Warn method. Keep this diagnostic
			// structured, with an explicit warning marker, without rejecting
			// operations or widening the cache scope to search for hidden members.
			klog.FromContext(ctx).Info("SandboxServing template labels do not match the Manager cache selector; member Sandboxes may be hidden",
				"severity", "warning", "serving", klog.KObj(parent), "selector", s.labelSelector,
				"labelPath", "spec.provision.sandboxTemplate.template.metadata.labels")
		}
	}
	var list agents.SandboxList
	if err := s.client.List(ctx, &list, client.InNamespace(serving.Namespace), client.MatchingLabels{sessionbinding.ServingUIDLabel: serving.UID}); err != nil {
		return nil, err
	}
	out := make([]*agents.Sandbox, 0, len(list.Items))
	currentUIDs := make(map[types.UID]struct{}, len(list.Items))
	for i := range list.Items {
		box := &list.Items[i]
		owner := metav1.GetControllerOf(box)
		if owner == nil || string(owner.UID) != serving.UID || owner.Name != serving.Name || owner.Kind != "SandboxServing" || owner.APIVersion != agents.GroupVersion.String() {
			continue
		}
		out = append(out, box)
		currentUIDs[box.UID] = struct{}{}
	}
	for uid, write := range s.memberWrites {
		_, current := currentUIDs[uid]
		if (write.servingUID == serving.UID && !current) || (!write.firstUnsatisfied.IsZero() && time.Since(write.firstUnsatisfied) > expectations.ExpectationTimeout) {
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
				return nil, fmt.Errorf("%w: waiting for fresh member %s", session.ErrServingUnavailable, box.Name)
			}
			// Reads and writes hold the session lock across List and this check. Once this
			// backend accepts a fresh snapshot, it has no older request still
			// awaiting validation. Revisit this if the lock scope changes.
			delete(s.memberWrites, box.UID)
		}
		// Skipping a stale member would silently omit its other durable bindings.
		// Reject the entire snapshot until the shared expectation is satisfied.
		if !sharedSatisfied {
			return nil, fmt.Errorf("%w: waiting for fresh member %s", session.ErrServingUnavailable, box.Name)
		}
	}
	return out, nil
}

func memberSessions(members []*agents.Sandbox) ([]session.ServingSession, error) {
	out := make([]session.ServingSession, 0)
	seen := map[string]struct{}{}
	for _, box := range members {
		bindings, err := sessionbinding.Decode(box.Annotations)
		if err != nil {
			return nil, fmt.Errorf("%w: member %s: %v", session.ErrServingDataAmbiguous, box.Name, err)
		}
		for id, record := range bindings {
			if _, found := seen[id]; found {
				return nil, fmt.Errorf("%w: duplicate session %s", session.ErrServingDataAmbiguous, id)
			}
			seen[id] = struct{}{}
			out = append(out, session.ServingSession{SessionID: id, SandboxID: sandboxid.Resolve(box), State: record.State, LastRequestAt: record.LastRequestAt})
		}
	}
	return out, nil
}

func (s *Store) ReadServingSessions(ctx context.Context, serving session.Serving) ([]session.ServingSession, error) {
	if err := s.lockSessions(ctx); err != nil {
		return nil, err
	}
	defer s.unlockSessions()
	members, err := s.members(ctx, serving)
	if err != nil {
		return nil, err
	}
	return memberSessions(members)
}

func (s *Store) CloseServingSession(ctx context.Context, serving session.Serving, id string) error {
	if err := s.lockSessions(ctx); err != nil {
		return err
	}
	defer s.unlockSessions()
	deadline := time.NewTimer(200 * time.Millisecond)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	conflictedVersion := ""
	for {
		members, err := s.members(ctx, serving)
		if err != nil {
			return err
		}
		// Close releases every observed copy of this Session. Queries still
		// reject duplicates until deterministic repair has run.
		var box *agents.Sandbox
		var targetBindings sessionbinding.Bindings
		copies := 0
		for _, member := range members {
			bindings, err := sessionbinding.Decode(member.Annotations)
			if err != nil {
				return fmt.Errorf("%w: member %s: %v", session.ErrServingDataAmbiguous, member.Name, err)
			}
			if _, exists := bindings[id]; exists {
				copies++
				if box == nil {
					box = member
					targetBindings = bindings
				}
			}
		}
		if box == nil {
			return nil
		}
		if box.ResourceVersion != conflictedVersion {
			delete(targetBindings, id)
			annotations, err := sessionbinding.Encode(box.Annotations, targetBindings)
			if err != nil {
				return fmt.Errorf("%w: %v", session.ErrServingDataAmbiguous, err)
			}
			_, err = s.patchMemberBindings(ctx, serving.UID, box, annotations[sessionbinding.BindingsAnnotation])
			if err == nil {
				if copies == 1 {
					return nil
				}
				conflictedVersion = ""
				continue
			}
			if !apierrors.IsConflict(err) {
				// Definite API rejections did not commit. Transport/server failures can
				// have committed. Reuse the existing next-RV expectation and its timeout
				// policy instead of retaining a separate uncertain-write record.
				if !apierrors.IsForbidden(err) && !apierrors.IsUnauthorized(err) && !apierrors.IsInvalid(err) && !apierrors.IsBadRequest(err) && !apierrors.IsNotFound(err) {
					expectations.ResourceVersionExpectationExpect(&metav1.ObjectMeta{
						UID:             box.UID,
						ResourceVersion: expectations.GetNewerResourceVersion(box),
					})
				}
				return fmt.Errorf("%w: close member binding: %v", session.ErrServingUnavailable, err)
			}
			conflictedVersion = box.ResourceVersion
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("%w: waiting for fresh member after conflict", session.ErrServingUnavailable)
		case <-tick.C:
		}
	}
}

var _ session.ServingSessionBackend = (*Store)(nil)
