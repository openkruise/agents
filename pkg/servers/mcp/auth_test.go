/*
Copyright 2026 The Kruise Authors.

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

package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/stretchr/testify/assert"
)

// mockKeyStorage provides a simple mock for SecretKeyStorage
type mockKeyStorage struct {
	keys map[string]*models.CreatedTeamAPIKey
}

func newMockKeyStorage() *mockKeyStorage {
	return &mockKeyStorage{
		keys: make(map[string]*models.CreatedTeamAPIKey),
	}
}

func (m *mockKeyStorage) addKey(key string, user *models.CreatedTeamAPIKey) {
	m.keys[key] = user
}

func (m *mockKeyStorage) LoadByKey(key string) (*models.CreatedTeamAPIKey, bool) {
	user, ok := m.keys[key]
	return user, ok
}

func TestNewAuth(t *testing.T) {
	t.Run("with key storage", func(t *testing.T) {
		storage := &keys.SecretKeyStorage{}
		auth := NewAuth(storage)

		assert.NotNil(t, auth)
		assert.Equal(t, storage, auth.keys)
	})

	t.Run("with nil key storage", func(t *testing.T) {
		auth := NewAuth(nil)

		assert.NotNil(t, auth)
		assert.Nil(t, auth.keys)
	})
}

func TestAuth_ValidateAPIKey(t *testing.T) {
	t.Run("auth disabled (nil keys) returns anonymous user", func(t *testing.T) {
		auth := &Auth{keys: nil}
		ctx := context.Background()

		user, err := auth.ValidateAPIKey(ctx, "any-key")

		assert.NoError(t, err)
		assert.NotNil(t, user)
		assert.Equal(t, keys.AdminKeyID, user.ID)
		assert.Equal(t, "auth-disabled", user.Name)
	})

	t.Run("auth disabled with empty key returns anonymous user", func(t *testing.T) {
		auth := &Auth{keys: nil}
		ctx := context.Background()

		user, err := auth.ValidateAPIKey(ctx, "")

		assert.NoError(t, err)
		assert.NotNil(t, user)
		assert.Equal(t, "auth-disabled", user.Name)
	})
}

func TestAuth_HTTPAuthMiddleware(t *testing.T) {
	t.Run("auth disabled passes through", func(t *testing.T) {
		auth := &Auth{keys: nil}
		called := false
		nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			// Verify user is set in context
			user, err := GetUserFromContext(r.Context())
			assert.NoError(t, err)
			assert.NotNil(t, user)
			assert.Equal(t, "auth-disabled", user.Name)
			w.WriteHeader(http.StatusOK)
		})

		handler := auth.HTTPAuthMiddleware(nextHandler)
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.True(t, called)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("valid API key passes through", func(t *testing.T) {
		mockStorage := newMockKeyStorage()
		testUser := &models.CreatedTeamAPIKey{
			ID:   uuid.New(),
			Name: "test-user",
			Key:  "valid-key",
		}
		mockStorage.addKey("valid-key", testUser)

		// Create auth with mock storage (now works because Auth.keys is KeyStorage interface)
		auth := &Auth{keys: mockStorage}

		called := false
		nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			// Verify user is set in context
			user, err := GetUserFromContext(r.Context())
			assert.NoError(t, err)
			assert.Equal(t, testUser.ID, user.ID)
			assert.Equal(t, "test-user", user.Name)
			w.WriteHeader(http.StatusOK)
		})

		handler := auth.HTTPAuthMiddleware(nextHandler)
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-API-KEY", "valid-key")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.True(t, called)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("invalid API key returns 401", func(t *testing.T) {
		mockStorage := newMockKeyStorage()
		// Storage is empty, no valid keys
		auth := &Auth{keys: mockStorage}

		called := false
		nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		})

		handler := auth.HTTPAuthMiddleware(nextHandler)
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-API-KEY", "invalid-key")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.False(t, called)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	})

	t.Run("missing API key with auth enabled returns 401", func(t *testing.T) {
		mockStorage := newMockKeyStorage()
		auth := &Auth{keys: mockStorage}

		called := false
		nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		})

		handler := auth.HTTPAuthMiddleware(nextHandler)
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		// No X-API-KEY header
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		assert.False(t, called)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Contains(t, rec.Body.String(), "Unauthorized")
	})
}
