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

package opensandbox

import (
	"context"
	"net/http"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils"
)

// anonymousUser owns resources created while authentication is disabled. It
// mirrors the E2B layer's AnonymousUser convention: reusing AdminKeyID lets
// the canonical admin key access those resources after authentication is
// enabled, so a deployment that flips auth on does not orphan sandboxes
// created during the auth-disabled window.
var anonymousUser = &models.CreatedTeamAPIKey{
	ID:   keys.AdminKeyID,
	Name: "auth-disabled",
	Team: models.AdminTeam(),
}

// contextKey is an unexported type for context keys defined in this package,
// so they cannot collide with keys set by other packages on the same request.
// The E2B layer uses its own private contextKey type for the same reason; the
// two packages deliberately do not share a user-context key so that a request
// routed through one protocol never accidentally satisfies the other's
// GetUserFromContext.
type contextKey string

const userContextKey contextKey = "opensandbox-user"

// CheckApiKey authenticates the caller via the OPEN-SANDBOX-API-KEY header,
// reusing the same keys.KeyStorage backend as the E2B X-API-Key path (issue
// #690: "Auth adapter: Accept OPEN-SANDBOX-API-KEY header, reuse
// keys.KeyStorage").
//
// Behavior mirrors the E2B CheckApiKey middleware:
//   - When keys is nil (authentication disabled), the request runs as the
//     canonical anonymous caller with admin privileges.
//   - An invalid or missing key returns 401.
//
// The sandbox-scoped owner anti-enumeration check (an ownership mismatch
// returns the same 404 as a missing route, so authenticated callers cannot
// probe which sandbox IDs exist) is deliberately not implemented here: Phase 1
// registers only create, which carries no sandboxID path value, so the check
// would be unreachable. It lands together with the first sandbox-scoped route
// so it is exercised against a real route.
//
// The resolved user is stashed in the request context under userContextKey
// and retrieved by GetUserFromContext.
func CheckApiKey(keyStore keys.KeyStorage) web.MiddleWare {
	return func(ctx context.Context, r *http.Request) (context.Context, *web.ApiError) {
		logger := klog.FromContext(ctx)
		middleWareLog := logger.WithValues("middleware", "opensandbox.CheckApiKey")

		var user *models.CreatedTeamAPIKey
		if keyStore == nil {
			user = anonymousUser
		} else {
			apiKey := r.Header.Get(HeaderOpenSandboxAPIKey)
			// There is no such an existing key that can be decoded in the
			// world, so it is unnecessary to fallback to the original
			// api-key. Check raw api-key only. This mirrors the E2B layer's
			// comment and keeps the two auth paths behaviorally identical.
			rawAPIKey := keys.ToStoredRawAPIKey(apiKey)
			loaded, ok := keyStore.LoadByKey(ctx, rawAPIKey)
			if !ok {
				middleWareLog.V(utils.DebugLogLevel).Info("failed to load key by OPEN-SANDBOX-API-KEY")
				return ctx, &web.ApiError{
					Code:    http.StatusUnauthorized,
					Message: "Invalid API Key",
				}
			}
			user = loaded
		}

		ctx = klog.NewContext(ctx, logger.WithValues("user", user.Name))
		ctx = context.WithValue(ctx, userContextKey, user)
		return ctx, nil
	}
}

// GetUserFromContext retrieves the authenticated user stashed by CheckApiKey.
// It returns nil when the middleware was not in the chain or rejected the
// request; handlers must treat nil as an internal error (the middleware
// should have already returned 401).
func GetUserFromContext(ctx context.Context) *models.CreatedTeamAPIKey {
	value := ctx.Value(userContextKey)
	user, ok := value.(*models.CreatedTeamAPIKey)
	if !ok {
		return nil
	}
	return user
}

// NamespaceOfUser resolves the Kubernetes namespace a user's sandboxes live
// in. Keys in the admin team can access resources in cluster scope, so the
// admin team maps to the empty string (the convention used by the E2B layer's
// getNamespaceOfUser). Team name — not team UUID — is the namespace identity,
// per the shared API-layer rule.
func NamespaceOfUser(user *models.CreatedTeamAPIKey) string {
	team := keys.TeamForKey(user)
	if team.Name == models.AdminTeamName {
		return ""
	}
	return team.Name
}
