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
	"time"

	"k8s.io/klog/v2"

	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
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

// sandboxContextKey carries the infra.Sandbox loaded by loadOwnedSandbox so the
// handler does not repeat the lookup. It is package-private for the same reason
// as userContextKey: a request routed through the OpenSandbox surface must never
// satisfy the E2B layer's context lookups, and vice versa.
const sandboxContextKey contextKey = "opensandbox-sandbox"

// sandboxLoadTimeout bounds the informer-backed GetSandbox lookup. It mirrors
// the E2B layer's 2-second bound so a stuck cache read fails fast instead of
// holding the request for the full client deadline.
const sandboxLoadTimeout = 2 * time.Second

// CheckApiKey authenticates the caller via the OPEN-SANDBOX-API-KEY header,
// reusing the same keys.KeyStorage backend as the E2B X-API-Key path (issue
// #690: "Auth adapter: Accept OPEN-SANDBOX-API-KEY header, reuse
// keys.KeyStorage").
//
// Behavior mirrors the E2B CheckApiKey middleware:
//   - When keys is nil (authentication disabled), the request runs as the
//     canonical anonymous caller with AdminKeyID and admin-team lookup scope.
//     The concrete key ID remains the sandbox owner.
//   - An invalid or missing key returns 401.
//
// Sandbox-scoped routes additionally use loadOwnedSandbox to check ownership.
// Missing and foreign sandboxes share the same public 404 status and body.
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
// in. Admin-team keys use the empty string for cross-namespace lookup, matching
// the E2B layer's getNamespaceOfUser. This scope does not bypass owner checks:
// ownership still uses the authenticated key ID. Team name — not team UUID —
// is the namespace identity, per the shared API-layer rule.
func NamespaceOfUser(user *models.CreatedTeamAPIKey) string {
	team := keys.TeamForKey(user)
	if team.Name == models.AdminTeamName {
		return ""
	}
	return team.Name
}

// loadOwnedSandbox returns a middleware that resolves the `{sandboxId}` path
// value into an infra.Sandbox owned by the authenticated caller and stashes it
// in the request context for the handler.
//
// It is the sandbox-scoped owner anti-enumeration boundary the Phase 1 auth
// comment deferred to this PR: a sandbox that does not exist, exists but is
// owned by another team, or exists but is not in an expected state all collapse
// to the same 404, so an authenticated caller cannot probe which sandbox IDs
// exist or who owns them. Only an inconclusive infra failure (ErrorInternal)
// surfaces as 500. expectedStates is per-route (describe/delete accept the
// claimed set for describe; delete accepts any owned state; resume/renew accept
// only the live set) and is forwarded to
// Manager.GetSandbox, which enforces both ownership and state.
//
// The OpenSandbox DELETE spec admits 404 for a missing sandbox (unlike the E2B
// layer's idempotent 204), so every sandbox-scoped route — including delete —
// lets this middleware's 404 propagate.
func (s *Server) loadOwnedSandbox(expectedStates []string) web.MiddleWare {
	return func(ctx context.Context, r *http.Request) (context.Context, *web.ApiError) {
		log := klog.FromContext(ctx)
		sandboxID := r.PathValue(pathValueSandboxID)
		user := GetUserFromContext(ctx)
		if user == nil {
			// CheckApiKey should have rejected the request first; 500 keeps the
			// missing-middleware bug visible instead of masquerading as auth.
			return ctx, &web.ApiError{
				Code:    http.StatusInternalServerError,
				Message: "user not found in request context",
			}
		}

		getCtx, cancel := context.WithTimeout(ctx, sandboxLoadTimeout)
		defer cancel()
		sbx, err := s.manager.GetSandbox(getCtx, user.ID.String(), expectedStates, infra.GetSandboxOptions{
			Namespace: NamespaceOfUser(user),
			SandboxID: sandboxID,
		})
		if err != nil {
			log.Error(err, "failed to load sandbox", "sandboxID", sandboxID)
			code := getSandboxErrorCode(err)
			message := "sandbox not found"
			if code == http.StatusInternalServerError {
				message = "failed to load sandbox"
			}
			return ctx, &web.ApiError{Code: code, Message: message}
		}
		return context.WithValue(ctx, sandboxContextKey, sbx), nil
	}
}

// getSandboxErrorCode maps a Manager.GetSandbox failure to the OpenSandbox HTTP
// boundary under the anti-enumeration rule: every classified failure stays 404
// so existence and ownership do not leak through a distinguishable status, and
// only an inconclusive infra failure becomes 500. It mirrors the E2B layer's
// getSandboxErrorCode; the two are kept separate so a future change to either
// protocol's enumeration posture cannot silently reshape the other.
func getSandboxErrorCode(err error) int {
	if managererrors.GetErrCode(err) == managererrors.ErrorInternal {
		return http.StatusInternalServerError
	}
	return http.StatusNotFound
}

// sandboxFromContext retrieves the infra.Sandbox stashed by loadOwnedSandbox.
// It returns nil when the middleware was not in the chain; handlers treat nil as
// an internal error because the middleware should have already resolved it.
func sandboxFromContext(ctx context.Context) infra.Sandbox {
	sbx, _ := ctx.Value(sandboxContextKey).(infra.Sandbox)
	return sbx
}

// Shared guard messages. Extracted to constants so the wording stays uniform
// across handlers and the repeated literals do not trip goconst.
const (
	errMessageUserNotInContext    = "user not found in request context"
	errMessageSandboxNotInContext = "sandbox not found in request context"
)

// sandboxFromContextOrError retrieves the sandbox stashed by loadOwnedSandbox,
// returning a 500 ApiError when it is absent. Absence is a wiring bug — the
// middleware short-circuits with 404 before the handler runs — so 500 keeps it
// visible instead of masquerading as a client error. It centralizes the guard
// every sandbox-scoped handler runs first.
func sandboxFromContextOrError(ctx context.Context) (infra.Sandbox, *web.ApiError) {
	sbx := sandboxFromContext(ctx)
	if sbx == nil {
		return nil, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: errMessageSandboxNotInContext,
		}
	}
	return sbx, nil
}
