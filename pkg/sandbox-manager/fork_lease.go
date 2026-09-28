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

package sandbox_manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/google/uuid"
	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

const (
	forkLeaseDuration          = 90 * time.Second
	forkLeaseReleaseTimeout    = 10 * time.Second
	forkLeaseRenewInterval     = forkLeaseDuration / 3
	forkLeaseHolderPrefix      = "fork:"
	lifecycleLeaseHolderPrefix = "lifecycle:"
)

func forkLeaseKey(sbx infra.Sandbox) (client.ObjectKey, bool) {
	if sbx.GetUID() == "" {
		return client.ObjectKey{}, false
	}
	return client.ObjectKey{
		Namespace: sbx.GetNamespace(),
		Name:      "sandbox-fork-" + string(sbx.GetUID()),
	}, true
}

func leaseIsActive(lease *coordinationv1.Lease, now time.Time) bool {
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" ||
		lease.Spec.LeaseDurationSeconds == nil || lease.Spec.RenewTime == nil {
		return false
	}
	return now.Before(lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second))
}

func forkLeaseOwnerReference(sbx infra.Sandbox) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: v1alpha1.GroupVersion.String(),
		Kind:       "Sandbox",
		Name:       sbx.GetName(),
		UID:        sbx.GetUID(),
		Controller: ptr.To(true),
	}
}

func (m *SandboxManager) acquireLease(ctx context.Context, sbx infra.Sandbox, holderPrefix string, joinLifecycle bool) (*coordinationv1.Lease, bool, error) {
	key, ok := forkLeaseKey(sbx)
	if !ok {
		return nil, false, nil
	}
	provider := m.infra.GetCache()
	if provider == nil || provider.GetClient() == nil || provider.GetAPIReader() == nil {
		return nil, false, managererrors.NewError(managererrors.ErrorInternal, "fork lease client is not configured")
	}
	writer := provider.GetClient()
	reader := provider.GetAPIReader()
	now := metav1.NewMicroTime(time.Now())
	durationSeconds := int32(forkLeaseDuration / time.Second)
	holder := holderPrefix + uuid.NewString()
	lease := &coordinationv1.Lease{}
	// Lease acquisition must observe the API server directly: informer cache
	// staleness would permit concurrent managers to enter the same source operation.
	if err := reader.Get(ctx, key, lease); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, false, managererrors.WrapError(managererrors.ErrorInternal, err, "get fork lease for sandbox %s", sbx.GetSandboxID())
		}
		lease = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:       key.Namespace,
				Name:            key.Name,
				OwnerReferences: []metav1.OwnerReference{forkLeaseOwnerReference(sbx)},
			},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       ptr.To(holder),
				LeaseDurationSeconds: ptr.To(durationSeconds),
				AcquireTime:          &now,
				RenewTime:            &now,
			},
		}
		if err := writer.Create(ctx, lease); err == nil {
			return lease, true, nil
		} else if !apierrors.IsAlreadyExists(err) {
			return nil, false, managererrors.WrapError(managererrors.ErrorInternal, err, "create fork lease for sandbox %s", sbx.GetSandboxID())
		}
		if err := reader.Get(ctx, key, lease); err != nil {
			return nil, false, managererrors.WrapError(managererrors.ErrorInternal, err, "get competing fork lease for sandbox %s", sbx.GetSandboxID())
		}
	}

	if leaseIsActive(lease, now.Time) {
		if joinLifecycle && strings.HasPrefix(ptr.Deref(lease.Spec.HolderIdentity, ""), holderPrefix) {
			return nil, false, nil
		}
		return nil, false, managererrors.NewError(managererrors.ErrorConflict, "sandbox %s is busy with an exclusive operation", sbx.GetSandboxID())
	}
	if len(lease.OwnerReferences) == 0 {
		lease.OwnerReferences = []metav1.OwnerReference{forkLeaseOwnerReference(sbx)}
	}
	lease.Spec.HolderIdentity = ptr.To(holder)
	lease.Spec.LeaseDurationSeconds = ptr.To(durationSeconds)
	lease.Spec.AcquireTime = &now
	lease.Spec.RenewTime = &now
	if err := writer.Update(ctx, lease); err != nil {
		if apierrors.IsConflict(err) {
			return nil, false, managererrors.NewError(managererrors.ErrorConflict, "sandbox %s is busy with an exclusive operation", sbx.GetSandboxID())
		}
		return nil, false, managererrors.WrapError(managererrors.ErrorInternal, err, "take expired fork lease for sandbox %s", sbx.GetSandboxID())
	}
	return lease, true, nil
}

func (m *SandboxManager) acquireForkLease(ctx context.Context, sbx infra.Sandbox) (*coordinationv1.Lease, error) {
	lease, _, err := m.acquireLease(ctx, sbx, forkLeaseHolderPrefix, false)
	return lease, err
}

func (m *SandboxManager) acquireLifecycleLease(ctx context.Context, sbx infra.Sandbox, operation string) (*coordinationv1.Lease, bool, error) {
	return m.acquireLease(ctx, sbx, lifecycleLeaseHolderPrefix+operation+":", true)
}

func (m *SandboxManager) renewForkLease(ctx context.Context, held *coordinationv1.Lease) error {
	if held == nil {
		return nil
	}
	provider := m.infra.GetCache()
	if provider == nil || provider.GetClient() == nil || provider.GetAPIReader() == nil {
		return fmt.Errorf("fork lease client is not configured")
	}
	current := &coordinationv1.Lease{}
	if err := provider.GetAPIReader().Get(ctx, client.ObjectKeyFromObject(held), current); err != nil {
		return fmt.Errorf("get fork lease %s/%s for renewal: %w", held.Namespace, held.Name, err)
	}
	if ptr.Deref(current.Spec.HolderIdentity, "") != ptr.Deref(held.Spec.HolderIdentity, "") {
		return fmt.Errorf("fork lease %s/%s was acquired by another operation", held.Namespace, held.Name)
	}
	now := metav1.NewMicroTime(time.Now())
	current.Spec.RenewTime = &now
	if err := provider.GetClient().Update(ctx, current); err != nil {
		return fmt.Errorf("renew fork lease %s/%s: %w", held.Namespace, held.Name, err)
	}
	return nil
}

func (m *SandboxManager) renewForkLeaseUntilStopped(ctx context.Context, held *coordinationv1.Lease, cancelOperation context.CancelFunc) error {
	if held == nil {
		return nil
	}
	ticker := time.NewTicker(forkLeaseRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			renewCtx, cancel := context.WithTimeout(ctx, forkLeaseReleaseTimeout)
			err := m.renewForkLease(renewCtx, held)
			cancel()
			if err != nil {
				cancelOperation()
				return err
			}
		}
	}
}

func (m *SandboxManager) releaseForkLease(ctx context.Context, held *coordinationv1.Lease) error {
	if held == nil {
		return nil
	}
	provider := m.infra.GetCache()
	if provider == nil || provider.GetClient() == nil || provider.GetAPIReader() == nil {
		return fmt.Errorf("fork lease client is not configured")
	}
	current := &coordinationv1.Lease{}
	if err := provider.GetAPIReader().Get(ctx, client.ObjectKeyFromObject(held), current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get fork lease %s/%s for release: %w", held.Namespace, held.Name, err)
	}
	if ptr.Deref(current.Spec.HolderIdentity, "") != ptr.Deref(held.Spec.HolderIdentity, "") {
		return nil
	}
	current.Spec.HolderIdentity = nil
	current.Spec.RenewTime = nil
	if err := provider.GetClient().Update(ctx, current); err != nil {
		if apierrors.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("release fork lease %s/%s: %w", held.Namespace, held.Name, err)
	}
	return nil
}

func (m *SandboxManager) withLease(ctx context.Context, lease *coordinationv1.Lease, operation func(context.Context) error) (err error) {
	if lease == nil {
		return operation(ctx)
	}
	operationCtx, cancelOperation := context.WithCancel(ctx)
	renewCtx, stopRenewal := context.WithCancel(context.WithoutCancel(ctx))
	renewalDone := make(chan error, 1)
	go func() {
		renewalDone <- m.renewForkLeaseUntilStopped(renewCtx, lease, cancelOperation)
	}()

	err = operation(operationCtx)
	cancelOperation()
	stopRenewal()
	if renewalErr := <-renewalDone; renewalErr != nil {
		err = errors.Join(err, renewalErr)
	}

	releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(ctx), forkLeaseReleaseTimeout)
	defer cancelRelease()
	if releaseErr := m.releaseForkLease(releaseCtx, lease); releaseErr != nil {
		err = errors.Join(err, releaseErr)
	}
	return err
}

func (m *SandboxManager) withForkLease(ctx context.Context, sbx infra.Sandbox, operation func(context.Context) error) error {
	lease, err := m.acquireForkLease(ctx, sbx)
	if err != nil {
		return err
	}
	return m.withLease(ctx, lease, operation)
}

func (m *SandboxManager) withLifecycleLease(ctx context.Context, sbx infra.Sandbox, operationName string, operation func(context.Context) error) error {
	lease, acquired, err := m.acquireLifecycleLease(ctx, sbx, operationName)
	if err != nil {
		return err
	}
	if !acquired {
		return operation(ctx)
	}
	return m.withLease(ctx, lease, operation)
}

// CreateCheckpoint creates a checkpoint while excluding concurrent fork, pause,
// resume, delete, and snapshot operations for the same source sandbox.
func (m *SandboxManager) CreateCheckpoint(ctx context.Context, sbx infra.Sandbox, opts infra.CreateCheckpointOptions) (checkpointID string, err error) {
	err = m.withForkLease(ctx, sbx, func(ctx context.Context) error {
		checkpointID, err = sbx.CreateCheckpoint(ctx, opts)
		return err
	})
	return checkpointID, err
}
