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
	"math"

	"github.com/google/uuid"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	netutils "k8s.io/apimachinery/pkg/util/net"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/pkg/sandboxid"
)

const (
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

// Manager instances have no stable unique ordinal, so random or hash-based
// Sonyflake worker IDs cannot provide coordinated allocation. This per-prefix
// Lease is a persistent allocation ledger, not leader election: it is not
// renewed, used for liveness detection, or reset.
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

		generation, owned, err := validateWorkerLease(lease, holderIdentity)
		if err != nil {
			return 0, fmt.Errorf("invalid sandbox ID worker Lease %s: %w", key, err)
		}
		// known-limit: reuse requires the previous holder of this worker ID
		// to have exited and wall-clock time not to repeat; otherwise switch prefixes
		// before the generation wraps through sandboxid.WorkerIDLimit, or upgrade
		// to a fenced allocator.
		if owned {
			return generation % sandboxid.WorkerIDLimit, nil
		}
		if generation == math.MaxInt32 {
			return 0, fmt.Errorf("sandbox ID allocation generation for prefix %q is exhausted at %d, please use a different id prefix", prefix, generation)
		}

		nextGeneration := generation + 1
		counter := int32(nextGeneration)
		lease.Spec.HolderIdentity = &holderIdentity
		lease.Spec.LeaseTransitions = &counter
		updateErr := writer.Update(ctx, lease)
		if updateErr == nil {
			return nextGeneration % sandboxid.WorkerIDLimit, nil
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
	return uint32(counter), *lease.Spec.HolderIdentity == holderIdentity, nil
}

func isAmbiguousLeaseWrite(err error) bool {
	return apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || netutils.IsTimeout(err) ||
		netutils.IsProbableEOF(err) || netutils.IsConnectionReset(err) || netutils.IsHTTP2ConnectionLost(err)
}
