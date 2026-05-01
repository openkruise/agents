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
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

type updateHookClient struct {
	client.Client
	updateHook func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error
}

func (c *updateHookClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if c.updateHook != nil {
		if err := c.updateHook(ctx, obj, opts...); err != nil {
			return err
		}
	}
	return c.Client.Update(ctx, obj, opts...)
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	return scheme
}

func newSecretStorageForTest(t *testing.T, data map[string][]byte) (*secretKeyStorage, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(newTestScheme(t)).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: KeySecretName, Namespace: "default"},
		Data:       data,
	}).Build()
	return NewSecretKeyStorage(c, c, "default", "admin-key").(*secretKeyStorage), c
}

func getSecretForTest(t *testing.T, c client.Client) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: KeySecretName}, secret)
	require.NoError(t, err)
	return secret
}

func assertExpectedError(t *testing.T, err error, expectError string) {
	t.Helper()
	if expectError == "" {
		require.NoError(t, err)
		return
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), expectError)
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
	tests := []struct {
		name        string
		prepare     func(t *testing.T) (*secretKeyStorage, client.Client)
		assertion   func(t *testing.T, storage *secretKeyStorage, c client.Client)
		expectError string
	}{
		{
			name: "secret not found",
			prepare: func(t *testing.T) (*secretKeyStorage, client.Client) {
				c := fake.NewClientBuilder().WithScheme(newTestScheme(t)).Build()
				return NewSecretKeyStorage(c, c, "default", "admin-key").(*secretKeyStorage), c
			},
			expectError: "not found",
		},
		{
			name: "admin key exists",
			prepare: func(t *testing.T) (*secretKeyStorage, client.Client) {
				admin := &models.CreatedTeamAPIKey{ID: AdminKeyID, Key: "admin-key", Name: "admin"}
				b, err := json.Marshal(admin)
				require.NoError(t, err)
				return newSecretStorageForTest(t, map[string][]byte{
					"admin-key":         b,
					AdminKeyID.String(): b,
				})
			},
			assertion: func(t *testing.T, storage *secretKeyStorage, _ client.Client) {
				loaded, found := storage.LoadByID(context.Background(), AdminKeyID.String())
				require.True(t, found)
				require.Equal(t, "admin-key", loaded.Key)
				require.Equal(t, models.AdminTeam(), loaded.Team)
			},
		},
		{
			name: "create admin key",
			prepare: func(t *testing.T) (*secretKeyStorage, client.Client) {
				return newSecretStorageForTest(t, map[string][]byte{})
			},
			assertion: func(t *testing.T, _ *secretKeyStorage, c client.Client) {
				secret := getSecretForTest(t, c)
				bytes, ok := secret.Data[AdminKeyID.String()]
				assert.True(t, ok)
				var admin models.CreatedTeamAPIKey
				require.NoError(t, json.Unmarshal(bytes, &admin))
				require.Equal(t, models.AdminTeam(), admin.Team)
			},
		},
		{
			name: "conflict while creating admin key tolerated",
			prepare: func(t *testing.T) (*secretKeyStorage, client.Client) {
				_, c := newSecretStorageForTest(t, map[string][]byte{})
				var updated int32
				hookClient := &updateHookClient{
					Client: c,
					updateHook: func(_ context.Context, _ client.Object, _ ...client.UpdateOption) error {
						if atomic.AddInt32(&updated, 1) == 1 {
							return apierrors.NewConflict(schema.GroupResource{Group: "", Resource: "secrets"}, KeySecretName, errors.New("conflict"))
						}
						return nil
					},
				}
				return NewSecretKeyStorage(hookClient, hookClient, "default", "admin-key").(*secretKeyStorage), c
			},
		},
		{
			name: "non-conflict update error returned",
			prepare: func(t *testing.T) (*secretKeyStorage, client.Client) {
				_, c := newSecretStorageForTest(t, map[string][]byte{})
				hookClient := &updateHookClient{
					Client: c,
					updateHook: func(_ context.Context, _ client.Object, _ ...client.UpdateOption) error {
						return errors.New("update failed")
					},
				}
				return NewSecretKeyStorage(hookClient, hookClient, "default", "admin-key").(*secretKeyStorage), c
			},
			expectError: "update failed",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			storage, c := tt.prepare(t)
			err := storage.Init(context.Background())
			assertExpectedError(t, err, tt.expectError)
			if err == nil && tt.assertion != nil {
				tt.assertion(t, storage, c)
			}
		})
	}
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
	storage, c := newSecretStorageForTest(t, map[string][]byte{
		valid.ID.String(): validBytes,
		"invalid":         []byte("not-json"),
	})
	storage.storeKey(&models.CreatedTeamAPIKey{ID: uuid.New(), Key: "stale"})

	require.NoError(t, storage.refresh(context.Background(), c))
	loaded, found := storage.LoadByKey(context.Background(), valid.Key)
	assert.True(t, found)
	require.Equal(t, models.AdminTeam(), loaded.Team)
	_, found = storage.LoadByKey(context.Background(), "stale")
	assert.False(t, found)

	require.NoError(t, c.Delete(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: KeySecretName, Namespace: "default"},
	}))
	require.Error(t, storage.refresh(context.Background(), c))
}

func TestSecretKeyStorage_UpdateAndRetry(t *testing.T) {
	tests := []struct {
		name        string
		run         func(t *testing.T) error
		expectError string
	}{
		{
			name: "update get error",
			run: func(t *testing.T) error {
				c := fake.NewClientBuilder().WithScheme(newTestScheme(t)).Build()
				storage := NewSecretKeyStorage(c, c, "default", "admin-key").(*secretKeyStorage)
				return storage.updateSecret(context.Background(), "id", &models.CreatedTeamAPIKey{ID: uuid.New(), Key: "x"})
			},
			expectError: "not found",
		},
		{
			name: "update with nil data map creates map",
			run: func(t *testing.T) error {
				storage, c := newSecretStorageForTest(t, nil)
				k := &models.CreatedTeamAPIKey{ID: uuid.New(), Key: "x", Name: "name"}
				err := storage.updateSecret(context.Background(), k.ID.String(), k)
				if err == nil {
					secret := getSecretForTest(t, c)
					assert.Contains(t, secret.Data, k.ID.String())
				}
				return err
			},
		},
		{
			name: "update delete branch",
			run: func(t *testing.T) error {
				k := &models.CreatedTeamAPIKey{ID: uuid.New(), Key: "x", Name: "name"}
				b, err := json.Marshal(k)
				require.NoError(t, err)
				storage, c := newSecretStorageForTest(t, map[string][]byte{k.ID.String(): b})
				err = storage.updateSecret(context.Background(), k.ID.String(), nil)
				if err == nil {
					secret := getSecretForTest(t, c)
					assert.NotContains(t, secret.Data, k.ID.String())
				}
				return err
			},
		},
		{
			name: "marshal error",
			run: func(t *testing.T) error {
				storage, _ := newSecretStorageForTest(t, map[string][]byte{})
				oldMarshal := marshalAPIKey
				marshalAPIKey = func(v any) ([]byte, error) { return nil, fmt.Errorf("boom") }
				t.Cleanup(func() { marshalAPIKey = oldMarshal })
				return storage.updateSecret(context.Background(), "id", &models.CreatedTeamAPIKey{ID: uuid.New(), Key: "x"})
			},
			expectError: "failed to marshal",
		},
		{
			name: "retry handles conflict",
			run: func(t *testing.T) error {
				_, c := newSecretStorageForTest(t, map[string][]byte{})
				var updated int32
				hookClient := &updateHookClient{
					Client: c,
					updateHook: func(_ context.Context, _ client.Object, _ ...client.UpdateOption) error {
						if atomic.AddInt32(&updated, 1) == 1 {
							return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, KeySecretName, errors.New("conflict"))
						}
						return nil
					},
				}
				storage := NewSecretKeyStorage(hookClient, hookClient, "default", "admin-key").(*secretKeyStorage)
				err := storage.retryUpdateSecret(context.Background(), uuid.NewString(), &models.CreatedTeamAPIKey{ID: uuid.New(), Key: "x"})
				if err == nil {
					assert.GreaterOrEqual(t, atomic.LoadInt32(&updated), int32(2))
				}
				return err
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run(t)
			assertExpectedError(t, err, tt.expectError)
		})
	}
}

func TestSecretKeyStorage_CreateKey(t *testing.T) {
	tests := []struct {
		name        string
		run         func(t *testing.T) error
		expectError string
	}{
		{
			name: "validation nil user",
			run: func(t *testing.T) error {
				storage, _ := newSecretStorageForTest(t, map[string][]byte{})
				_, err := storage.CreateKey(context.Background(), nil, CreateKeyOptions{Name: "x"})
				return err
			},
			expectError: "required",
		},
		{
			name: "validation empty name",
			run: func(t *testing.T) error {
				storage, _ := newSecretStorageForTest(t, map[string][]byte{})
				user := &models.CreatedTeamAPIKey{ID: uuid.New(), CreatedBy: &models.TeamUser{ID: uuid.New()}}
				_, err := storage.CreateKey(context.Background(), user, CreateKeyOptions{})
				return err
			},
			expectError: "required",
		},
		{
			name: "success",
			run: func(t *testing.T) error {
				storage, _ := newSecretStorageForTest(t, map[string][]byte{})
				userTeam := &models.Team{ID: uuid.New(), Name: "user-team"}
				user := &models.CreatedTeamAPIKey{ID: uuid.New(), Team: userTeam, CreatedBy: &models.TeamUser{ID: uuid.New()}}
				key, err := storage.CreateKey(context.Background(), user, CreateKeyOptions{Name: "name"})
				if err == nil {
					require.NotNil(t, key)
					require.Equal(t, userTeam, key.Team)
					require.NotNil(t, key.CreatedBy)
					require.Equal(t, user.ID, key.CreatedBy.ID)
					_, found := storage.LoadByID(context.Background(), key.ID.String())
					assert.True(t, found)
				}
				return err
			},
		},
		{
			name: "unique id exhaustion",
			run: func(t *testing.T) error {
				storage, _ := newSecretStorageForTest(t, map[string][]byte{})
				user := &models.CreatedTeamAPIKey{ID: uuid.New(), CreatedBy: &models.TeamUser{ID: uuid.New()}}
				oldGenerate := generateUUID
				fixed := uuid.MustParse("11111111-1111-1111-1111-111111111111")
				generateUUID = func() uuid.UUID { return fixed }
				t.Cleanup(func() { generateUUID = oldGenerate })
				storage.storeKey(&models.CreatedTeamAPIKey{ID: fixed, Key: fixed.String()})
				_, err := storage.CreateKey(context.Background(), user, CreateKeyOptions{Name: "name"})
				return err
			},
			expectError: "failed to generate unique api-key",
		},
		{
			name: "update error",
			run: func(t *testing.T) error {
				user := &models.CreatedTeamAPIKey{ID: uuid.New(), CreatedBy: &models.TeamUser{ID: uuid.New()}}
				_, c := newSecretStorageForTest(t, map[string][]byte{})
				hookClient := &updateHookClient{
					Client: c,
					updateHook: func(_ context.Context, _ client.Object, _ ...client.UpdateOption) error {
						return errors.New("update failed")
					},
				}
				storageErr := NewSecretKeyStorage(hookClient, hookClient, "default", "admin-key").(*secretKeyStorage)
				_, err := storageErr.CreateKey(context.Background(), user, CreateKeyOptions{Name: "name"})
				return err
			},
			expectError: "update failed",
		},
		{
			name: "success defaults missing user team to admin team",
			run: func(t *testing.T) error {
				storage, _ := newSecretStorageForTest(t, map[string][]byte{})
				key, err := storage.CreateKey(t.Context(), &models.CreatedTeamAPIKey{ID: uuid.New()}, CreateKeyOptions{Name: "default-team"})
				require.NoError(t, err)
				require.Equal(t, models.AdminTeam(), key.Team)
				return err
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run(t)
			assertExpectedError(t, err, tt.expectError)
		})
	}
}

func TestSecretKeyStorage_DeleteAndList(t *testing.T) {
	storage, c := newSecretStorageForTest(t, map[string][]byte{})
	owner := uuid.New()
	other := uuid.New()
	teamA := &models.Team{ID: uuid.New(), Name: "team-a"}
	teamB := &models.Team{ID: uuid.New(), Name: "team-b"}
	user := &models.CreatedTeamAPIKey{ID: owner, Team: teamA, CreatedBy: &models.TeamUser{ID: owner}}
	storage.storeKey(user)

	key, err := storage.CreateKey(context.Background(), user, CreateKeyOptions{Name: "name"})
	require.NoError(t, err)

	// Same-team key should be visible even if CreatedBy does not match.
	otherKey := &models.CreatedTeamAPIKey{ID: other, Key: uuid.NewString(), Name: "other", Team: teamA, CreatedBy: &models.TeamUser{ID: uuid.New()}}
	storage.storeKey(otherKey)

	// CreatedBy no longer grants visibility across teams.
	crossTeamKey := &models.CreatedTeamAPIKey{ID: uuid.New(), Key: uuid.NewString(), Name: "cross", Team: teamB, CreatedBy: &models.TeamUser{ID: owner}}
	storage.storeKey(crossTeamKey)

	keys, err := storage.ListByOwner(context.Background(), owner)
	require.NoError(t, err)
	require.Len(t, keys, 3)

	keys, err = storage.ListByOwner(context.Background(), uuid.New())
	require.NoError(t, err)
	require.Len(t, keys, 0)

	require.NoError(t, storage.DeleteKey(context.Background(), nil))
	require.NoError(t, storage.DeleteKey(context.Background(), key))
	_, found := storage.LoadByID(context.Background(), key.ID.String())
	assert.False(t, found)

	secret := getSecretForTest(t, c)
	assert.NotContains(t, secret.Data, key.ID.String())

	storage2, c2 := newSecretStorageForTest(t, map[string][]byte{})
	key2, err := storage2.CreateKey(context.Background(), user, CreateKeyOptions{Name: "name"})
	require.NoError(t, err)
	hookClient := &updateHookClient{
		Client: c2,
		updateHook: func(_ context.Context, _ client.Object, _ ...client.UpdateOption) error {
			return errors.New("delete failed")
		},
	}
	storageWithDeleteError := NewSecretKeyStorage(hookClient, hookClient, "default", "admin-key").(*secretKeyStorage)
	require.Error(t, storageWithDeleteError.DeleteKey(context.Background(), key2))
}

func TestSecretKeyStorage_TeamLifecycle(t *testing.T) {
	tests := []struct {
		name        string
		run         func(t *testing.T) error
		expectError string
	}{
		{
			name: "create key targets existing team by name",
			run: func(t *testing.T) error {
				storage, _ := newSecretStorageForTest(t, map[string][]byte{})
				teamA := &models.Team{ID: uuid.New(), Name: "team-a"}
				existing := &models.CreatedTeamAPIKey{ID: uuid.New(), Key: uuid.NewString(), Name: "existing", Team: teamA}
				storage.storeKey(existing)

				creator := &models.CreatedTeamAPIKey{ID: uuid.New(), Team: models.AdminTeam()}
				key, err := storage.CreateKey(context.Background(), creator, CreateKeyOptions{Name: "new", TeamName: "team-a"})
				if err != nil {
					return err
				}
				require.Equal(t, teamA, key.Team)
				return nil
			},
		},
		{
			name: "find team by name and list teams deduplicates",
			run: func(t *testing.T) error {
				storage, _ := newSecretStorageForTest(t, map[string][]byte{})
				admin := &models.CreatedTeamAPIKey{ID: AdminKeyID, Key: "admin", Name: "admin", Team: models.AdminTeam()}
				storage.storeKey(admin)
				teamA := &models.Team{ID: uuid.New(), Name: "team-a"}
				storage.storeKey(&models.CreatedTeamAPIKey{ID: uuid.New(), Key: uuid.NewString(), Name: "a1", Team: teamA})
				storage.storeKey(&models.CreatedTeamAPIKey{ID: uuid.New(), Key: uuid.NewString(), Name: "a2", Team: teamA})

				found, ok, err := storage.FindTeamByName(context.Background(), "team-a")
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, teamA, found)

				teams, err := storage.ListTeams(context.Background(), admin)
				require.NoError(t, err)
				require.Len(t, teams, 2)
				names := make([]string, 0, len(teams))
				for _, team := range teams {
					names = append(names, team.Name)
					require.Empty(t, team.APIKey)
				}
				require.ElementsMatch(t, []string{"admin", "team-a"}, names)
				return nil
			},
		},
		{
			name: "normal caller lists own team only",
			run: func(t *testing.T) error {
				storage, _ := newSecretStorageForTest(t, map[string][]byte{})
				teamA := &models.Team{ID: uuid.New(), Name: "team-a"}
				teamB := &models.Team{ID: uuid.New(), Name: "team-b"}
				caller := &models.CreatedTeamAPIKey{ID: uuid.New(), Key: uuid.NewString(), Name: "caller", Team: teamA}
				storage.storeKey(caller)
				storage.storeKey(&models.CreatedTeamAPIKey{ID: uuid.New(), Key: uuid.NewString(), Name: "other", Team: teamB})

				teams, err := storage.ListTeams(context.Background(), caller)
				require.NoError(t, err)
				require.Len(t, teams, 1)
				require.Equal(t, "team-a", teams[0].Name)
				require.True(t, teams[0].IsDefault)
				return nil
			},
		},
		{
			name: "delete last admin key is forbidden",
			run: func(t *testing.T) error {
				storage, _ := newSecretStorageForTest(t, map[string][]byte{})
				admin := &models.CreatedTeamAPIKey{ID: AdminKeyID, Key: "admin", Name: "admin", Team: models.AdminTeam()}
				storage.storeKey(admin)
				return storage.DeleteKey(context.Background(), admin)
			},
			expectError: "last active admin",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run(t)
			assertExpectedError(t, err, tt.expectError)
		})
	}
}

func TestSecretKeyStorage_IdxByTeamCache(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "storeKey populates idxByTeam",
			run: func(t *testing.T) {
				storage, _ := newSecretStorageForTest(t, map[string][]byte{})
				teamA := &models.Team{ID: uuid.New(), Name: "team-a"}
				storage.storeKey(&models.CreatedTeamAPIKey{ID: uuid.New(), Key: uuid.NewString(), Name: "k1", Team: teamA})

				found, ok, err := storage.FindTeamByName(context.Background(), "team-a")
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, teamA.ID, found.ID)
				require.Equal(t, teamA.Name, found.Name)
			},
		},
		{
			name: "FindTeamByName returns false for missing team",
			run: func(t *testing.T) {
				storage, _ := newSecretStorageForTest(t, map[string][]byte{})
				_, ok, err := storage.FindTeamByName(context.Background(), "nonexistent")
				require.NoError(t, err)
				require.False(t, ok)
			},
		},
		{
			name: "FindTeamByName returns clone not reference",
			run: func(t *testing.T) {
				storage, _ := newSecretStorageForTest(t, map[string][]byte{})
				teamA := &models.Team{ID: uuid.New(), Name: "team-a"}
				storage.storeKey(&models.CreatedTeamAPIKey{ID: uuid.New(), Key: uuid.NewString(), Name: "k1", Team: teamA})

				found1, _, _ := storage.FindTeamByName(context.Background(), "team-a")
				found2, _, _ := storage.FindTeamByName(context.Background(), "team-a")
				require.Equal(t, found1, found2)
				// Verify they are different pointers (clone).
				require.NotSame(t, found1, found2)
			},
		},
		{
			name: "refresh cleans stale teams from idxByTeam",
			run: func(t *testing.T) {
				teamA := &models.Team{ID: uuid.New(), Name: "team-a"}
				key1 := &models.CreatedTeamAPIKey{ID: uuid.New(), Key: uuid.NewString(), Name: "k1", Team: teamA}
				b, err := json.Marshal(key1)
				require.NoError(t, err)

				storage, c := newSecretStorageForTest(t, map[string][]byte{key1.ID.String(): b})
				require.NoError(t, storage.refresh(context.Background(), c))

				// team-a should be findable
				_, ok, _ := storage.FindTeamByName(context.Background(), "team-a")
				require.True(t, ok)

				// Manually insert a stale team
				storage.idxByTeam.Store("stale-team", &models.Team{ID: uuid.New(), Name: "stale-team"})

				// After refresh, stale team should be cleaned
				require.NoError(t, storage.refresh(context.Background(), c))
				_, ok, _ = storage.FindTeamByName(context.Background(), "stale-team")
				require.False(t, ok)

				// team-a should still be present
				_, ok, _ = storage.FindTeamByName(context.Background(), "team-a")
				require.True(t, ok)
			},
		},
		{
			name: "delete key does not immediately remove team from cache",
			run: func(t *testing.T) {
				storage, _ := newSecretStorageForTest(t, map[string][]byte{})
				teamA := &models.Team{ID: uuid.New(), Name: "team-a"}
				key1 := &models.CreatedTeamAPIKey{ID: uuid.New(), Key: uuid.NewString(), Name: "k1", Team: teamA}
				storage.storeKey(key1)

				// Delete the only key in team-a
				require.NoError(t, storage.DeleteKey(context.Background(), key1))

				// Team should still be in idxByTeam (eventual consistency, cleaned by refresh)
				_, ok, _ := storage.FindTeamByName(context.Background(), "team-a")
				require.True(t, ok)
			},
		},
		{
			name: "multiple keys same team share one idxByTeam entry",
			run: func(t *testing.T) {
				storage, _ := newSecretStorageForTest(t, map[string][]byte{})
				teamA := &models.Team{ID: uuid.New(), Name: "team-a"}
				storage.storeKey(&models.CreatedTeamAPIKey{ID: uuid.New(), Key: uuid.NewString(), Name: "k1", Team: teamA})
				storage.storeKey(&models.CreatedTeamAPIKey{ID: uuid.New(), Key: uuid.NewString(), Name: "k2", Team: teamA})

				found, ok, err := storage.FindTeamByName(context.Background(), "team-a")
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, teamA.ID, found.ID)
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t)
		})
	}
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
	storage, c := newSecretStorageForTest(t, map[string][]byte{})
	require.NoError(t, c.Delete(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: KeySecretName, Namespace: "default"},
	}))
	oldTicker := newRefreshTicker
	newRefreshTicker = func() *time.Ticker { return time.NewTicker(2 * time.Millisecond) }
	t.Cleanup(func() { newRefreshTicker = oldTicker })
	storage.Run()
	time.Sleep(8 * time.Millisecond)
	storage.Stop()
}
