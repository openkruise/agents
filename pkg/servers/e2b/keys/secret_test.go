package keys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

func newSecretStorageForTest(t *testing.T, data map[string][]byte) (*secretKeyStorage, *fake.Clientset) {
	t.Helper()
	client := fake.NewClientset()
	_, err := client.CoreV1().Secrets("default").Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: KeySecretName},
		Data:       data,
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	return NewSecretKeyStorage(client, "default", "admin-key").(*secretKeyStorage), client
}

func TestSecretKeyStorage_LoadByKeyAndID(t *testing.T) {
	storage, _ := newSecretStorageForTest(t, map[string][]byte{})
	key := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  uuid.NewString(),
		Name: "test",
		CreatedBy: &models.TeamUser{
			ID: uuid.New(),
		},
	}
	storage.storeKey(key)

	gotByKey, found := storage.LoadByKey(context.Background(), key.Key)
	require.True(t, found)
	require.Equal(t, key.ID, gotByKey.ID)

	gotByID, found := storage.LoadByID(context.Background(), key.ID.String())
	require.True(t, found)
	require.Equal(t, key.Key, gotByID.Key)

	_, found = storage.LoadByKey(context.Background(), "missing")
	assert.False(t, found)
	_, found = storage.LoadByID(context.Background(), uuid.NewString())
	assert.False(t, found)
}

func TestSecretKeyStorage_Init(t *testing.T) {
	t.Run("secret not found", func(t *testing.T) {
		client := fake.NewClientset()
		storage := NewSecretKeyStorage(client, "default", "admin-key")
		err := storage.Init(context.Background())
		require.Error(t, err)
	})

	t.Run("admin key exists", func(t *testing.T) {
		admin := &models.CreatedTeamAPIKey{ID: AdminKeyID, Key: "admin-key", Name: "admin"}
		b, err := json.Marshal(admin)
		require.NoError(t, err)
		storage, _ := newSecretStorageForTest(t, map[string][]byte{
			"admin-key":         b,
			AdminKeyID.String(): b,
		})
		require.NoError(t, storage.Init(context.Background()))
		loaded, found := storage.LoadByID(context.Background(), AdminKeyID.String())
		require.True(t, found)
		require.Equal(t, "admin-key", loaded.Key)
	})

	t.Run("create admin key", func(t *testing.T) {
		storage, client := newSecretStorageForTest(t, map[string][]byte{})
		require.NoError(t, storage.Init(context.Background()))
		secret, err := client.CoreV1().Secrets("default").Get(context.Background(), KeySecretName, metav1.GetOptions{})
		require.NoError(t, err)
		_, ok := secret.Data[AdminKeyID.String()]
		assert.True(t, ok)
	})

	t.Run("conflict while creating admin key tolerated", func(t *testing.T) {
		storage, client := newSecretStorageForTest(t, map[string][]byte{})
		var updated int32
		client.PrependReactor("update", "secrets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			if atomic.AddInt32(&updated, 1) == 1 {
				return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "", Resource: "secrets"}, KeySecretName, errors.New("conflict"))
			}
			obj := action.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
			return true, obj, nil
		})
		require.NoError(t, storage.Init(context.Background()))
	})

	t.Run("non-conflict update error returned", func(t *testing.T) {
		storage, client := newSecretStorageForTest(t, map[string][]byte{})
		client.PrependReactor("update", "secrets", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			return true, nil, errors.New("update failed")
		})
		err := storage.Init(context.Background())
		require.Error(t, err)
	})
}

func TestSecretKeyStorage_Refresh(t *testing.T) {
	valid := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  uuid.NewString(),
		Name: "valid",
		CreatedBy: &models.TeamUser{
			ID: uuid.New(),
		},
	}
	validBytes, err := json.Marshal(valid)
	require.NoError(t, err)
	storage, client := newSecretStorageForTest(t, map[string][]byte{
		valid.ID.String(): validBytes,
		"invalid":         []byte("not-json"),
	})
	storage.storeKey(&models.CreatedTeamAPIKey{ID: uuid.New(), Key: "stale"})

	require.NoError(t, storage.refresh(context.Background()))
	_, found := storage.LoadByKey(context.Background(), valid.Key)
	assert.True(t, found)
	_, found = storage.LoadByKey(context.Background(), "stale")
	assert.False(t, found)

	require.NoError(t, client.CoreV1().Secrets("default").Delete(context.Background(), KeySecretName, metav1.DeleteOptions{}))
	require.Error(t, storage.refresh(context.Background()))
}

func TestSecretKeyStorage_UpdateAndRetry(t *testing.T) {
	t.Run("update get error", func(t *testing.T) {
		client := fake.NewClientset()
		storage := NewSecretKeyStorage(client, "default", "admin-key").(*secretKeyStorage)
		err := storage.updateSecret(context.Background(), "id", &models.CreatedTeamAPIKey{ID: uuid.New(), Key: "x"})
		require.Error(t, err)
	})

	t.Run("update with nil data map creates map", func(t *testing.T) {
		storage, client := newSecretStorageForTest(t, nil)
		k := &models.CreatedTeamAPIKey{ID: uuid.New(), Key: "x", Name: "name"}
		require.NoError(t, storage.updateSecret(context.Background(), k.ID.String(), k))
		secret, err := client.CoreV1().Secrets("default").Get(context.Background(), KeySecretName, metav1.GetOptions{})
		require.NoError(t, err)
		assert.Contains(t, secret.Data, k.ID.String())
	})

	t.Run("update delete branch", func(t *testing.T) {
		k := &models.CreatedTeamAPIKey{ID: uuid.New(), Key: "x", Name: "name"}
		b, err := json.Marshal(k)
		require.NoError(t, err)
		storage, client := newSecretStorageForTest(t, map[string][]byte{k.ID.String(): b})
		require.NoError(t, storage.updateSecret(context.Background(), k.ID.String(), nil))
		secret, err := client.CoreV1().Secrets("default").Get(context.Background(), KeySecretName, metav1.GetOptions{})
		require.NoError(t, err)
		assert.NotContains(t, secret.Data, k.ID.String())
	})

	t.Run("marshal error", func(t *testing.T) {
		storage, _ := newSecretStorageForTest(t, map[string][]byte{})
		oldMarshal := marshalAPIKey
		marshalAPIKey = func(v any) ([]byte, error) { return nil, fmt.Errorf("boom") }
		t.Cleanup(func() { marshalAPIKey = oldMarshal })
		err := storage.updateSecret(context.Background(), "id", &models.CreatedTeamAPIKey{ID: uuid.New(), Key: "x"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to marshal")
	})

	t.Run("retry handles conflict", func(t *testing.T) {
		storage, client := newSecretStorageForTest(t, map[string][]byte{})
		var updated int32
		client.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if atomic.AddInt32(&updated, 1) == 1 {
				return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, KeySecretName, errors.New("conflict"))
			}
			obj := action.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
			return true, obj, nil
		})
		err := storage.retryUpdateSecret(context.Background(), uuid.NewString(), &models.CreatedTeamAPIKey{ID: uuid.New(), Key: "x"})
		require.NoError(t, err)
		assert.GreaterOrEqual(t, atomic.LoadInt32(&updated), int32(2))
	})
}

func TestSecretKeyStorage_CreateKey(t *testing.T) {
	storage, _ := newSecretStorageForTest(t, map[string][]byte{})
	user := &models.CreatedTeamAPIKey{ID: uuid.New(), CreatedBy: &models.TeamUser{ID: uuid.New()}}

	t.Run("validation", func(t *testing.T) {
		_, err := storage.CreateKey(context.Background(), nil, "x")
		require.Error(t, err)
		_, err = storage.CreateKey(context.Background(), user, "")
		require.Error(t, err)
	})

	t.Run("success", func(t *testing.T) {
		key, err := storage.CreateKey(context.Background(), user, "name")
		require.NoError(t, err)
		require.NotNil(t, key)
		_, found := storage.LoadByID(context.Background(), key.ID.String())
		assert.True(t, found)
	})

	t.Run("unique id exhaustion", func(t *testing.T) {
		oldGenerate := generateUUID
		fixed := uuid.MustParse("11111111-1111-1111-1111-111111111111")
		generateUUID = func() uuid.UUID { return fixed }
		t.Cleanup(func() { generateUUID = oldGenerate })
		storage.storeKey(&models.CreatedTeamAPIKey{ID: fixed, Key: fixed.String()})
		_, err := storage.CreateKey(context.Background(), user, "name")
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to generate unique api-key")
	})

	t.Run("update error", func(t *testing.T) {
		storageErr, client := newSecretStorageForTest(t, map[string][]byte{})
		client.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("update failed")
		})
		_, err := storageErr.CreateKey(context.Background(), user, "name")
		require.Error(t, err)
	})
}

func TestSecretKeyStorage_DeleteAndList(t *testing.T) {
	storage, client := newSecretStorageForTest(t, map[string][]byte{})
	owner := uuid.New()
	other := uuid.New()
	user := &models.CreatedTeamAPIKey{ID: owner, CreatedBy: &models.TeamUser{ID: owner}}

	key, err := storage.CreateKey(context.Background(), user, "name")
	require.NoError(t, err)

	// force owner match via CreatedBy
	otherKey := &models.CreatedTeamAPIKey{ID: other, Key: uuid.NewString(), Name: "other", CreatedBy: &models.TeamUser{ID: owner}}
	storage.storeKey(otherKey)

	keys, err := storage.ListByOwner(context.Background(), owner)
	require.NoError(t, err)
	require.Len(t, keys, 2)

	keys, err = storage.ListByOwner(context.Background(), uuid.New())
	require.NoError(t, err)
	require.Len(t, keys, 0)

	require.NoError(t, storage.DeleteKey(context.Background(), nil))
	require.NoError(t, storage.DeleteKey(context.Background(), key))
	_, found := storage.LoadByID(context.Background(), key.ID.String())
	assert.False(t, found)

	secret, err := client.CoreV1().Secrets("default").Get(context.Background(), KeySecretName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, secret.Data, key.ID.String())

	storage2, client2 := newSecretStorageForTest(t, map[string][]byte{})
	key2, err := storage2.CreateKey(context.Background(), user, "name")
	require.NoError(t, err)
	client2.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("delete failed")
	})
	require.Error(t, storage2.DeleteKey(context.Background(), key2))
}

func TestSecretKeyStorage_RunStop(t *testing.T) {
	storage, _ := newSecretStorageForTest(t, map[string][]byte{})
	oldTicker := newRefreshTicker
	newRefreshTicker = func() *time.Ticker { return time.NewTicker(2 * time.Millisecond) }
	t.Cleanup(func() { newRefreshTicker = oldTicker })
	storage.Run()
	time.Sleep(8 * time.Millisecond)
	storage.Stop()
	require.NotPanics(t, func() { storage.Stop() })
}

func TestSecretKeyStorage_RunRefreshErrorBranch(t *testing.T) {
	storage, client := newSecretStorageForTest(t, map[string][]byte{})
	require.NoError(t, client.CoreV1().Secrets("default").Delete(context.Background(), KeySecretName, metav1.DeleteOptions{}))
	oldTicker := newRefreshTicker
	newRefreshTicker = func() *time.Ticker { return time.NewTicker(2 * time.Millisecond) }
	t.Cleanup(func() { newRefreshTicker = oldTicker })
	storage.Run()
	time.Sleep(8 * time.Millisecond)
	storage.Stop()
}
