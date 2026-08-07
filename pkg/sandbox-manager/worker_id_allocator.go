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
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	netutils "k8s.io/apimachinery/pkg/util/net"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	workerIDBits        = 20
	workerIDLimit       = 1 << workerIDBits
	workerLeaseNameBase = "sandbox-manager-sandbox-id-worker-"
)

func (m *SandboxManager) allocateWorkerID(ctx context.Context, prefix string) (uint32, error) {
	provider := m.infra.GetCache()
	if provider == nil {
		return 0, fmt.Errorf("sandbox ID worker allocator is not configured")
	}
	return allocateLeaseWorkerID(
		ctx,
		provider.GetClient(),
		provider.GetAPIReader(),
		m.systemNamespace,
		uuid.NewString(),
		prefix,
	)
}

func workerLeaseName(prefix string) string {
	sum := sha256.Sum256([]byte(prefix))
	return workerLeaseNameBase + hex.EncodeToString(sum[:12])
}

func allocateLeaseWorkerID(
	ctx context.Context,
	writer client.Client,
	reader client.Reader,
	namespace string,
	holderIdentity string,
	prefix string,
) (uint32, error) {
	if writer == nil || reader == nil {
		return 0, fmt.Errorf("sandbox ID worker allocator is not configured")
	}
	key := client.ObjectKey{Namespace: namespace, Name: workerLeaseName(prefix)}
	lease, err := getOrCreateWorkerLease(ctx, writer, reader, key, holderIdentity)
	if err != nil {
		return 0, err
	}

	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}

		current, owned, err := validateWorkerLease(lease, holderIdentity)
		if err != nil {
			return 0, fmt.Errorf("invalid sandbox ID worker Lease %s: %w", key, err)
		}
		if owned {
			return current, nil
		}
		if current == workerIDLimit-1 {
			// one prefix intentionally supports only 2^20 process
			// incarnations. Stop all managers and switch to a never-used prefix;
			// widening the worker field requires a new ID-format version.
			return 0, fmt.Errorf("sandbox ID worker domain for prefix %q is exhausted at %d", prefix, workerIDLimit)
		}

		next := current + 1
		counter := int32(next)
		lease.Spec.HolderIdentity = &holderIdentity
		lease.Spec.LeaseTransitions = &counter
		updateErr := writer.Update(ctx, lease)
		if updateErr == nil {
			return next, nil
		}
		if !apierrors.IsConflict(updateErr) && !isAmbiguousLeaseWrite(updateErr) {
			return 0, fmt.Errorf("update sandbox ID worker Lease %s: %w", key, updateErr)
		}
		lease, err = getWorkerLease(ctx, reader, key)
		if err != nil {
			return 0, fmt.Errorf("confirm sandbox ID worker Lease %s after update error %v: %w", key, updateErr, err)
		}
	}
}

func getOrCreateWorkerLease(
	ctx context.Context,
	writer client.Client,
	reader client.Reader,
	key client.ObjectKey,
	holderIdentity string,
) (*coordinationv1.Lease, error) {
	lease, err := getWorkerLease(ctx, reader, key)
	if err == nil {
		return lease, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("read sandbox ID worker Lease %s: %w", key, err)
	}

	created := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:   &holderIdentity,
			LeaseTransitions: new(int32),
		},
	}
	createErr := writer.Create(ctx, created)
	if createErr == nil {
		return created, nil
	}
	if !apierrors.IsAlreadyExists(createErr) && !isAmbiguousLeaseWrite(createErr) {
		return nil, fmt.Errorf("create sandbox ID worker Lease %s: %w", key, createErr)
	}
	lease, err = getWorkerLease(ctx, reader, key)
	if err != nil {
		return nil, fmt.Errorf("confirm sandbox ID worker Lease %s after create error %v: %w", key, createErr, err)
	}
	return lease, nil
}

func getWorkerLease(
	ctx context.Context,
	reader client.Reader,
	key client.ObjectKey,
) (*coordinationv1.Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lease := &coordinationv1.Lease{}
	if err := reader.Get(ctx, key, lease); err != nil {
		return nil, err
	}
	return lease, nil
}

func validateWorkerLease(lease *coordinationv1.Lease, holderIdentity string) (uint32, bool, error) {
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		return 0, false, fmt.Errorf("holderIdentity is missing")
	}
	if lease.Spec.LeaseTransitions == nil {
		return 0, false, fmt.Errorf("leaseTransitions is missing")
	}
	counter := *lease.Spec.LeaseTransitions
	if counter < 0 {
		return 0, false, fmt.Errorf("leaseTransitions %d is negative", counter)
	}
	if counter >= workerIDLimit {
		return 0, false, fmt.Errorf("leaseTransitions %d reached the %d-worker limit, please use a different id prefix", counter, workerIDLimit)
	}
	return uint32(counter), *lease.Spec.HolderIdentity == holderIdentity, nil
}

func isAmbiguousLeaseWrite(err error) bool {
	return apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || netutils.IsTimeout(err) ||
		netutils.IsProbableEOF(err) || netutils.IsConnectionReset(err) || netutils.IsHTTP2ConnectionLost(err)
}
