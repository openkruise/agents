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
	"sync"
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
	toolscache "k8s.io/client-go/tools/cache"
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

type getHookClient struct {
	client.Client
	getHook func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error
}

func (c *getHookClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if c.getHook != nil {
		if err := c.getHook(ctx, key, obj, opts...); err != nil {
			return err
		}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

type staleSecretReadClient struct {
	client.Client

	mu       sync.Mutex
	secret   *corev1.Secret
	getCount int32
}

func (c *staleSecretReadClient) SetSecret(secret *corev1.Secret) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.secret = secret.DeepCopy()
}

func (c *staleSecretReadClient) ClearSecret() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.secret = nil
}

func (c *staleSecretReadClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	atomic.AddInt32(&c.getCount, 1)
	c.mu.Lock()
	var secret *corev1.Secret
	if c.secret != nil {
		secret = c.secret.DeepCopy()
	}
	c.mu.Unlock()
	if secret != nil && key.Namespace == secret.Namespace && key.Name == secret.Name {
		target, ok := obj.(*corev1.Secret)
		if ok {
			secret.DeepCopyInto(target)
			return nil
		}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *staleSecretReadClient) GetCount() int32 {
	return atomic.LoadInt32(&c.getCount)
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
	return NewSecretKeyStorage(c, c, nil, "default", "admin-key").(*secretKeyStorage), c
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

func useGeneratedUUIDsForTest(t *testing.T, ids ...uuid.UUID) {
	t.Helper()
	oldGenerate := generateUUID
	remaining := append([]uuid.UUID(nil), ids...)
	generateUUID = func() uuid.UUID {
		if len(remaining) == 0 {
			t.Fatalf("generateUUID called more times than expected")
			return uuid.Nil
		}
		next := remaining[0]
		remaining = remaining[1:]
		return next
	}
	t.Cleanup(func() { generateUUID = oldGenerate })
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
				return NewSecretKeyStorage(c, c, nil, "default", "admin-key").(*secretKeyStorage), c
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
				return NewSecretKeyStorage(hookClient, hookClient, nil, "default", "admin-key").(*secretKeyStorage), c
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
				return NewSecretKeyStorage(hookClient, hookClient, nil, "default", "admin-key").(*secretKeyStorage), c
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
				storage := NewSecretKeyStorage(c, c, nil, "default", "admin-key").(*secretKeyStorage)
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
				storage := NewSecretKeyStorage(hookClient, hookClient, nil, "default", "admin-key").(*secretKeyStorage)
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
				storage, c := newSecretStorageForTest(t, map[string][]byte{})
				userTeam := &models.Team{Name: "user-team"}
				user := &models.CreatedTeamAPIKey{ID: uuid.New(), Team: userTeam, CreatedBy: &models.TeamUser{ID: uuid.New()}}
				key, err := storage.CreateKey(context.Background(), user, CreateKeyOptions{Name: "name"})
				if err == nil {
					require.NotNil(t, key)
					require.Equal(t, userTeam, key.Team)
					require.NotNil(t, key.CreatedBy)
					require.Equal(t, user.ID, key.CreatedBy.ID)
					_, found := storage.LoadByID(context.Background(), key.ID.String())
					assert.False(t, found, "created key should wait for informer-backed refresh before entering the index")

					secret := getSecretForTest(t, c)
					require.Contains(t, secret.Data, key.ID.String())
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
				storageErr := NewSecretKeyStorage(hookClient, hookClient, nil, "default", "admin-key").(*secretKeyStorage)
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
		{
			name: "conflict retry reuses same team from latest secret",
			run: func(t *testing.T) error {
				_, c := newSecretStorageForTest(t, map[string][]byte{})
				teamName := "team-a"
				existingTeam := &models.Team{Name: teamName}
				existingKey := &models.CreatedTeamAPIKey{
					ID:   uuid.MustParse("22222222-2222-2222-2222-222222222222"),
					Key:  "existing-key",
					Name: "existing",
					Team: existingTeam,
				}
				existingBytes, err := json.Marshal(existingKey)
				require.NoError(t, err)

				var updated int32
				hookClient := &updateHookClient{
					Client: c,
					updateHook: func(ctx context.Context, _ client.Object, _ ...client.UpdateOption) error {
						if atomic.AddInt32(&updated, 1) != 1 {
							return nil
						}
						secret := getSecretForTest(t, c)
						if secret.Data == nil {
							secret.Data = map[string][]byte{}
						}
						secret.Data[existingKey.ID.String()] = existingBytes
						require.NoError(t, c.Update(ctx, secret))
						return apierrors.NewConflict(schema.GroupResource{Group: "", Resource: "secrets"}, KeySecretName, errors.New("conflict"))
					},
				}
				storage := NewSecretKeyStorage(hookClient, hookClient, nil, "default", "admin-key").(*secretKeyStorage)

				useGeneratedUUIDsForTest(t,
					uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
					uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
					uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc"),
				)

				creator := &models.CreatedTeamAPIKey{ID: uuid.New(), Team: models.AdminTeam()}
				key, err := storage.CreateKey(context.Background(), creator, CreateKeyOptions{Name: "new", TeamName: teamName})
				if err != nil {
					return err
				}
				require.Equal(t, existingTeam, key.Team)

				secret := getSecretForTest(t, c)
				require.Contains(t, secret.Data, key.ID.String())
				var storedKey models.CreatedTeamAPIKey
				require.NoError(t, json.Unmarshal(secret.Data[key.ID.String()], &storedKey))
				require.Equal(t, existingTeam, storedKey.Team)
				return nil
			},
		},
		{
			name: "create reuses team from secret and skips invalid data",
			run: func(t *testing.T) error {
				teamName := "team-a"
				existingTeam := &models.Team{Name: teamName}
				existingKey := &models.CreatedTeamAPIKey{
					ID:   uuid.MustParse("22222222-2222-2222-2222-222222222222"),
					Key:  "existing-key",
					Name: "existing",
					Team: existingTeam,
				}
				existingBytes, err := json.Marshal(existingKey)
				require.NoError(t, err)
				storage, _ := newSecretStorageForTest(t, map[string][]byte{
					"invalid":               []byte("not-json"),
					existingKey.ID.String(): existingBytes,
				})

				useGeneratedUUIDsForTest(t,
					uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
					uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
					uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc"),
				)

				creator := &models.CreatedTeamAPIKey{ID: uuid.New(), Team: models.AdminTeam()}
				key, err := storage.CreateKey(context.Background(), creator, CreateKeyOptions{Name: "new", TeamName: teamName})
				if err != nil {
					return err
				}
				require.Equal(t, existingTeam, key.Team)
				return nil
			},
		},
		{
			name: "new target team uses generated team when secret has no match",
			run: func(t *testing.T) error {
				storage, _ := newSecretStorageForTest(t, map[string][]byte{})
				generatedTeam := &models.Team{
					ID:   uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
					Name: "team-a",
				}

				useGeneratedUUIDsForTest(t,
					uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
					uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc"),
					uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd"),
				)

				creator := &models.CreatedTeamAPIKey{ID: uuid.New(), Team: models.AdminTeam()}
				key, err := storage.CreateKey(context.Background(), creator, CreateKeyOptions{Name: "new", TeamName: generatedTeam.Name})
				if err != nil {
					return err
				}
				require.Equal(t, generatedTeam, key.Team)
				return nil
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

func TestSecretKeyStorage_CreateKeyInSecretErrors(t *testing.T) {
	tests := []struct {
		name        string
		run         func(t *testing.T) error
		expectError string
	}{
		{
			name: "get secret error",
			run: func(t *testing.T) error {
				c := fake.NewClientBuilder().WithScheme(newTestScheme(t)).Build()
				storage := NewSecretKeyStorage(c, c, nil, "default", "admin-key").(*secretKeyStorage)
				_, err := storage.createKeyInSecret(t.Context(), uuid.NewString(), &models.CreatedTeamAPIKey{
					ID:   uuid.New(),
					Key:  uuid.NewString(),
					Name: "new",
				})
				return err
			},
			expectError: "not found",
		},
		{
			name: "marshal error",
			run: func(t *testing.T) error {
				storage, _ := newSecretStorageForTest(t, map[string][]byte{})
				oldMarshal := marshalAPIKey
				marshalAPIKey = func(v any) ([]byte, error) { return nil, fmt.Errorf("marshal failed") }
				t.Cleanup(func() { marshalAPIKey = oldMarshal })
				_, err := storage.createKeyInSecret(t.Context(), uuid.NewString(), &models.CreatedTeamAPIKey{
					ID:   uuid.New(),
					Key:  uuid.NewString(),
					Name: "new",
				})
				return err
			},
			expectError: "failed to marshal",
		},
		{
			name: "update error",
			run: func(t *testing.T) error {
				_, c := newSecretStorageForTest(t, map[string][]byte{})
				hookClient := &updateHookClient{
					Client: c,
					updateHook: func(_ context.Context, _ client.Object, _ ...client.UpdateOption) error {
						return errors.New("update failed")
					},
				}
				storage := NewSecretKeyStorage(hookClient, hookClient, nil, "default", "admin-key").(*secretKeyStorage)
				_, err := storage.createKeyInSecret(t.Context(), uuid.NewString(), &models.CreatedTeamAPIKey{
					ID:   uuid.New(),
					Key:  uuid.NewString(),
					Name: "new",
				})
				return err
			},
			expectError: "update failed",
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

func TestFindTeamByNameInSecret(t *testing.T) {
	tests := []struct {
		name      string
		data      map[string][]byte
		teamName  string
		expectHit bool
	}{
		{
			name:      "invalid data is skipped",
			data:      map[string][]byte{"invalid": []byte("not-json")},
			teamName:  "team-a",
			expectHit: false,
		},
		{
			name: "matching team is cloned",
			data: func() map[string][]byte {
				key := &models.CreatedTeamAPIKey{
					ID:   uuid.New(),
					Key:  uuid.NewString(),
					Name: "existing",
					Team: &models.Team{Name: "team-a"},
				}
				b, err := json.Marshal(key)
				require.NoError(t, err)
				return map[string][]byte{key.ID.String(): b}
			}(),
			teamName:  "team-a",
			expectHit: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			secret := &corev1.Secret{Data: tt.data}
			team, found := findTeamByNameInSecret(secret, tt.teamName)
			assert.Equal(t, tt.expectHit, found)
			if tt.expectHit {
				require.NotNil(t, team)
				assert.Equal(t, tt.teamName, team.Name)
			}
		})
	}
}

func TestSecretKeyStorage_DeleteAndList(t *testing.T) {
	storage, c := newSecretStorageForTest(t, map[string][]byte{})
	owner := uuid.New()
	other := uuid.New()
	teamA := &models.Team{Name: "team-a"}
	teamB := &models.Team{Name: "team-b"}
	user := &models.CreatedTeamAPIKey{ID: owner, Team: teamA, CreatedBy: &models.TeamUser{ID: owner}}
	storage.storeKey(user)

	key, err := storage.CreateKey(context.Background(), user, CreateKeyOptions{Name: "name"})
	require.NoError(t, err)
	// This test exercises list/delete behavior. Seed the created key into the
	// index explicitly because CreateKey no longer mutates informer-owned state.
	storage.storeKey(key)

	// Same-team key should be visible even if CreatedBy does not match.
	otherKey := &models.CreatedTeamAPIKey{ID: other, Key: uuid.NewString(), Name: "other", Team: teamA, CreatedBy: &models.TeamUser{ID: uuid.New()}}
	storage.storeKey(otherKey)

	// CreatedBy no longer grants visibility across teams.
	crossTeamKey := &models.CreatedTeamAPIKey{ID: uuid.New(), Key: uuid.NewString(), Name: "cross", Team: teamB, CreatedBy: &models.TeamUser{ID: owner}}
	storage.storeKey(crossTeamKey)

	keys, err := storage.ListByOwnerTeam(context.Background(), &models.CreatedTeamAPIKey{ID: owner, Team: teamA})
	require.NoError(t, err)
	require.Len(t, keys, 3)

	keys, err = storage.ListByOwnerTeam(context.Background(), &models.CreatedTeamAPIKey{ID: uuid.New(), Team: &models.Team{Name: "nonexistent"}})
	require.NoError(t, err)
	require.Len(t, keys, 0)

	require.NoError(t, storage.DeleteKey(context.Background(), nil))
	require.NoError(t, storage.DeleteKey(context.Background(), key))
	_, found := storage.LoadByID(context.Background(), key.ID.String())
	assert.True(t, found, "deleted key should wait for informer-backed refresh before leaving the index")
	require.NoError(t, storage.refresh(context.Background(), storage.APIReader))
	_, found = storage.LoadByID(context.Background(), key.ID.String())
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
	storageWithDeleteError := NewSecretKeyStorage(hookClient, hookClient, nil, "default", "admin-key").(*secretKeyStorage)
	require.Error(t, storageWithDeleteError.DeleteKey(context.Background(), key2))
}

func TestSecretKeyStorage_ListNilUsers(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, storage *secretKeyStorage)
	}{
		{
			name: "nil owner returns nil keys",
			run: func(t *testing.T, storage *secretKeyStorage) {
				keys, err := storage.ListByOwnerTeam(t.Context(), nil)
				require.NoError(t, err)
				assert.Nil(t, keys)
			},
		},
		{
			name: "nil user returns nil teams",
			run: func(t *testing.T, storage *secretKeyStorage) {
				teams, err := storage.ListTeams(t.Context(), nil)
				require.NoError(t, err)
				assert.Nil(t, teams)
			},
		},
		{
			name: "non-admin skips other teams",
			run: func(t *testing.T, storage *secretKeyStorage) {
				storage.storeKey(&models.CreatedTeamAPIKey{
					ID:   uuid.New(),
					Key:  uuid.NewString(),
					Name: "other",
					Team: &models.Team{Name: "team-b"},
				})

				teams, err := storage.ListTeams(t.Context(), &models.CreatedTeamAPIKey{
					ID:   uuid.New(),
					Key:  uuid.NewString(),
					Name: "caller",
					Team: &models.Team{Name: "team-a"},
				})
				require.NoError(t, err)
				assert.Empty(t, teams)
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			storage, _ := newSecretStorageForTest(t, map[string][]byte{})
			tt.run(t, storage)
		})
	}
}

func TestSecretKeyStorage_ListByOwnerMatchesLegacyTeamsByName(t *testing.T) {
	tests := []struct {
		name      string
		teamName  string
		ownerTeam string
		otherTeam string
		wantCount int
	}{
		{
			name:      "same team name with different legacy ids is visible",
			teamName:  "team-a",
			ownerTeam: "11111111-1111-1111-1111-111111111111",
			otherTeam: "22222222-2222-2222-2222-222222222222",
			wantCount: 2,
		},
		{
			name:      "different team name is hidden even with old id data",
			teamName:  "team-a",
			ownerTeam: "11111111-1111-1111-1111-111111111111",
			otherTeam: "22222222-2222-2222-2222-222222222222",
			wantCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ownerID := uuid.New()
			otherTeamName := tt.teamName
			if tt.wantCount == 1 {
				otherTeamName = "team-b"
			}
			otherID := uuid.New()
			storage, _ := newSecretStorageForTest(t, map[string][]byte{
				ownerID.String(): []byte(fmt.Sprintf(`{"id":%q,"key":"owner-key","name":"owner","team":{"id":%q,"name":%q}}`, ownerID.String(), tt.ownerTeam, tt.teamName)),
				otherID.String(): []byte(fmt.Sprintf(`{"id":%q,"key":"other-key","name":"other","team":{"id":%q,"name":%q}}`, otherID.String(), tt.otherTeam, otherTeamName)),
			})
			require.NoError(t, storage.refresh(context.Background(), storage.APIReader))

			ownerKey := &models.CreatedTeamAPIKey{ID: ownerID, Team: &models.Team{Name: tt.teamName}}
			keys, err := storage.ListByOwnerTeam(context.Background(), ownerKey)
			require.NoError(t, err)
			require.Len(t, keys, tt.wantCount)
		})
	}
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
				teamA := &models.Team{Name: "team-a"}
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
				teamA := &models.Team{Name: "team-a"}
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
				teamA := &models.Team{Name: "team-a"}
				teamB := &models.Team{Name: "team-b"}
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
			name: "delete well-known admin key is forbidden",
			run: func(t *testing.T) error {
				storage, _ := newSecretStorageForTest(t, map[string][]byte{})
				admin := &models.CreatedTeamAPIKey{ID: AdminKeyID, Key: "admin", Name: "admin", Team: models.AdminTeam()}
				storage.storeKey(admin)
				return storage.DeleteKey(context.Background(), admin)
			},
			expectError: "well-known admin api-key cannot be deleted",
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
				teamA := &models.Team{Name: "team-a"}
				storage.storeKey(&models.CreatedTeamAPIKey{ID: uuid.New(), Key: uuid.NewString(), Name: "k1", Team: teamA})

				found, ok, err := storage.FindTeamByName(context.Background(), "team-a")
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, teamA, found)
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
				teamA := &models.Team{Name: "team-a"}
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
				teamA := &models.Team{Name: "team-a"}
				key1 := &models.CreatedTeamAPIKey{ID: uuid.New(), Key: uuid.NewString(), Name: "k1", Team: teamA}
				b, err := json.Marshal(key1)
				require.NoError(t, err)

				storage, c := newSecretStorageForTest(t, map[string][]byte{key1.ID.String(): b})
				require.NoError(t, storage.refresh(context.Background(), c))

				// team-a should be findable
				_, ok, _ := storage.FindTeamByName(context.Background(), "team-a")
				require.True(t, ok)

				// Manually insert a stale team
				storage.idxByTeam.Store("stale-team", &models.Team{Name: "stale-team"})

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
				teamA := &models.Team{Name: "team-a"}
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
				teamA := &models.Team{Name: "team-a"}
				storage.storeKey(&models.CreatedTeamAPIKey{ID: uuid.New(), Key: uuid.NewString(), Name: "k1", Team: teamA})
				storage.storeKey(&models.CreatedTeamAPIKey{ID: uuid.New(), Key: uuid.NewString(), Name: "k2", Team: teamA})

				found, ok, err := storage.FindTeamByName(context.Background(), "team-a")
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, teamA, found)
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

func TestSecretKeyStorage_RunStop_RegistersAndRemovesHandler(t *testing.T) {
	storage, _ := newSecretStorageForTest(t, map[string][]byte{})
	stub := newStubCache()
	storage.Cache = stub

	storage.Run()
	require.NotNil(t, stub.informer.currentHandler(), "Run should register an event handler")

	storage.Stop()
	require.True(t, stub.informer.wasRemoved(), "Stop should remove the event handler")
	require.NotPanics(t, func() { storage.Stop() }, "Stop must be idempotent")
}

func TestSecretKeyStorage_RunErrors(t *testing.T) {
	tests := []struct {
		name   string
		stub   func() *stubCache
		verify func(t *testing.T, storage *secretKeyStorage, stub *stubCache)
	}{
		{
			name: "get informer error closes done",
			stub: func() *stubCache {
				stub := newStubCache()
				stub.getInformerErr = errors.New("get informer failed")
				return stub
			},
			verify: func(t *testing.T, storage *secretKeyStorage, _ *stubCache) {
				select {
				case <-storage.done:
				case <-time.After(time.Second):
					t.Fatal("expected done to be closed when GetInformer fails")
				}
			},
		},
		{
			name: "add event handler error closes done",
			stub: func() *stubCache {
				stub := newStubCache()
				stub.informer.addErr = errors.New("add handler failed")
				return stub
			},
			verify: func(t *testing.T, storage *secretKeyStorage, _ *stubCache) {
				select {
				case <-storage.done:
				case <-time.After(time.Second):
					t.Fatal("expected done to be closed when AddEventHandler fails")
				}
			},
		},
		{
			name: "remove event handler error still stops",
			stub: func() *stubCache {
				stub := newStubCache()
				stub.informer.removeErr = errors.New("remove handler failed")
				return stub
			},
			verify: func(t *testing.T, storage *secretKeyStorage, stub *stubCache) {
				storage.Stop()
				assert.True(t, stub.informer.wasRemoved())
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			storage, _ := newSecretStorageForTest(t, map[string][]byte{})
			stub := tt.stub()
			storage.Cache = stub

			storage.Run()
			tt.verify(t, storage, stub)
		})
	}
}

func TestSecretKeyStorage_RefreshWorkerLogsRefreshErrorAndContinues(t *testing.T) {
	storage, c := newSecretStorageForTest(t, map[string][]byte{})
	var getCount int32
	storage.Client = &getHookClient{
		Client: c,
		getHook: func(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			atomic.AddInt32(&getCount, 1)
			return errors.New("refresh failed")
		},
	}

	storage.wg.Add(1)
	go storage.refreshWorker(t.Context())

	storage.triggerRefresh()
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&getCount) > 0
	}, time.Second, 5*time.Millisecond)

	close(storage.stop)
	storage.wg.Wait()
}

func TestSecretKeyStorage_Run_HandlerEventTriggersRefresh(t *testing.T) {
	// Seed a Secret with a key the storage does not yet know about.
	id := uuid.New()
	keyStr := uuid.NewString()
	apiKey := &models.CreatedTeamAPIKey{
		ID: id, Key: keyStr, Name: "watched", Team: models.AdminTeam(), CreatedAt: time.Now(),
	}
	apiKeyJSON, err := json.Marshal(apiKey)
	require.NoError(t, err)

	storage, _ := newSecretStorageForTest(t, map[string][]byte{id.String(): apiKeyJSON})
	stub := newStubCache()
	storage.Cache = stub

	// Reset the in-memory indexes so we can observe the worker re-populating them.
	storage.idxByKey = sync.Map{}
	storage.idxByID = sync.Map{}
	storage.idxByTeam = sync.Map{}
	_, ok := storage.LoadByKey(t.Context(), keyStr)
	require.False(t, ok, "precondition: key should not be in cache before Run")

	storage.Run()
	t.Cleanup(storage.Stop)

	// Drive a synthetic event matching the watched Secret.
	stub.informer.currentHandler().OnUpdate(nil, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: KeySecretName},
	})

	require.Eventually(t, func() bool {
		_, ok := storage.LoadByKey(t.Context(), keyStr)
		return ok
	}, time.Second, 5*time.Millisecond, "expected refreshWorker to repopulate the index")
}

func TestSecretKeyStorage_Run_PeriodicRefreshTriggersRefresh(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "refreshes from cached reader without an event"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			id := uuid.New()
			keyStr := uuid.NewString()
			apiKey := &models.CreatedTeamAPIKey{
				ID: id, Key: keyStr, Name: "periodic", Team: models.AdminTeam(), CreatedAt: time.Now(),
			}
			apiKeyJSON, err := json.Marshal(apiKey)
			require.NoError(t, err)

			storage, _ := newSecretStorageForTest(t, map[string][]byte{id.String(): apiKeyJSON})
			stub := newStubCache()
			storage.Cache = stub
			storage.refreshInterval = 10 * time.Millisecond
			storage.idxByKey = sync.Map{}
			storage.idxByID = sync.Map{}
			storage.idxByTeam = sync.Map{}

			storage.Run()
			t.Cleanup(storage.Stop)

			require.Eventually(t, func() bool {
				_, ok := storage.LoadByKey(t.Context(), keyStr)
				return ok
			}, time.Second, 5*time.Millisecond, "expected periodic refresh to repopulate the index")
		})
	}
}

func TestSecretKeyStorage_CreateKeyWaitsForInformerRefresh(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "pending stale refresh keeps create waiting for informer-owned index"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			apiClient := fake.NewClientBuilder().WithScheme(newTestScheme(t)).WithObjects(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: KeySecretName, Namespace: "default"},
				Data:       map[string][]byte{},
			}).Build()
			cachedClient := &staleSecretReadClient{Client: apiClient}
			cachedClient.SetSecret(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: KeySecretName, Namespace: "default"},
				Data:       map[string][]byte{},
			})
			storage := NewSecretKeyStorage(cachedClient, apiClient, nil, "default", "admin-key").(*secretKeyStorage)
			storage.triggerRefresh()

			newID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
			newKey := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
			useGeneratedUUIDsForTest(t, newID, newKey)
			creator := &models.CreatedTeamAPIKey{
				ID:        uuid.New(),
				Team:      &models.Team{Name: "team-a"},
				CreatedBy: &models.TeamUser{ID: uuid.New()},
			}

			created, err := storage.CreateKey(context.Background(), creator, CreateKeyOptions{Name: "created"})
			require.NoError(t, err)
			require.Equal(t, newID, created.ID)
			require.Equal(t, newKey.String(), created.Key)

			_, found := storage.LoadByID(context.Background(), created.ID.String())
			require.False(t, found, "CreateKey must not update the in-memory index before informer refresh")
			_, found = storage.LoadByKey(context.Background(), created.Key)
			require.False(t, found, "CreateKey must not update the in-memory index before informer refresh")

			storage.wg.Add(1)
			go storage.refreshWorker(t.Context())
			t.Cleanup(func() {
				close(storage.stop)
				storage.wg.Wait()
			})

			require.Eventually(t, func() bool {
				return cachedClient.GetCount() > 0
			}, time.Second, 5*time.Millisecond)
			_, found = storage.LoadByID(context.Background(), created.ID.String())
			require.False(t, found, "stale queued refresh should keep the informer-owned index unchanged")

			cachedClient.ClearSecret()
			storage.triggerRefresh()
			require.Eventually(t, func() bool {
				_, ok := storage.LoadByID(context.Background(), created.ID.String())
				return ok
			}, time.Second, 5*time.Millisecond, "expected fresh informer refresh to publish created key")
			_, found = storage.LoadByKey(context.Background(), created.Key)
			require.True(t, found)
		})
	}
}

func TestSecretKeyStorage_DeleteKeyWaitsForInformerRefresh(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "pending stale refresh keeps delete waiting for informer-owned index"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			apiKey := &models.CreatedTeamAPIKey{
				ID:   uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
				Key:  "key-to-delete",
				Name: "delete-me",
				Team: &models.Team{Name: "team-a"},
			}
			apiKeyJSON, err := json.Marshal(apiKey)
			require.NoError(t, err)
			staleSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: KeySecretName, Namespace: "default"},
				Data:       map[string][]byte{apiKey.ID.String(): apiKeyJSON},
			}
			apiClient := fake.NewClientBuilder().WithScheme(newTestScheme(t)).WithObjects(staleSecret.DeepCopy()).Build()
			cachedClient := &staleSecretReadClient{Client: apiClient}
			cachedClient.SetSecret(staleSecret)
			storage := NewSecretKeyStorage(cachedClient, apiClient, nil, "default", "admin-key").(*secretKeyStorage)
			storage.storeKey(apiKey)
			storage.triggerRefresh()

			require.NoError(t, storage.DeleteKey(context.Background(), apiKey))

			_, found := storage.LoadByID(context.Background(), apiKey.ID.String())
			require.True(t, found, "DeleteKey must not update the in-memory index before informer refresh")
			_, found = storage.LoadByKey(context.Background(), apiKey.Key)
			require.True(t, found, "DeleteKey must not update the in-memory index before informer refresh")

			storage.wg.Add(1)
			go storage.refreshWorker(t.Context())
			t.Cleanup(func() {
				close(storage.stop)
				storage.wg.Wait()
			})

			require.Eventually(t, func() bool {
				return cachedClient.GetCount() > 0
			}, time.Second, 5*time.Millisecond)
			_, found = storage.LoadByID(context.Background(), apiKey.ID.String())
			require.True(t, found, "stale queued refresh should keep the informer-owned index unchanged")

			cachedClient.ClearSecret()
			storage.triggerRefresh()
			require.Eventually(t, func() bool {
				_, ok := storage.LoadByID(context.Background(), apiKey.ID.String())
				return !ok
			}, time.Second, 5*time.Millisecond, "expected fresh informer refresh to remove deleted key")
			_, found = storage.LoadByKey(context.Background(), apiKey.Key)
			require.False(t, found)
		})
	}
}

func TestSecretKeyStorage_Run_FilteredEventDoesNotTriggerRefresh(t *testing.T) {
	storage, _ := newSecretStorageForTest(t, map[string][]byte{})
	stub := newStubCache()
	storage.Cache = stub

	storage.Run()
	t.Cleanup(storage.Stop)

	// Event for an unrelated Secret should be filtered out — refreshC stays empty.
	stub.informer.currentHandler().OnUpdate(nil, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "unrelated"},
	})

	select {
	case <-storage.refreshC:
		t.Fatal("did not expect a refresh signal for unrelated Secret")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestSecretKeyStorage_TriggerRefresh_Coalesces(t *testing.T) {
	s := &secretKeyStorage{refreshC: make(chan struct{}, 1)}

	s.triggerRefresh()
	s.triggerRefresh()
	s.triggerRefresh()

	select {
	case <-s.refreshC:
	default:
		t.Fatal("expected at least one signal in refreshC")
	}
	select {
	case <-s.refreshC:
		t.Fatal("expected coalescing; multiple signals queued")
	default:
	}
}

func TestSecretKeyStorage_OnSecretEvent_Match(t *testing.T) {
	s := &secretKeyStorage{Namespace: "default", refreshC: make(chan struct{}, 1)}

	s.onSecretEvent(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default",
		Name:      KeySecretName,
	}})

	select {
	case <-s.refreshC:
	default:
		t.Fatal("expected refresh signal for matching Secret")
	}
}

func TestSecretKeyStorage_OnSecretEvent_Mismatch(t *testing.T) {
	cases := []struct {
		name      string
		namespace string
		objName   string
	}{
		{"wrong namespace", "other-ns", KeySecretName},
		{"wrong name", "default", "other-secret"},
		{"both wrong", "other-ns", "other-secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &secretKeyStorage{Namespace: "default", refreshC: make(chan struct{}, 1)}

			s.onSecretEvent(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Namespace: tc.namespace,
				Name:      tc.objName,
			}})

			select {
			case <-s.refreshC:
				t.Fatal("did not expect a refresh signal for non-matching Secret")
			default:
			}
		})
	}
}

func TestSecretKeyStorage_OnSecretEvent_Tombstone(t *testing.T) {
	s := &secretKeyStorage{Namespace: "default", refreshC: make(chan struct{}, 1)}

	s.onSecretEvent(toolscache.DeletedFinalStateUnknown{
		Key: "default/" + KeySecretName,
		Obj: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      KeySecretName,
		}},
	})

	select {
	case <-s.refreshC:
	default:
		t.Fatal("expected refresh signal from tombstone")
	}
}

func TestSecretKeyStorage_OnSecretEvent_UnknownType(t *testing.T) {
	s := &secretKeyStorage{Namespace: "default", refreshC: make(chan struct{}, 1)}

	s.onSecretEvent("not a secret")

	select {
	case <-s.refreshC:
		t.Fatal("did not expect a refresh signal for non-Secret object")
	default:
	}
}
