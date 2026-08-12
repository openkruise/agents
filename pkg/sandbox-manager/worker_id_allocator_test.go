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
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openkruise/agents/pkg/sandboxid"
)

const testWorkerNamespace = "sandbox-system"

func newWorkerAllocatorTestClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func testWorkerLease(prefix, holder string, counter *int32) *coordinationv1.Lease {
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       testWorkerNamespace,
			Name:            workerLeaseName(prefix),
			ResourceVersion: "1",
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:   &holder,
			LeaseTransitions: counter,
		},
	}
}

func testAllocateWorkerID(ctx context.Context, c client.Client, reader client.Reader, holder, prefix string) (uint32, error) {
	return allocateLeaseWorkerID(ctx, c, reader, testWorkerNamespace, holder, prefix)
}

func TestLeaseWorkerIDAllocatorPrefixDomains(t *testing.T) {
	c := newWorkerAllocatorTestClient(t)

	workerID, err := testAllocateWorkerID(t.Context(), c, c, "manager-a", "prod-")
	require.NoError(t, err)
	assert.Equal(t, uint32(0), workerID)
	workerID, err = testAllocateWorkerID(t.Context(), c, c, "manager-a", "prod-")
	require.NoError(t, err)
	assert.Equal(t, uint32(0), workerID, "one process reuses its confirmed allocation")

	workerID, err = testAllocateWorkerID(t.Context(), c, c, "manager-b", "prod-")
	require.NoError(t, err)
	assert.Equal(t, uint32(1), workerID)
	workerID, err = testAllocateWorkerID(t.Context(), c, c, "manager-c", "recovery-")
	require.NoError(t, err)
	assert.Equal(t, uint32(0), workerID)

	assert.NotEqual(t, workerLeaseName("prod-"), workerLeaseName("recovery-"))
	assert.Len(t, workerLeaseName("prod-"), len(workerLeaseNameBase)+24)
	leases := &coordinationv1.LeaseList{}
	require.NoError(t, c.List(t.Context(), leases))
	require.Len(t, leases.Items, 2)
}

func TestLeaseWorkerIDAllocatorFailsClosedOnInvalidCounter(t *testing.T) {
	negative := int32(-1)
	maxGeneration := int32(math.MaxInt32)
	tests := []struct {
		name        string
		prefix      string
		lease       *coordinationv1.Lease
		expectID    uint32
		expectError string
	}{
		{name: "missing holder", prefix: "missing-holder", lease: testWorkerLease("missing-holder", "", new(int32)), expectError: "holderIdentity is missing"},
		{name: "missing counter", prefix: "missing-counter", lease: testWorkerLease("missing-counter", "other", nil), expectError: "leaseTransitions is missing"},
		{name: "negative counter", prefix: "negative", lease: testWorkerLease("negative", "other", &negative), expectError: "is negative"},
		{name: "maximum generation already owned", prefix: "owned-maximum", lease: testWorkerLease("owned-maximum", "manager-a", &maxGeneration), expectID: uint32(math.MaxInt32) % sandboxid.WorkerIDLimit},
		{name: "maximum generation cannot advance", prefix: "exhausted", lease: testWorkerLease("exhausted", "other", &maxGeneration), expectError: "allocation generation for prefix \"exhausted\" is exhausted at 2147483647"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newWorkerAllocatorTestClient(t, tt.lease)
			workerID, err := testAllocateWorkerID(t.Context(), c, c, "manager-a", tt.prefix)
			if tt.expectError == "" {
				require.NoError(t, err)
				assert.Equal(t, tt.expectID, workerID)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
		})
	}
}

func TestLeaseWorkerIDAllocatorWrapsWorkerIDWithoutResettingGeneration(t *testing.T) {
	generation := int32(sandboxid.WorkerIDLimit - 1)
	c := newWorkerAllocatorTestClient(t, testWorkerLease("prod-", "manager-a", &generation))

	workerID, err := testAllocateWorkerID(t.Context(), c, c, "manager-b", "prod-")
	require.NoError(t, err)
	assert.Equal(t, uint32(0), workerID)

	lease := &coordinationv1.Lease{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: testWorkerNamespace, Name: workerLeaseName("prod-")}, lease))
	require.NotNil(t, lease.Spec.LeaseTransitions)
	assert.Equal(t, int32(sandboxid.WorkerIDLimit), *lease.Spec.LeaseTransitions)
}

func TestLeaseWorkerIDAllocatorCreateAlreadyExistsRetriesFromLatest(t *testing.T) {
	counter := int32(4)
	lease := testWorkerLease("prod-", "manager-a", &counter)
	base := newWorkerAllocatorTestClient(t, lease)
	var gets atomic.Int32
	reader := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if gets.Add(1) == 1 {
				return apierrors.NewNotFound(coordinationv1.Resource("leases"), key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	workerID, err := testAllocateWorkerID(t.Context(), base, reader, "manager-b", "prod-")
	require.NoError(t, err)
	assert.Equal(t, uint32(5), workerID)
}

func TestLeaseWorkerIDAllocatorConfirmsAmbiguousWrites(t *testing.T) {
	serverTimeout := func(operation string) error {
		return apierrors.NewServerTimeout(schema.GroupResource{Group: coordinationv1.GroupName, Resource: "leases"}, operation, 1)
	}
	t.Run("create accepted for same holder", func(t *testing.T) {
		base := newWorkerAllocatorTestClient(t)
		writer := interceptor.NewClient(base, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				require.NoError(t, c.Create(ctx, obj, opts...))
				return serverTimeout("create")
			},
		})
		workerID, err := testAllocateWorkerID(t.Context(), writer, base, "manager-a", "prod-")
		require.NoError(t, err)
		assert.Equal(t, uint32(0), workerID)
	})

	t.Run("update accepted for same holder", func(t *testing.T) {
		counter := int32(7)
		base := newWorkerAllocatorTestClient(t, testWorkerLease("prod-", "manager-a", &counter))
		writer := interceptor.NewClient(base, interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				require.NoError(t, c.Update(ctx, obj, opts...))
				return serverTimeout("update")
			},
		})
		workerID, err := testAllocateWorkerID(t.Context(), writer, base, "manager-b", "prod-")
		require.NoError(t, err)
		assert.Equal(t, uint32(8), workerID)
	})

	t.Run("different holder advances from latest", func(t *testing.T) {
		counter := int32(7)
		base := newWorkerAllocatorTestClient(t, testWorkerLease("prod-", "manager-a", &counter))
		var updates atomic.Int32
		writer := interceptor.NewClient(base, interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if updates.Add(1) != 1 {
					return c.Update(ctx, obj, opts...)
				}
				require.NoError(t, c.Update(ctx, obj, opts...))
				latest := &coordinationv1.Lease{}
				require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), latest))
				otherHolder := "manager-c"
				otherCounter := int32(9)
				latest.Spec.HolderIdentity = &otherHolder
				latest.Spec.LeaseTransitions = &otherCounter
				require.NoError(t, c.Update(ctx, latest))
				return serverTimeout("update")
			},
		})
		workerID, err := testAllocateWorkerID(t.Context(), writer, base, "manager-b", "prod-")
		require.NoError(t, err)
		assert.Equal(t, uint32(10), workerID)
	})
}

func TestLeaseWorkerIDAllocatorConcurrentCAS(t *testing.T) {
	counter := int32(0)
	base := newWorkerAllocatorTestClient(t, testWorkerLease("prod-", "manager-a", &counter))
	var gets atomic.Int32
	var firstGets sync.WaitGroup
	firstGets.Add(2)
	releaseGets := make(chan struct{})
	reader := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)
			if gets.Add(1) <= 2 {
				firstGets.Done()
				<-releaseGets
			}
			return err
		},
	})

	results := make(chan uint32, 2)
	errs := make(chan error, 2)
	for _, holder := range []string{"manager-b", "manager-c"} {
		go func() {
			workerID, err := testAllocateWorkerID(t.Context(), base, reader, holder, "prod-")
			results <- workerID
			errs <- err
		}()
	}
	firstGets.Wait()
	close(releaseGets)
	ids := []uint32{<-results, <-results}
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	slices.Sort(ids)
	assert.Equal(t, []uint32{1, 2}, ids)
}

func TestLeaseWorkerIDAllocatorStopsOnCancellationAndConfirmationFailure(t *testing.T) {
	t.Run("canceled before read", func(t *testing.T) {
		base := newWorkerAllocatorTestClient(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := testAllocateWorkerID(ctx, base, base, "manager-a", "prod-")
		assert.ErrorIs(t, err, context.Canceled)
		leases := &coordinationv1.LeaseList{}
		require.NoError(t, base.List(t.Context(), leases))
		assert.Empty(t, leases.Items)
	})

	t.Run("confirmation read failure terminates", func(t *testing.T) {
		base := newWorkerAllocatorTestClient(t)
		confirmErr := errors.New("confirmation unavailable")
		var gets atomic.Int32
		reader := interceptor.NewClient(base, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if gets.Add(1) == 1 {
					return apierrors.NewNotFound(coordinationv1.Resource("leases"), key.Name)
				}
				return confirmErr
			},
		})
		writer := interceptor.NewClient(base, interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return apierrors.NewServerTimeout(schema.GroupResource{Group: coordinationv1.GroupName, Resource: "leases"}, "create", 1)
			},
		})
		_, err := testAllocateWorkerID(t.Context(), writer, reader, "manager-a", "prod-")
		require.Error(t, err)
		assert.ErrorIs(t, err, confirmErr)
	})
}
