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

package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agents "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandboxroute"
	session "github.com/openkruise/agents/pkg/sessionservice"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

func (s *Store) snapshot(ctx context.Context, serving session.SessionService) (session.SessionServiceSnapshot, []*agents.Sandbox, error) {
	if err := s.ready(ctx); err != nil {
		return session.SessionServiceSnapshot{}, nil, err
	}
	var parent agents.SessionService
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: serving.Namespace, Name: serving.Name}, &parent); err != nil {
		if apierrors.IsNotFound(err) {
			err = session.ErrSessionServiceNotFound
		}
		return session.SessionServiceSnapshot{}, nil, err
	}
	current := servingValue(&parent)
	if current.UID != serving.UID || current.ID != serving.ID {
		return session.SessionServiceSnapshot{}, nil, session.ErrSessionServiceNotFound
	}
	members, err := s.membersForParent(ctx, &parent)
	if err != nil {
		return session.SessionServiceSnapshot{}, nil, err
	}
	snapshot := session.SessionServiceSnapshot{SessionService: current, Members: make([]session.SessionServiceMember, 0, len(members))}
	for _, box := range members {
		bindings, err := sessionbinding.Decode(box.Annotations)
		if err != nil {
			return session.SessionServiceSnapshot{}, nil, fmt.Errorf("%w: member %s: %v", session.ErrSessionServiceDataAmbiguous, box.Name, err)
		}
		route, err := sandboxroute.RouteFromSandbox(box)
		if err != nil {
			return session.SessionServiceSnapshot{}, nil, fmt.Errorf("%w: member route: %v", session.ErrSessionServiceUnavailable, err)
		}
		ready := false
		for _, c := range box.Status.Conditions {
			if c.Type == string(agents.SandboxConditionReady) {
				ready = c.Status == metav1.ConditionTrue
			}
		}
		snapshot.Members = append(snapshot.Members, session.SessionServiceMember{UID: string(box.UID), Version: box.ResourceVersion, Phase: string(box.Status.Phase), Ready: ready, Deleting: !box.DeletionTimestamp.IsZero(), Route: route, Bindings: bindings})
	}
	return snapshot, members, nil
}

func (s *Store) ReadSessionServiceSnapshot(ctx context.Context, serving session.SessionService) (session.SessionServiceSnapshot, error) {
	if err := s.lockSessions(ctx); err != nil {
		return session.SessionServiceSnapshot{}, err
	}
	defer s.unlockSessions()
	snapshot, _, err := s.snapshot(ctx, serving)
	return snapshot, err
}

func (s *Store) ReplaceSessionServiceBindings(ctx context.Context, observed session.SessionServiceSnapshot, target session.SessionServiceMember, bindings sessionbinding.Bindings) (string, error) {
	if err := s.lockSessions(ctx); err != nil {
		return "", err
	}
	defer s.unlockSessions()
	current, boxes, err := s.snapshot(ctx, observed.SessionService)
	if err != nil {
		return "", err
	}
	if current.SessionService.Version != observed.SessionService.Version || len(current.Members) != len(observed.Members) {
		return "", session.ErrSessionServiceConflict
	}
	versions := make(map[string]string, len(observed.Members))
	for _, m := range observed.Members {
		versions[m.UID] = m.Version
	}
	var box *agents.Sandbox
	for i, m := range current.Members {
		if versions[m.UID] != m.Version {
			return "", session.ErrSessionServiceConflict
		}
		if m.UID == target.UID && m.Version == target.Version {
			box = boxes[i]
		}
	}
	if box == nil {
		return "", session.ErrSessionServiceConflict
	}
	annotations, err := sessionbinding.Encode(box.Annotations, bindings)
	if err != nil {
		return "", fmt.Errorf("%w: %v", session.ErrSessionServiceCapacity, err)
	}
	version, err := s.patchMemberBindings(ctx, current.SessionService.UID, box, annotations[sessionbinding.BindingsAnnotation])
	if err == nil {
		return version, nil
	}
	if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return "", fmt.Errorf("%w: %v", session.ErrSessionServiceConflict, err)
	}
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
		return "", fmt.Errorf("binding write rejected: %w", err)
	}
	expectations.ResourceVersionExpectationExpect(&metav1.ObjectMeta{UID: box.UID, ResourceVersion: expectations.GetNewerResourceVersion(box)})
	return "", fmt.Errorf("%w: %v", session.ErrSessionServiceWriteUnknown, err)
}

// patchMemberBindings changes only the binding annotation and records the ACK.
// The caller holds sessionLock and owns validation and error classification.
func (s *Store) patchMemberBindings(ctx context.Context, sessionServiceUID string, box *agents.Sandbox, bindings string) (string, error) {
	raw, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"uid":             box.UID,
			"resourceVersion": box.ResourceVersion,
			"annotations":     map[string]string{sessionbinding.BindingsAnnotation: bindings},
		},
	})
	if err != nil {
		return "", err
	}
	updated := box.DeepCopy()
	if err := s.client.Patch(ctx, updated, client.RawPatch(types.MergePatchType, raw)); err != nil {
		return "", err
	}
	expectations.ResourceVersionExpectationExpect(updated)
	if s.memberWrites == nil {
		s.memberWrites = make(map[types.UID]servingMemberWrite)
	}
	s.memberWrites[updated.UID] = servingMemberWrite{sessionServiceUID: sessionServiceUID, version: updated.ResourceVersion}
	return updated.ResourceVersion, nil
}

var _ session.Store = (*Store)(nil)
