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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	cachepkg "github.com/openkruise/agents/pkg/cache"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
)

func newForkLeaseTestSandbox(uid types.UID) infra.Sandbox {
	return sandboxcr.AsSandbox(&v1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default",
		Name:      "source",
		UID:       uid,
	}}, nil)
}

type forkLeaseClientCache struct {
	cachepkg.Provider
	writer ctrlclient.Client
	reader ctrlclient.Reader
}

func (c *forkLeaseClientCache) GetClient() ctrlclient.Client {
	if c.writer != nil {
		return c.writer
	}
	return c.Provider.GetClient()
}

func (c *forkLeaseClientCache) GetAPIReader() ctrlclient.Reader {
	if c.reader != nil {
		return c.reader
	}
	return c.Provider.GetAPIReader()
}

func overrideForkLeaseClients(t *testing.T, manager *SandboxManager, writer ctrlclient.Client, reader ctrlclient.Reader) {
	t.Helper()
	infraInstance := manager.infra.(*sandboxcr.Infra)
	infraInstance.Cache = &forkLeaseClientCache{Provider: infraInstance.Cache, writer: writer, reader: reader}
}

func TestForkLeaseKeyAndActivity(t *testing.T) {
	_, ok := forkLeaseKey(newForkLeaseTestSandbox(""))
	assert.False(t, ok)

	key, ok := forkLeaseKey(newForkLeaseTestSandbox("source-uid"))
	require.True(t, ok)
	assert.Equal(t, "default", key.Namespace)
	assert.Equal(t, "sandbox-fork-source-uid", key.Name)

	now := time.Now()
	lease := &coordinationv1.Lease{}
	assert.False(t, leaseIsActive(lease, now))
	lease.Spec.HolderIdentity = ptr.To("fork:test")
	lease.Spec.LeaseDurationSeconds = ptr.To(int32(10))
	assert.False(t, leaseIsActive(lease, now))
	lease.Spec.RenewTime = &metav1.MicroTime{Time: now.Add(-time.Second)}
	assert.True(t, leaseIsActive(lease, now))
	lease.Spec.RenewTime = &metav1.MicroTime{Time: now.Add(-11 * time.Second)}
	assert.False(t, leaseIsActive(lease, now))
}

func TestAcquireAndReleaseForkLease(t *testing.T) {
	manager, client := setupTestManager(t)
	source := newForkLeaseTestSandbox("source-uid")

	held, err := manager.acquireForkLease(t.Context(), source)
	require.NoError(t, err)
	require.NotNil(t, held)
	assert.Contains(t, ptr.Deref(held.Spec.HolderIdentity, ""), forkLeaseHolderPrefix)
	assert.Equal(t, int32(forkLeaseDuration/time.Second), ptr.Deref(held.Spec.LeaseDurationSeconds, 0))
	require.Len(t, held.OwnerReferences, 1)
	assert.Equal(t, source.GetUID(), held.OwnerReferences[0].UID)

	_, err = manager.acquireForkLease(t.Context(), source)
	require.Error(t, err)
	assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err))

	require.NoError(t, manager.releaseForkLease(t.Context(), held))
	key, _ := forkLeaseKey(source)
	stored := &coordinationv1.Lease{}
	require.NoError(t, client.Get(t.Context(), key, stored))
	assert.Nil(t, stored.Spec.HolderIdentity)
	assert.Nil(t, stored.Spec.RenewTime)
}

func TestAcquireForkLeaseClientFailures(t *testing.T) {
	source := newForkLeaseTestSandbox("source-uid")

	t.Run("missing cache", func(t *testing.T) {
		manager := &SandboxManager{infra: &sandboxcr.Infra{}}
		_, err := manager.acquireForkLease(t.Context(), source)
		require.Error(t, err)
		assert.Equal(t, managererrors.ErrorInternal, managererrors.GetErrCode(err))
	})

	t.Run("reader failure", func(t *testing.T) {
		manager, base := setupTestManager(t)
		readerErr := errors.New("reader failed")
		reader := interceptor.NewClient(base.(ctrlclient.WithWatch), interceptor.Funcs{
			Get: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectKey, ctrlclient.Object, ...ctrlclient.GetOption) error {
				return readerErr
			},
		})
		overrideForkLeaseClients(t, manager, nil, reader)

		_, err := manager.acquireForkLease(t.Context(), source)
		require.Error(t, err)
		assert.Equal(t, managererrors.ErrorInternal, managererrors.GetErrCode(err))
	})

	t.Run("create failure", func(t *testing.T) {
		manager, base := setupTestManager(t)
		createErr := errors.New("create failed")
		writer := interceptor.NewClient(base.(ctrlclient.WithWatch), interceptor.Funcs{
			Create: func(context.Context, ctrlclient.WithWatch, ctrlclient.Object, ...ctrlclient.CreateOption) error {
				return createErr
			},
		})
		overrideForkLeaseClients(t, manager, writer, nil)

		_, err := manager.acquireForkLease(t.Context(), source)
		require.Error(t, err)
		assert.Equal(t, managererrors.ErrorInternal, managererrors.GetErrCode(err))
	})

	t.Run("expired lease update conflict", func(t *testing.T) {
		manager, base := setupTestManager(t)
		key, _ := forkLeaseKey(source)
		duration := int32(forkLeaseDuration / time.Second)
		require.NoError(t, base.Create(t.Context(), &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       ptr.To("fork:expired"),
				LeaseDurationSeconds: &duration,
				RenewTime:            &metav1.MicroTime{Time: time.Now().Add(-forkLeaseDuration)},
			},
		}))
		writer := interceptor.NewClient(base.(ctrlclient.WithWatch), interceptor.Funcs{
			Update: func(context.Context, ctrlclient.WithWatch, ctrlclient.Object, ...ctrlclient.UpdateOption) error {
				return apierrors.NewConflict(schema.GroupResource{Group: coordinationv1.GroupName, Resource: "leases"}, key.Name, errors.New("changed"))
			},
		})
		overrideForkLeaseClients(t, manager, writer, nil)

		_, err := manager.acquireForkLease(t.Context(), source)
		require.Error(t, err)
		assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err))
	})
}

func TestReleaseForkLeaseDoesNotReleaseNewHolder(t *testing.T) {
	manager, client := setupTestManager(t)
	source := newForkLeaseTestSandbox("source-uid")
	held, err := manager.acquireForkLease(t.Context(), source)
	require.NoError(t, err)

	key, _ := forkLeaseKey(source)
	stored := &coordinationv1.Lease{}
	require.NoError(t, client.Get(t.Context(), key, stored))
	stored.Spec.HolderIdentity = ptr.To("fork:replacement")
	require.NoError(t, client.Update(t.Context(), stored))

	require.NoError(t, manager.releaseForkLease(t.Context(), held))
	require.NoError(t, client.Get(t.Context(), key, stored))
	assert.Equal(t, "fork:replacement", ptr.Deref(stored.Spec.HolderIdentity, ""))
}

func TestForkLeaseRenewalAndReleaseClientFailures(t *testing.T) {
	source := newForkLeaseTestSandbox("source-uid")

	t.Run("renewal update failure", func(t *testing.T) {
		manager, base := setupTestManager(t)
		held, err := manager.acquireForkLease(t.Context(), source)
		require.NoError(t, err)
		writer := interceptor.NewClient(base.(ctrlclient.WithWatch), interceptor.Funcs{
			Update: func(context.Context, ctrlclient.WithWatch, ctrlclient.Object, ...ctrlclient.UpdateOption) error {
				return errors.New("renew update failed")
			},
		})
		overrideForkLeaseClients(t, manager, writer, nil)

		require.ErrorContains(t, manager.renewForkLease(t.Context(), held), "renew fork lease")
	})

	t.Run("release reader failure", func(t *testing.T) {
		manager, base := setupTestManager(t)
		held, err := manager.acquireForkLease(t.Context(), source)
		require.NoError(t, err)
		reader := interceptor.NewClient(base.(ctrlclient.WithWatch), interceptor.Funcs{
			Get: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectKey, ctrlclient.Object, ...ctrlclient.GetOption) error {
				return errors.New("release read failed")
			},
		})
		overrideForkLeaseClients(t, manager, nil, reader)

		require.ErrorContains(t, manager.releaseForkLease(t.Context(), held), "get fork lease")
	})

	t.Run("release update conflict is ignored", func(t *testing.T) {
		manager, base := setupTestManager(t)
		held, err := manager.acquireForkLease(t.Context(), source)
		require.NoError(t, err)
		writer := interceptor.NewClient(base.(ctrlclient.WithWatch), interceptor.Funcs{
			Update: func(_ context.Context, _ ctrlclient.WithWatch, obj ctrlclient.Object, _ ...ctrlclient.UpdateOption) error {
				return apierrors.NewConflict(schema.GroupResource{Group: coordinationv1.GroupName, Resource: "leases"}, obj.GetName(), errors.New("changed"))
			},
		})
		overrideForkLeaseClients(t, manager, writer, nil)

		require.NoError(t, manager.releaseForkLease(t.Context(), held))
	})

	t.Run("release update failure", func(t *testing.T) {
		manager, base := setupTestManager(t)
		held, err := manager.acquireForkLease(t.Context(), source)
		require.NoError(t, err)
		writer := interceptor.NewClient(base.(ctrlclient.WithWatch), interceptor.Funcs{
			Update: func(context.Context, ctrlclient.WithWatch, ctrlclient.Object, ...ctrlclient.UpdateOption) error {
				return errors.New("release update failed")
			},
		})
		overrideForkLeaseClients(t, manager, writer, nil)

		require.ErrorContains(t, manager.releaseForkLease(t.Context(), held), "release fork lease")
	})
}

func TestForkLeaseLifecycleEdges(t *testing.T) {
	manager, client := setupTestManager(t)
	noUIDLease, err := manager.acquireForkLease(t.Context(), newForkLeaseTestSandbox(""))
	require.NoError(t, err)
	assert.Nil(t, noUIDLease)

	source := newForkLeaseTestSandbox("source-uid")
	key, _ := forkLeaseKey(source)
	expiredHolder := "fork:expired"
	duration := int32(forkLeaseDuration / time.Second)
	expired := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &expiredHolder,
			LeaseDurationSeconds: &duration,
			RenewTime:            &metav1.MicroTime{Time: time.Now().Add(-forkLeaseDuration)},
		},
	}
	require.NoError(t, client.Create(t.Context(), expired))

	lease, err := manager.acquireForkLease(t.Context(), source)
	require.NoError(t, err)
	require.NotNil(t, lease)
	assert.NotEqual(t, expiredHolder, ptr.Deref(lease.Spec.HolderIdentity, ""))
	require.Len(t, lease.OwnerReferences, 1)

	require.NoError(t, manager.renewForkLease(t.Context(), lease))
	stored := &coordinationv1.Lease{}
	require.NoError(t, client.Get(t.Context(), key, stored))
	stored.Spec.HolderIdentity = ptr.To("fork:replacement")
	require.NoError(t, client.Update(t.Context(), stored))
	require.ErrorContains(t, manager.renewForkLease(t.Context(), lease), "acquired by another operation")

	require.NoError(t, client.Delete(t.Context(), stored))
	require.NoError(t, manager.releaseForkLease(t.Context(), lease))

	called := false
	require.NoError(t, manager.withLease(t.Context(), nil, func(context.Context) error {
		called = true
		return nil
	}))
	assert.True(t, called)
}

func TestSandboxManagerCreateCheckpointDelegates(t *testing.T) {
	manager := &SandboxManager{}
	source := newForkOrchestrationSandbox("source", "source-uid")
	source.checkpointID = "checkpoint-id"

	checkpointID, err := manager.CreateCheckpoint(t.Context(), source, infra.CreateCheckpointOptions{Fork: true})

	require.NoError(t, err)
	assert.Equal(t, "checkpoint-id", checkpointID)
	require.Len(t, source.checkpointOps, 1)
}

func TestWithLeaseReleasesAfterOperationError(t *testing.T) {
	manager, client := setupTestManager(t)
	source := newForkLeaseTestSandbox("source-uid")
	lease, err := manager.acquireForkLease(t.Context(), source)
	require.NoError(t, err)
	opErr := errors.New("operation failed")

	err = manager.withLease(t.Context(), lease, func(context.Context) error {
		return opErr
	})
	require.ErrorIs(t, err, opErr)

	key, _ := forkLeaseKey(source)
	stored := &coordinationv1.Lease{}
	require.NoError(t, client.Get(t.Context(), key, stored))
	assert.Nil(t, stored.Spec.HolderIdentity)
}
