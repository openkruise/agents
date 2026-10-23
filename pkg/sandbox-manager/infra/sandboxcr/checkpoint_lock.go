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

package sandboxcr

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandboxid"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

// checkpointLockAnnotationPath is the JSON pointer of AnnotationCheckpointLock.
var checkpointLockAnnotationPath = "/metadata/annotations/" + strings.ReplaceAll(agentsv1alpha1.AnnotationCheckpointLock, "/", "~1")

func encodeCheckpointLock(lock agentsv1alpha1.CheckpointLock) (string, error) {
	value, err := json.Marshal(lock)
	if err != nil {
		return "", fmt.Errorf("encode checkpoint lock: %w", err)
	}
	return string(value), nil
}

// activeCheckpointLock reports the unexpired lock recorded on sbx; an absent or
// unparsable value is free.
func activeCheckpointLock(sbx *agentsv1alpha1.Sandbox, now time.Time) (agentsv1alpha1.CheckpointLock, bool) {
	value := sbx.Annotations[agentsv1alpha1.AnnotationCheckpointLock]
	if value == "" {
		return agentsv1alpha1.CheckpointLock{}, false
	}
	var lock agentsv1alpha1.CheckpointLock
	if err := json.Unmarshal([]byte(value), &lock); err != nil {
		return agentsv1alpha1.CheckpointLock{}, false
	}
	return lock, lock.Holder != "" && now.Before(lock.ExpireTime.Time)
}

func (s *Sandbox) AcquireCheckpointLock(ctx context.Context, opts infra.CheckpointLockOptions) (agentsv1alpha1.CheckpointLock, error) {
	key := client.ObjectKeyFromObject(s.Sandbox)
	var acquired agentsv1alpha1.CheckpointLock
	// The informer view is the only read: a stale view either reports a lock
	// that already ended or fails the resourceVersion precondition and retries.
	err := retry.OnError(retry.DefaultBackoff, apierrors.IsConflict, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		latest := &agentsv1alpha1.Sandbox{}
		if err := s.Cache.GetClient().Get(ctx, key, latest); err != nil {
			return err
		}
		// The informer view may lag behind this process's own release; never
		// judge the lock on an object older than the wrapper.
		if expectations.IsResourceVersionReallyNewer(latest.ResourceVersion, s.Sandbox.ResourceVersion) {
			latest = s.Sandbox.DeepCopy()
		}
		now := time.Now()
		if held, active := activeCheckpointLock(latest, now); active {
			return managererrors.NewError(managererrors.ErrorConflict, "sandbox %s has a %s checkpoint in progress until %s",
				sandboxid.Resolve(latest), held.Operation, held.ExpireTime.UTC().Format(time.RFC3339))
		}
		// Second precision keeps the value identical after the JSON round trip
		// that release compares against.
		lock := agentsv1alpha1.CheckpointLock{
			Holder:     uuid.NewString(),
			Operation:  opts.Operation,
			ExpireTime: metav1.NewTime(now.Add(opts.TTL).UTC().Truncate(time.Second)),
		}
		value, err := encodeCheckpointLock(lock)
		if err != nil {
			return err
		}
		locked := latest.DeepCopy()
		if locked.Annotations == nil {
			locked.Annotations = map[string]string{}
		}
		locked.Annotations[agentsv1alpha1.AnnotationCheckpointLock] = value
		if err := s.Cache.GetClient().Patch(ctx, locked, client.MergeFromWithOptions(latest, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		s.Sandbox = locked
		expectations.ResourceVersionExpectationExpect(locked)
		acquired = lock
		return nil
	})
	if apierrors.IsConflict(err) {
		return agentsv1alpha1.CheckpointLock{}, managererrors.WrapError(managererrors.ErrorInternal, err, "acquire checkpoint lock for sandbox %s", s.GetSandboxID())
	}
	if err != nil {
		return agentsv1alpha1.CheckpointLock{}, err
	}
	return acquired, nil
}

// ReleaseCheckpointLock removes lock only while the Sandbox still records it.
// A lock that expired and was replaced is left to its new holder.
func (s *Sandbox) ReleaseCheckpointLock(ctx context.Context, lock agentsv1alpha1.CheckpointLock) error {
	value, err := encodeCheckpointLock(lock)
	if err != nil {
		return err
	}
	patch, err := json.Marshal([]map[string]string{
		{"op": "test", "path": checkpointLockAnnotationPath, "value": value},
		{"op": "remove", "path": checkpointLockAnnotationPath},
	})
	if err != nil {
		return fmt.Errorf("encode checkpoint lock release patch: %w", err)
	}
	released := s.Sandbox.DeepCopy()
	patchErr := s.Cache.GetClient().Patch(ctx, released, client.RawPatch(types.JSONPatchType, patch))
	if patchErr == nil {
		s.Sandbox = released
		expectations.ResourceVersionExpectationExpect(released)
		return nil
	}
	if apierrors.IsNotFound(patchErr) {
		return nil
	}
	// A failed test operation means the lock is no longer ours; the informer
	// view distinguishes that from a write failure without an API-server read.
	current := &agentsv1alpha1.Sandbox{}
	if err := s.Cache.GetClient().Get(ctx, client.ObjectKeyFromObject(s.Sandbox), current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("release checkpoint lock for sandbox %s: %w", s.GetSandboxID(), patchErr)
	}
	// The informer view may not have observed this process's own acquisition yet.
	if expectations.IsResourceVersionReallyNewer(current.ResourceVersion, s.Sandbox.ResourceVersion) {
		current = s.Sandbox
	}
	if current.Annotations[agentsv1alpha1.AnnotationCheckpointLock] != value {
		return nil
	}
	return fmt.Errorf("release checkpoint lock for sandbox %s: %w", s.GetSandboxID(), patchErr)
}
