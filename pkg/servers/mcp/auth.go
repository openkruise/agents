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

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

// KeyStorage defines the interface for API key validation
type KeyStorage interface {
	LoadByKey(key string) (*models.CreatedTeamAPIKey, bool)
}

// Auth handles authentication for MCP server
type Auth struct {
	keys KeyStorage
}

func NewAuth(keys *keys.SecretKeyStorage) *Auth {
	a := &Auth{}
	if keys != nil {
		a.keys = keys
	}
	return a
}

// ValidateAPIKey validates the API key and returns user information
func (a *Auth) ValidateAPIKey(ctx context.Context, apiKey string) (*models.CreatedTeamAPIKey, error) {
	if a.keys == nil {
		// If authentication is disabled, return anonymous user
		return &models.CreatedTeamAPIKey{
			ID:   keys.AdminKeyID,
			Name: "auth-disabled",
		}, nil
	}

	if apiKey == "" {
		return nil, NewMCPError(ErrorCodeAuthFailed, "API key is required", nil)
	}

	user, ok := a.keys.LoadByKey(apiKey)
	if !ok {
		return nil, NewMCPError(ErrorCodeAuthFailed, "Invalid API key", nil)
	}

	return user, nil
}

// HTTPAuthMiddleware creates an HTTP middleware for X-API-KEY authentication
func (a *Auth) HTTPAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// Extract X-API-KEY from header
		apiKey := r.Header.Get("X-API-KEY")

		// Validate API key
		user, err := a.ValidateAPIKey(ctx, apiKey)
		if err != nil {
			// Security audit log: authentication failed
			klog.FromContext(ctx).Error(err, "HTTP authentication failed",
				"apiKeyHint", MaskAPIKey(apiKey),
				"path", r.URL.Path,
				"remoteAddr", r.RemoteAddr)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Unauthorized","message":"Invalid or missing API key"}`))
			return
		}

		// Set user in context and proceed
		ctx = SetUserContext(ctx, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
