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
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

// ExtendShutdownTime validates against each observed version and patches only
// ShutdownTime. A concurrent renewal or pause deadline can never be overwritten
// using validation performed against an earlier request snapshot.
func (s *Sandbox) ExtendShutdownTime(ctx context.Context, deadline time.Time) error {
	deadline = timeout.NormalizeTime(deadline)
	key := client.ObjectKeyFromObject(s.Sandbox)
	var attemptErr error
	err := retry.RetryOnConflict(retry.DefaultRetry, func() (err error) {
		defer func() { attemptErr = err }()
		if err := ctx.Err(); err != nil {
			return err
		}
		latest := &agentsv1alpha1.Sandbox{}
		if err := s.Cache.GetClient().Get(ctx, key, latest); err != nil {
			return err
		}
		if latest.DeletionTimestamp != nil {
			return managererrors.NewError(managererrors.ErrorConflict, "sandbox is terminating")
		}
		current := timeout.GetTimeoutFromSandbox(latest)
		if !deadline.After(time.Now()) || (!current.ShutdownTime.IsZero() && !deadline.After(current.ShutdownTime)) || (!current.PauseTime.IsZero() && !deadline.After(current.PauseTime)) {
			return managererrors.NewError(managererrors.ErrorBadRequest, "expiresAt must be in the future and strictly extend the current expiration")
		}
		updated := latest.DeepCopy()
		updated.Spec.ShutdownTime = &metav1.Time{Time: deadline}
		if err := s.Cache.GetClient().Patch(ctx, updated, client.MergeFromWithOptions(latest, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		s.Sandbox = updated
		expectations.ResourceVersionExpectationExpect(updated)
		return nil
	})
	if err == nil && attemptErr != nil {
		err = attemptErr
	}
	if apierrors.IsConflict(err) {
		return managererrors.WrapError(managererrors.ErrorConflict, err, "sandbox changed while extending expiration")
	}
	if err != nil {
		return fmt.Errorf("extend sandbox expiration: %w", err)
	}
	return nil
}
