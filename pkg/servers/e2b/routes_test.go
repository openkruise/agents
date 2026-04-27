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

	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

// TestCheckAdminKey tests the CheckAdminKey middleware
func TestCheckAdminKey(t *testing.T) {
	tests := []struct {
		name          string
		ctxUser       any // value to store in context with "user" key
		expectError   bool
		expectedCode  int
		expectedMsg   string
		expectCtxUser bool // whether user should be retrievable from context after middleware
	}{
		{
			name:          "valid admin user",
			ctxUser:       &models.CreatedTeamAPIKey{ID: keys.AdminKeyID, Name: "admin"},
			expectError:   false,
			expectCtxUser: true,
		},
		{
			name:         "non-admin user",
			ctxUser:      &models.CreatedTeamAPIKey{ID: uuid.New(), Name: "regular-user"},
			expectError:  true,
			expectedCode: http.StatusForbidden,
			expectedMsg:  "User is not admin",
		},
		{
			name:         "no user in context",
			ctxUser:      nil,
			expectError:  true,
			expectedCode: http.StatusUnauthorized,
			expectedMsg:  "User not found",
		},
		{
			name:         "invalid user object type (string)",
			ctxUser:      "user",
			expectError:  true,
			expectedCode: http.StatusUnauthorized,
			expectedMsg:  "User not found",
		},
		{
			name:         "invalid user object type (int)",
			ctxUser:      123,
			expectError:  true,
			expectedCode: http.StatusUnauthorized,
			expectedMsg:  "User not found",
		},
		{
			name:         "invalid user object type (map)",
			ctxUser:      map[string]string{"id": "test"},
			expectError:  true,
			expectedCode: http.StatusUnauthorized,
			expectedMsg:  "User not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := &Controller{}
			ctx := logs.NewContext()
			if tt.ctxUser != nil {
				ctx = context.WithValue(ctx, "user", tt.ctxUser)
			}
			req, err := http.NewRequest(http.MethodGet, "http://localhost/test", nil)
			require.NoError(t, err)

			newCtx, apiErr := controller.CheckAdminKey(ctx, req)

			if tt.expectError {
				assert.NotNil(t, apiErr)
				if apiErr != nil {
					assert.Equal(t, tt.expectedCode, apiErr.Code)
					assert.Equal(t, tt.expectedMsg, apiErr.Message)
				}
			} else {
				assert.Nil(t, apiErr)
				if tt.expectCtxUser {
					user := GetUserFromContext(newCtx)
					assert.NotNil(t, user)
				}
			}
		})
	}
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
	regularUser, err := controller.keys.CreateKey(ctx, adminUser, "regular-user")
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
			name:         "invalid API key",
			apiKeyHeader: "invalid-key",
			expectError:  true,
			expectedCode: http.StatusUnauthorized,
			expectedMsg:  "Invalid API Key: invalid-key",
		},
		{
			name:         "empty X-API-KEY header",
			apiKeyHeader: "",
			expectError:  true,
			expectedCode: http.StatusUnauthorized,
			expectedMsg:  "Invalid API Key: ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "http://localhost/test", nil)
			require.NoError(t, err)

			if tt.apiKeyHeader != "" {
				req.Header.Set("X-API-KEY", tt.apiKeyHeader)
			}

			ctx := logs.NewContext()
			newCtx, apiErr := controller.CheckApiKey(ctx, req)

			if tt.expectError {
				assert.NotNil(t, apiErr)
				if apiErr != nil {
					assert.Equal(t, tt.expectedCode, apiErr.Code)
					assert.Equal(t, tt.expectedMsg, apiErr.Message)
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

// TestCheckApiKey_SandboxOwnership tests CheckApiKey with sandbox ownership validation
func TestCheckApiKey_SandboxOwnership(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	templateName := "test-template-auth"
	cleanup := CreateSandboxPool(t, controller, templateName, 2)
	defer cleanup()

	// Create admin user
	adminUser := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}

	// Create a regular user
	ctx := logs.NewContext()
	regularUser, err := controller.keys.CreateKey(ctx, adminUser, "regular-user")
	require.NoError(t, err)
	require.NotNil(t, regularUser)

	// Create another user for non-owner test
	anotherUser, err := controller.keys.CreateKey(ctx, adminUser, "another-user")
	require.NoError(t, err)
	require.NotNil(t, anotherUser)

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
			expectedMsg:  "Sandbox owner not found: non-existent-sandbox",
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
				req.Header.Set("X-API-KEY", tt.apiKeyHeader)
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

// TestCheckAdminKey_MiddlewareChain tests CheckAdminKey after CheckApiKey
func TestCheckAdminKey_MiddlewareChain(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	adminUser := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}

	// Create a regular user
	ctx := logs.NewContext()
	regularUser, err := controller.keys.CreateKey(ctx, adminUser, "regular-user")
	require.NoError(t, err)
	require.NotNil(t, regularUser)

	tests := []struct {
		name         string
		apiKeyHeader string
		expectAdmin  bool
		expectedCode int
		expectedMsg  string
	}{
		{
			name:         "admin user passes CheckAdminKey",
			apiKeyHeader: InitKey,
			expectAdmin:  true,
		},
		{
			name:         "regular user fails CheckAdminKey",
			apiKeyHeader: regularUser.Key,
			expectAdmin:  false,
			expectedCode: http.StatusForbidden,
			expectedMsg:  "User is not admin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "http://localhost/test", nil)
			require.NoError(t, err)
			req.Header.Set("X-API-KEY", tt.apiKeyHeader)

			ctx := logs.NewContext()

			// First call CheckApiKey
			ctx, apiErr := controller.CheckApiKey(ctx, req)
			assert.Nil(t, apiErr, "CheckApiKey should not fail")

			// Then call CheckAdminKey
			_, apiErr = controller.CheckAdminKey(ctx, req)

			if tt.expectAdmin {
				assert.Nil(t, apiErr, "CheckAdminKey should pass for admin user")
			} else {
				assert.NotNil(t, apiErr, "CheckAdminKey should fail for non-admin user")
				if apiErr != nil {
					assert.Equal(t, tt.expectedCode, apiErr.Code)
					assert.Equal(t, tt.expectedMsg, apiErr.Message)
				}
			}
		})
	}
}

// TestCheckApiKey_AnonymousUserWithAdminKeyID tests that AnonymousUser has AdminKeyID
func TestCheckApiKey_AnonymousUserWithAdminKeyID(t *testing.T) {
	// Verify AnonymousUser has AdminKeyID - this allows admin to access any sandbox
	assert.Equal(t, keys.AdminKeyID, AnonymousUser.ID, "AnonymousUser should have AdminKeyID")
	assert.Equal(t, "auth-disabled", AnonymousUser.Name, "AnonymousUser should have auth-disabled name")
}

// TestCheckApiKey_NoAPIKeyHeader tests behavior when no X-API-KEY header is provided
// This test is covered in TestCheckApiKey_WithRealSetup

// TestCheckAdminKey_NilUser tests CheckAdminKey when GetUserFromContext returns nil
func TestCheckAdminKey_NilUser(t *testing.T) {
	controller := &Controller{}
	ctx := logs.NewContext()
	// Don't set any user in context

	req, err := http.NewRequest(http.MethodGet, "http://localhost/test", nil)
	require.NoError(t, err)

	_, apiErr := controller.CheckAdminKey(ctx, req)

	assert.NotNil(t, apiErr)
	assert.Equal(t, http.StatusUnauthorized, apiErr.Code)
	assert.Equal(t, "User not found", apiErr.Message)
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
