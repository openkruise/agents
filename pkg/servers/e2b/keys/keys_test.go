package keys

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSecretKeyStorage_Init(t *testing.T) {
	existId := uuid.New()
	existKey := models.CreatedTeamAPIKey{
		ID:   existId,
		Key:  "GG",
		Name: "admin",
	}
	existData, _ := json.Marshal(existKey)
	tests := []struct {
		name           string
		existingSecret *corev1.Secret
		adminKey       string
		expectError    bool
	}{
		{
			name:           "create new secret",
			existingSecret: nil,
			expectError:    false,
			adminKey:       "GG",
		},
		{
			name: "secret already exists",
			existingSecret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name: KeySecretName,
				},
				Data: map[string][]byte{
					existId.String(): existData,
				},
			},
			adminKey:    "GG",
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset()
			if tt.existingSecret != nil {
				_, err := client.CoreV1().Secrets("default").Create(context.Background(), tt.existingSecret, metav1.CreateOptions{})
				require.NoError(t, err)
			}

			storage := &SecretKeyStorage{
				Namespace: "default",
				AdminKey:  tt.adminKey,
				Client:    client,
				Stop:      make(chan struct{}),
			}

			err := storage.Init(context.Background())
			if tt.expectError {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)

			// Check if secret exists
			secret, err := client.CoreV1().Secrets("default").Get(context.Background(), KeySecretName, metav1.GetOptions{})
			assert.NoError(t, err)
			assert.NotNil(t, secret)
			assert.Equal(t, len(secret.Data), 1)
			for k, v := range secret.Data {
				var apiKey models.CreatedTeamAPIKey
				err := json.Unmarshal(v, &apiKey)
				assert.NoError(t, err)
				assert.Equal(t, apiKey.ID.String(), k)
				assert.Equal(t, apiKey.Key, tt.adminKey)
				assert.Equal(t, apiKey.Name, "admin")
			}
		})
	}
}

func TestSecretKeyStorage_LoadByKey(t *testing.T) {
	// Setup
	testKey := uuid.New().String()
	testID := uuid.New()
	testAPIKey := &models.CreatedTeamAPIKey{
		CreatedAt: time.Now(),
		ID:        testID,
		Key:       testKey,
		Name:      "test-key",
		CreatedBy: &models.TeamUser{
			ID: uuid.New(),
		},
	}

	client := fake.NewClientset()
	storage := &SecretKeyStorage{
		Client:    client,
		Namespace: "default",
		Stop:      make(chan struct{}),
	}

	// Manually store the key in the storage
	storage.storeKey(testAPIKey)

	tests := []struct {
		name      string
		key       string
		wantKey   *models.CreatedTeamAPIKey
		wantFound bool
	}{
		{
			name:      "key exists",
			key:       testKey,
			wantKey:   testAPIKey,
			wantFound: true,
		},
		{
			name:      "key does not exist",
			key:       "non-existent-key",
			wantKey:   nil,
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotKey, gotFound := storage.LoadByKey(tt.key)
			if tt.wantFound {
				assert.True(t, gotFound)
				assert.Equal(t, tt.wantKey.ID, gotKey.ID)
				assert.Equal(t, tt.wantKey.Key, gotKey.Key)
				assert.Equal(t, tt.wantKey.Name, gotKey.Name)
			} else {
				assert.False(t, gotFound)
				assert.Nil(t, gotKey)
			}
		})
	}
}

func TestSecretKeyStorage_LoadByID(t *testing.T) {
	// Setup
	testKey := uuid.New().String()
	testID := uuid.New()
	testAPIKey := &models.CreatedTeamAPIKey{
		CreatedAt: time.Now(),
		ID:        testID,
		Key:       testKey,
		Name:      "test-key",
		CreatedBy: &models.TeamUser{
			ID: uuid.New(),
		},
	}

	client := fake.NewClientset()
	storage := &SecretKeyStorage{
		Client:    client,
		Namespace: "default",
		Stop:      make(chan struct{}),
	}

	// Manually store the key in the storage
	storage.storeKey(testAPIKey)

	tests := []struct {
		name      string
		id        string
		wantKey   *models.CreatedTeamAPIKey
		wantFound bool
	}{
		{
			name:      "id exists",
			id:        testID.String(),
			wantKey:   testAPIKey,
			wantFound: true,
		},
		{
			name:      "id does not exist",
			id:        uuid.New().String(),
			wantKey:   nil,
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotKey, gotFound := storage.LoadByID(tt.id)
			if tt.wantFound {
				assert.True(t, gotFound)
				assert.NotNil(t, gotKey)
				if gotKey != nil {
					assert.Equal(t, tt.wantKey.ID, gotKey.ID)
					assert.Equal(t, tt.wantKey.Key, gotKey.Key)
					assert.Equal(t, tt.wantKey.Name, gotKey.Name)
				}
			} else {
				assert.False(t, gotFound)
				assert.Nil(t, gotKey)
			}
		})
	}
}

func TestSecretKeyStorage_CreateKey(t *testing.T) {
	client := fake.NewClientset()

	// Create initial secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: KeySecretName,
		},
		Data: map[string][]byte{},
	}
	_, err := client.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	storage := &SecretKeyStorage{
		Client:    client,
		Namespace: "default",
		Stop:      make(chan struct{}),
	}

	user := &models.CreatedTeamAPIKey{
		ID: uuid.New(),
		CreatedBy: &models.TeamUser{
			ID: uuid.New(),
		},
	}

	tests := []struct {
		name        string
		user        *models.CreatedTeamAPIKey
		keyName     string
		expectError bool
	}{
		{
			name:        "valid key creation",
			user:        user,
			keyName:     "test-key",
			expectError: false,
		},
		{
			name:        "empty name",
			user:        user,
			keyName:     "",
			expectError: true,
		},
		{
			name:        "nil user",
			user:        nil,
			keyName:     "test-key",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := storage.CreateKey(context.Background(), tt.user, tt.keyName)

			if tt.expectError {
				assert.Error(t, err)
				assert.Nil(t, key)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, key)
				assert.Equal(t, tt.keyName, key.Name)
				assert.NotEmpty(t, key.Key)
				assert.NotEmpty(t, key.ID)

				// Check if key is stored in memory
				loadedByKey, found := storage.LoadByKey(key.Key)
				assert.True(t, found)
				assert.Equal(t, key.ID, loadedByKey.ID)

				loadedByID, found := storage.LoadByID(key.ID.String())
				assert.True(t, found)
				assert.Equal(t, key.Key, loadedByID.Key)

				// Check if key is stored in secret
				updatedSecret, err := client.CoreV1().Secrets("default").Get(context.Background(), KeySecretName, metav1.GetOptions{})
				assert.NoError(t, err)
				assert.Contains(t, updatedSecret.Data, key.ID.String())

				var storedKey models.CreatedTeamAPIKey
				err = json.Unmarshal(updatedSecret.Data[key.ID.String()], &storedKey)
				assert.NoError(t, err)
				assert.Equal(t, key.Key, storedKey.Key)
			}
		})
	}
}

func TestSecretKeyStorage_DeleteKey(t *testing.T) {
	client := fake.NewClientset()

	// Create initial secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: KeySecretName,
		},
		Data: map[string][]byte{},
	}
	_, err := client.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	storage := &SecretKeyStorage{
		Client:    client,
		Namespace: "default",
		Stop:      make(chan struct{}),
	}

	// Create a key to delete
	user := &models.CreatedTeamAPIKey{
		ID: uuid.New(),
		CreatedBy: &models.TeamUser{
			ID: uuid.New(),
		},
	}

	createdKey, err := storage.CreateKey(context.Background(), user, "test-key")
	require.NoError(t, err)
	require.NotNil(t, createdKey)

	tests := []struct {
		name        string
		key         *models.CreatedTeamAPIKey
		expectError bool
	}{
		{
			name:        "valid key deletion",
			key:         createdKey,
			expectError: false,
		},
		{
			name:        "nil key",
			key:         nil,
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := storage.DeleteKey(context.Background(), tt.key)

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)

				if tt.key != nil {
					// Check if key is removed from memory
					_, found := storage.LoadByKey(tt.key.Key)
					assert.False(t, found)

					_, found = storage.LoadByID(tt.key.ID.String())
					assert.False(t, found)

					// Check if key is removed from secret
					updatedSecret, err := client.CoreV1().Secrets("default").Get(context.Background(), KeySecretName, metav1.GetOptions{})
					assert.NoError(t, err)
					assert.NotContains(t, updatedSecret.Data, tt.key.ID.String())
				}
			}
		})
	}
}

func TestSecretKeyStorage_ListByOwner(t *testing.T) {
	client := fake.NewClientset()

	// Create initial secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: KeySecretName,
		},
		Data: map[string][]byte{},
	}
	_, err := client.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	storage := &SecretKeyStorage{
		Client:    client,
		Namespace: "default",
		Stop:      make(chan struct{}),
	}

	// Create test data
	ownerID := uuid.New()
	otherUserID := uuid.New()

	user := &models.CreatedTeamAPIKey{
		ID: ownerID,
		CreatedBy: &models.TeamUser{
			ID: ownerID,
		},
	}

	otherUser := &models.CreatedTeamAPIKey{
		ID: otherUserID,
		CreatedBy: &models.TeamUser{
			ID: otherUserID,
		},
	}

	// Create keys
	ownerKey1, err := storage.CreateKey(context.Background(), user, "owner-key-1")
	require.NoError(t, err)

	ownerKey2, err := storage.CreateKey(context.Background(), user, "owner-key-2")
	require.NoError(t, err)

	otherKey, err := storage.CreateKey(context.Background(), otherUser, "other-key")
	require.NoError(t, err)

	// Make otherKey owned by ownerID
	otherKey.CreatedBy = &models.TeamUser{ID: ownerID}
	storage.storeKey(otherKey)
	storage.storeKey(ownerKey1)
	storage.storeKey(ownerKey2)

	tests := []struct {
		name     string
		owner    uuid.UUID
		expected int
	}{
		{
			name:     "list owner keys",
			owner:    ownerID,
			expected: 3,
		},
		{
			name:     "list other user keys",
			owner:    otherUserID,
			expected: 0,
		},
		{
			name:     "list non-existent owner keys",
			owner:    uuid.New(),
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keys := storage.ListByOwner(tt.owner)
			assert.Len(t, keys, tt.expected)

			if tt.expected > 0 {
				for _, key := range keys {
					// Key should be owned by the owner or created by the owner
					assert.True(t, key.ID == tt.owner || (key.CreatedBy != nil && key.CreatedBy.ID == tt.owner))
				}
			}
		})
	}
}

func TestSecretKeyStorage_refresh(t *testing.T) {
	client := fake.NewClientset()
	storage := &SecretKeyStorage{
		Client:    client,
		Namespace: "default",
		Stop:      make(chan struct{}),
	}

	// Create test data
	testKey := uuid.New().String()
	testID := uuid.New()
	testAPIKey := &models.CreatedTeamAPIKey{
		CreatedAt: time.Now(),
		ID:        testID,
		Key:       testKey,
		Name:      "test-key",
		CreatedBy: &models.TeamUser{
			ID: uuid.New(),
		},
	}

	// Create secret with test data
	secretData, err := json.Marshal(testAPIKey)
	require.NoError(t, err)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: KeySecretName,
		},
		Data: map[string][]byte{
			testID.String(): secretData,
		},
	}
	_, err = client.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	// Add some garbage data to test error handling
	secret.Data["invalid-id"] = []byte("invalid-json")
	_, err = client.CoreV1().Secrets("default").Update(context.Background(), secret, metav1.UpdateOptions{})
	require.NoError(t, err)

	// Add a key to memory that should be removed after refresh
	storage.storeKey(&models.CreatedTeamAPIKey{
		ID:  uuid.New(),
		Key: "stale-key",
	})

	err = storage.refresh(context.Background())
	assert.NoError(t, err)

	// Check that valid key is loaded
	loadedKey, found := storage.LoadByKey(testKey)
	assert.True(t, found)
	assert.Equal(t, testID, loadedKey.ID)
	assert.Equal(t, testKey, loadedKey.Key)

	// Check that stale key is removed
	_, found = storage.LoadByKey("stale-key")
	assert.False(t, found)

	// Check that invalid key is not loaded
	_, found = storage.LoadByID("invalid-id")
	assert.False(t, found)
}
