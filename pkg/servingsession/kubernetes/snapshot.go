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
	session "github.com/openkruise/agents/pkg/servingsession"
	sessionbinding "github.com/openkruise/agents/pkg/servingsession/binding"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

func (s *Store) snapshot(ctx context.Context, serving session.Serving) (session.ServingSnapshot, []*agents.Sandbox, error) {
	if err := s.ready(ctx); err != nil {
		return session.ServingSnapshot{}, nil, err
	}
	var parent agents.SandboxServing
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: serving.Namespace, Name: serving.Name}, &parent); err != nil {
		if apierrors.IsNotFound(err) {
			err = session.ErrServingNotFound
		}
		return session.ServingSnapshot{}, nil, err
	}
	current := servingValue(&parent)
	if current.UID != serving.UID || current.ID != serving.ID {
		return session.ServingSnapshot{}, nil, session.ErrServingNotFound
	}
	members, err := s.membersForParent(ctx, &parent)
	if err != nil {
		return session.ServingSnapshot{}, nil, err
	}
	snapshot := session.ServingSnapshot{Serving: current, Members: make([]session.ServingMember, 0, len(members))}
	for _, box := range members {
		bindings, err := sessionbinding.Decode(box.Annotations)
		if err != nil {
			return session.ServingSnapshot{}, nil, fmt.Errorf("%w: member %s: %v", session.ErrServingDataAmbiguous, box.Name, err)
		}
		route, err := sandboxroute.RouteFromSandbox(box)
		if err != nil {
			return session.ServingSnapshot{}, nil, fmt.Errorf("%w: member route: %v", session.ErrServingUnavailable, err)
		}
		ready := false
		for _, c := range box.Status.Conditions {
			if c.Type == string(agents.SandboxConditionReady) {
				ready = c.Status == metav1.ConditionTrue
			}
		}
		snapshot.Members = append(snapshot.Members, session.ServingMember{UID: string(box.UID), Version: box.ResourceVersion, Phase: string(box.Status.Phase), Ready: ready, Deleting: !box.DeletionTimestamp.IsZero(), Route: route, Bindings: bindings})
	}
	return snapshot, members, nil
}

func (s *Store) ReadServingSnapshot(ctx context.Context, serving session.Serving) (session.ServingSnapshot, error) {
	if err := s.lockSessions(ctx); err != nil {
		return session.ServingSnapshot{}, err
	}
	defer s.unlockSessions()
	snapshot, _, err := s.snapshot(ctx, serving)
	return snapshot, err
}

func (s *Store) ReplaceServingBindings(ctx context.Context, observed session.ServingSnapshot, target session.ServingMember, bindings sessionbinding.Bindings) (string, error) {
	if err := s.lockSessions(ctx); err != nil {
		return "", err
	}
	defer s.unlockSessions()
	current, boxes, err := s.snapshot(ctx, observed.Serving)
	if err != nil {
		return "", err
	}
	if current.Serving.Version != observed.Serving.Version || len(current.Members) != len(observed.Members) {
		return "", session.ErrServingConflict
	}
	versions := make(map[string]string, len(observed.Members))
	for _, m := range observed.Members {
		versions[m.UID] = m.Version
	}
	var box *agents.Sandbox
	for i, m := range current.Members {
		if versions[m.UID] != m.Version {
			return "", session.ErrServingConflict
		}
		if m.UID == target.UID && m.Version == target.Version {
			box = boxes[i]
		}
	}
	if box == nil {
		return "", session.ErrServingConflict
	}
	annotations, err := sessionbinding.Encode(box.Annotations, bindings)
	if err != nil {
		return "", fmt.Errorf("%w: %v", session.ErrServingCapacity, err)
	}
	version, err := s.patchMemberBindings(ctx, current.Serving.UID, box, annotations[sessionbinding.BindingsAnnotation])
	if err == nil {
		return version, nil
	}
	if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return "", fmt.Errorf("%w: %v", session.ErrServingConflict, err)
	}
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
		return "", fmt.Errorf("%w: binding write rejected: %v", session.ErrServingUnavailable, err)
	}
	expectations.ResourceVersionExpectationExpect(&metav1.ObjectMeta{UID: box.UID, ResourceVersion: expectations.GetNewerResourceVersion(box)})
	return "", fmt.Errorf("%w: %v", session.ErrServingWriteUnknown, err)
}

// patchMemberBindings changes only the binding annotation and records the ACK.
// The caller holds sessionLock and owns validation and error classification.
func (s *Store) patchMemberBindings(ctx context.Context, servingUID string, box *agents.Sandbox, bindings string) (string, error) {
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
	s.memberWrites[updated.UID] = servingMemberWrite{servingUID: servingUID, version: updated.ResourceVersion}
	return updated.ResourceVersion, nil
}

var _ session.Store = (*Store)(nil)
