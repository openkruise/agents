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

package e2b

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

type lookupKeyStorage struct {
	byKey map[string]*models.CreatedTeamAPIKey
	calls []string
}

func (s *lookupKeyStorage) Init(context.Context) error { return nil }
func (s *lookupKeyStorage) Run()                       {}
func (s *lookupKeyStorage) Stop()                      {}

func (s *lookupKeyStorage) LoadByKey(_ context.Context, key string) (*models.CreatedTeamAPIKey, bool) {
	s.calls = append(s.calls, key)
	user, ok := s.byKey[key]
	return user, ok
}

func (s *lookupKeyStorage) LoadByID(_ context.Context, id string) (*models.CreatedTeamAPIKey, bool) {
	for _, user := range s.byKey {
		if user.ID.String() == id {
			return user, true
		}
	}
	return nil, false
}

func (s *lookupKeyStorage) CreateKey(context.Context, *models.CreatedTeamAPIKey, keys.CreateKeyOptions) (*models.CreatedTeamAPIKey, error) {
	return nil, nil
}

func (s *lookupKeyStorage) DeleteKey(context.Context, *models.CreatedTeamAPIKey) error {
	return nil
}

func (s *lookupKeyStorage) ListByOwnerTeam(context.Context, *models.CreatedTeamAPIKey) ([]*models.TeamAPIKey, error) {
	return nil, nil
}

func (s *lookupKeyStorage) ListTeams(context.Context, *models.CreatedTeamAPIKey) ([]*models.ListedTeam, error) {
	return nil, nil
}

func (s *lookupKeyStorage) FindTeamByName(context.Context, string) (*models.Team, bool, error) {
	return nil, false, nil
}

// TestCheckApiKey_BasicTests tests basic CheckApiKey middleware functionality
// Note: The "keys nil (auth disabled)" scenario is tested separately
// to avoid peer initialization timeout issues. See TestCheckApiKey_AnonymousUserWithAdminKeyID
// for AnonymousUser validation.

// TestCheckApiKey_WithRealSetup tests CheckApiKey with full Setup
func TestCheckApiKey_WithRealSetup(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	// The Setup creates admin key with InitKey
	adminUser := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}

	// Create a regular user key using CreateKey API
	ctx := logs.NewContext()
	regularUser, err := controller.keys.CreateKey(ctx, adminUser, keys.CreateKeyOptions{Name: "regular-user", TeamName: "regular-team"})
	require.NoError(t, err)
	require.NotNil(t, regularUser)

	tests := []struct {
		name          string
		apiKeyHeader  string
		expectError   bool
		expectedCode  int
		expectedMsg   string
		expectCtxUser bool
		expectedUser  *models.CreatedTeamAPIKey
	}{
		{
			name:          "valid admin API key",
			apiKeyHeader:  InitKey,
			expectError:   false,
			expectCtxUser: true,
			expectedUser:  adminUser,
		},
		{
			name:          "valid regular user API key",
			apiKeyHeader:  regularUser.Key,
			expectError:   false,
			expectCtxUser: true,
			expectedUser:  regularUser,
		},
		{
			name:          "valid E2B SDK compatible regular user API key",
			apiKeyHeader:  keys.EncodeForE2BSDK(regularUser.Key),
			expectError:   false,
			expectCtxUser: true,
			expectedUser:  regularUser,
		},
		{
			name:         "invalid API key",
			apiKeyHeader: "invalid-key",
			expectError:  true,
			expectedCode: http.StatusUnauthorized,
			expectedMsg:  "Invalid API Key",
		},
		{
			name:         "empty X-API-KEY header",
			apiKeyHeader: "",
			expectError:  true,
			expectedCode: http.StatusUnauthorized,
			expectedMsg:  "Invalid API Key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "http://localhost/test", nil)
			require.NoError(t, err)

			if tt.apiKeyHeader != "" {
				req.Header.Set(models.HeaderApiKey, tt.apiKeyHeader)
			}

			ctx := logs.NewContext()
			newCtx, apiErr := controller.CheckApiKey(ctx, req)

			if tt.expectError {
				assert.NotNil(t, apiErr)
				if apiErr != nil {
					assert.Equal(t, tt.expectedCode, apiErr.Code)
					assert.Equal(t, tt.expectedMsg, apiErr.Message)
					if tt.apiKeyHeader != "" {
						assert.NotContains(t, apiErr.Message, tt.apiKeyHeader)
						if decoded, ok := keys.DecodeFromE2BSDKCompatible(tt.apiKeyHeader); ok {
							assert.NotContains(t, apiErr.Message, decoded)
						}
					}
				}
			} else {
				assert.Nil(t, apiErr)
				if tt.expectCtxUser {
					user := GetUserFromContext(newCtx)
					assert.NotNil(t, user)
					if user != nil && tt.expectedUser != nil {
						assert.Equal(t, tt.expectedUser.ID, user.ID)
					}
				}
			}
		})
	}
}

func TestCheckApiKey_CanonicalizesE2BSDKCompatibleKeys(t *testing.T) {
	rawKey := "legacy-raw-key"
	rawUser := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  rawKey,
		Name: "legacy-user",
		Team: &models.Team{Name: "team-a"},
	}
	encodedRawKey := keys.EncodeForE2BSDK(rawKey)

	e2bLikeRawKey := "e2b_000000000000000"
	e2bLikeUser := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  e2bLikeRawKey,
		Name: "e2b-like-user",
		Team: &models.Team{Name: "team-a"},
	}
	encodedE2bLikeKey := keys.EncodeForE2BSDK(e2bLikeRawKey)

	tests := []struct {
		name      string
		stored    map[string]*models.CreatedTeamAPIKey
		header    string
		wantUser  *models.CreatedTeamAPIKey
		wantCalls []string
	}{
		{
			name:      "raw key authenticates directly",
			stored:    map[string]*models.CreatedTeamAPIKey{rawKey: rawUser},
			header:    rawKey,
			wantUser:  rawUser,
			wantCalls: []string{rawKey},
		},
		{
			name:      "compatible key authenticates as decoded raw key",
			stored:    map[string]*models.CreatedTeamAPIKey{rawKey: rawUser},
			header:    encodedRawKey,
			wantUser:  rawUser,
			wantCalls: []string{rawKey},
		},
		{
			name:      "E2B-like raw key authenticates directly",
			stored:    map[string]*models.CreatedTeamAPIKey{e2bLikeRawKey: e2bLikeUser},
			header:    e2bLikeRawKey,
			wantUser:  e2bLikeUser,
			wantCalls: []string{e2bLikeRawKey},
		},
		{
			name:      "E2B-like key authenticates as decoded raw key",
			stored:    map[string]*models.CreatedTeamAPIKey{e2bLikeRawKey: e2bLikeUser},
			header:    encodedE2bLikeKey,
			wantUser:  e2bLikeUser,
			wantCalls: []string{e2bLikeRawKey},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storage := &lookupKeyStorage{byKey: tt.stored}
			controller := &Controller{keys: storage}
			req, err := http.NewRequest(http.MethodGet, "http://localhost/test", nil)
			require.NoError(t, err)
			req.Header.Set(models.HeaderApiKey, tt.header)

			newCtx, apiErr := controller.CheckApiKey(logs.NewContext(), req)

			require.Nil(t, apiErr)
			user := GetUserFromContext(newCtx)
			require.NotNil(t, user)
			assert.Equal(t, tt.wantUser.ID, user.ID)
			assert.Equal(t, tt.wantCalls, storage.calls)
		})
	}
}

// TestCheckApiKey_SandboxOwnership tests CheckApiKey with sandbox ownership validation
func TestCheckApiKey_SandboxOwnership(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	templateName := "test-template-auth"

	// Create admin user
	adminUser := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
		Team: models.AdminTeam(),
	}

	// Create a regular user
	ctx := logs.NewContext()
	regularUser, err := controller.keys.CreateKey(ctx, adminUser, keys.CreateKeyOptions{Name: "regular-user", TeamName: "regular-team"})
	require.NoError(t, err)
	require.NotNil(t, regularUser)

	// Create another user for non-owner test
	anotherUser, err := controller.keys.CreateKey(ctx, adminUser, keys.CreateKeyOptions{Name: "another-user", TeamName: "another-team"})
	require.NoError(t, err)
	require.NotNil(t, anotherUser)

	adminCleanup := CreateSandboxPool(t, controller, templateName, 2)
	defer adminCleanup()
	regularCleanup := CreateSandboxPool(t, controller, templateName, 2, CreateSandboxPoolOptions{Namespace: regularUser.Team.Name})
	defer regularCleanup()

	// Create sandbox owned by regular user
	createResp, apiErr := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
		TemplateID: templateName,
		Metadata: map[string]string{
			models.ExtensionKeySkipInitRuntime: "true",
		},
	}, nil, regularUser))
	require.Nil(t, apiErr)
	require.NotNil(t, createResp)
	sandboxID := createResp.Body.SandboxID

	// Create sandbox owned by admin user
	adminCreateResp, apiErr := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
		TemplateID: templateName,
		Metadata: map[string]string{
			models.ExtensionKeySkipInitRuntime: "true",
		},
	}, nil, adminUser))
	require.Nil(t, apiErr)
	require.NotNil(t, adminCreateResp)
	adminSandboxID := adminCreateResp.Body.SandboxID

	// Wait for sandbox to be ready
	time.Sleep(100 * time.Millisecond)

	tests := []struct {
		name         string
		apiKeyHeader string
		sandboxID    string
		expectError  bool
		expectedCode int
		expectedMsg  string
	}{
		{
			name:         "owner can access own sandbox",
			apiKeyHeader: regularUser.Key,
			sandboxID:    sandboxID,
			expectError:  false,
		},
		{
			name:         "admin can access admin-owned sandbox",
			apiKeyHeader: InitKey,
			sandboxID:    adminSandboxID,
			expectError:  false,
		},
		{
			name:         "non-owner cannot access sandbox",
			apiKeyHeader: anotherUser.Key,
			sandboxID:    sandboxID,
			expectError:  true,
			expectedCode: http.StatusUnauthorized,
			expectedMsg:  "The user of API key is not the owner of sandbox: " + sandboxID,
		},
		{
			name:         "admin cannot access other user's sandbox",
			apiKeyHeader: InitKey,
			sandboxID:    sandboxID,
			expectError:  true,
			expectedCode: http.StatusUnauthorized,
			expectedMsg:  "The user of API key is not the owner of sandbox: " + sandboxID,
		},
		{
			name:         "sandbox not found",
			apiKeyHeader: InitKey,
			sandboxID:    "non-existent-sandbox",
			expectError:  true,
			expectedCode: http.StatusNotFound,
			expectedMsg:  "Sandbox route not found, maybe it is crashed or killed: non-existent-sandbox",
		},
		{
			name:         "no sandboxID in path - success",
			apiKeyHeader: regularUser.Key,
			sandboxID:    "",
			expectError:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "http://localhost/test", nil)
			require.NoError(t, err)

			if tt.apiKeyHeader != "" {
				req.Header.Set(models.HeaderApiKey, tt.apiKeyHeader)
			}

			if tt.sandboxID != "" {
				req.SetPathValue("sandboxID", tt.sandboxID)
			}

			ctx := logs.NewContext()
			_, apiErr := controller.CheckApiKey(ctx, req)

			if tt.expectError {
				assert.NotNil(t, apiErr)
				if apiErr != nil {
					assert.Equal(t, tt.expectedCode, apiErr.Code)
					assert.Equal(t, tt.expectedMsg, apiErr.Message)
				}
			} else {
				assert.Nil(t, apiErr)
			}
		})
	}
}

// TestCheckApiKey_AnonymousUserWithAdminKeyID tests that AnonymousUser has AdminKeyID
func TestCheckApiKey_AnonymousUserWithAdminKeyID(t *testing.T) {
	// Verify AnonymousUser has AdminKeyID - this allows admin to access any sandbox
	assert.Equal(t, keys.AdminKeyID, AnonymousUser.ID, "AnonymousUser should have AdminKeyID")
	assert.Equal(t, "auth-disabled", AnonymousUser.Name, "AnonymousUser should have auth-disabled name")
	assert.Equal(t, models.AdminTeam(), AnonymousUser.Team, "AnonymousUser should carry canonical admin team")
}

// TestGetUserFromContext tests the GetUserFromContext helper function
func TestGetUserFromContext(t *testing.T) {
	tests := []struct {
		name       string
		ctxValue   any
		expectNil  bool
		expectedID uuid.UUID
	}{
		{
			name:       "valid user",
			ctxValue:   &models.CreatedTeamAPIKey{ID: keys.AdminKeyID, Name: "admin"},
			expectNil:  false,
			expectedID: keys.AdminKeyID,
		},
		{
			name:      "nil value",
			ctxValue:  nil,
			expectNil: true,
		},
		{
			name:      "wrong type - string",
			ctxValue:  "user",
			expectNil: true,
		},
		{
			name:      "wrong type - int",
			ctxValue:  123,
			expectNil: true,
		},
		{
			name:      "wrong type - map",
			ctxValue:  map[string]string{"id": "test"},
			expectNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.ctxValue != nil {
				ctx = context.WithValue(ctx, "user", tt.ctxValue)
			}

			user := GetUserFromContext(ctx)

			if tt.expectNil {
				assert.Nil(t, user)
			} else {
				assert.NotNil(t, user)
				if user != nil {
					assert.Equal(t, tt.expectedID, user.ID)
				}
			}
		})
	}
}

// TestValidateTeamNamespace_RejectsDoubleDash verifies the API key creation guard rejects
// namespace names containing the sandbox ID separator before consulting Kubernetes.
func TestValidateTeamNamespace_RejectsDoubleDash(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()

	// Create a real Kubernetes namespace whose name happens to contain "--" so we can
	// prove the rejection comes from the validator, not from "namespace not found".
	require.NoError(t, fc.Create(t.Context(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "team--blue"},
	}))
	require.NoError(t, fc.Create(t.Context(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
	}))

	tests := []struct {
		name      string
		teamName  string
		expectErr bool
		wantCode  int
		wantMsg   string
	}{
		{name: "valid namespace passes", teamName: "team-a", expectErr: false},
		{name: "double-dash rejected even when namespace exists", teamName: "team--blue", expectErr: true, wantCode: http.StatusBadRequest, wantMsg: "must not contain"},
		{name: "double-dash at start", teamName: "--prefix", expectErr: true, wantCode: http.StatusBadRequest, wantMsg: "must not contain"},
		{name: "missing namespace returns 400 too but for different reason", teamName: "no-such-ns", expectErr: true, wantCode: http.StatusBadRequest, wantMsg: "does not exist"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiErr := controller.validateTeamNamespace(t.Context(), tt.teamName)
			if !tt.expectErr {
				assert.Nil(t, apiErr)
				return
			}
			require.NotNil(t, apiErr)
			assert.Equal(t, tt.wantCode, apiErr.Code)
			assert.Contains(t, apiErr.Message, tt.wantMsg)
		})
	}
}
