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

package apiauth

import (
	"context"
	"net/http"

	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"k8s.io/klog/v2"
)

// ContextKey identifies the authenticated caller across API surfaces.
type ContextKey string

const UserContextKey ContextKey = "user"

// AnonymousUser preserves admin ownership when API-key authentication is disabled.
var AnonymousUser = &models.CreatedTeamAPIKey{ID: keys.AdminKeyID, Name: "auth-disabled", Team: models.AdminTeam()}

func User(ctx context.Context) *models.CreatedTeamAPIKey {
	user, _ := ctx.Value(UserContextKey).(*models.CreatedTeamAPIKey)
	return user
}

func Namespace(user *models.CreatedTeamAPIKey) string {
	team := keys.TeamForKey(user)
	// Admin team keys resolve resources across namespaces; team UUIDs are display metadata.
	if team.Name == models.AdminTeamName {
		return ""
	}
	return team.Name
}

// Authenticate shares key normalization and caller identity without resource authorization.
func Authenticate(ctx context.Context, r *http.Request, storage keys.KeyStorage) (context.Context, *web.ApiError) {
	user := AnonymousUser
	if storage != nil {
		// Compatible SDK keys normalize once; do not fall back to their encoded value.
		raw := keys.ToStoredRawAPIKey(r.Header.Get(models.HeaderApiKey))
		var ok bool
		if raw != "" {
			user, ok = storage.LoadByKey(ctx, raw)
		}
		if !ok {
			klog.FromContext(ctx).V(5).Info("failed to load key by API-KEY", "middleware", "CheckApiKey")
			return ctx, &web.ApiError{Code: http.StatusUnauthorized, Message: "Invalid API Key"}
		}
	}
	ctx = klog.NewContext(ctx, klog.FromContext(ctx).WithValues("user", user.Name))
	return context.WithValue(ctx, UserContextKey, user), nil
}
