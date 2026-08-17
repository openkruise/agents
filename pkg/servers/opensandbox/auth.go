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
	e2bmodels "github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/opensandbox/models"
	"github.com/openkruise/agents/pkg/servers/web"
)

type userContextKey struct{}

// anonymousUser is used only when sc.keys is nil (OpenSandbox authentication
// disabled), mirroring e2b.AnonymousUser: it carries the admin identity so
// every caller can reach any namespace.
var anonymousUser = &e2bmodels.CreatedTeamAPIKey{
	ID:   keys.AdminKeyID,
	Name: "auth-disabled",
	Team: e2bmodels.AdminTeam(),
}

// CheckAPIKey authenticates the caller against the same team/API-key store
// the E2B API uses (see Dependencies.Keys), so a key minted through the E2B
// API-key endpoints also authenticates OpenSandbox requests and vice versa.
func (sc *Controller) CheckAPIKey(ctx context.Context, r *http.Request) (context.Context, *web.ApiError) {
	log := klog.FromContext(ctx).WithValues("middleware", "CheckAPIKey")
	apiKey := r.Header.Get(models.HeaderAPIKey)
	var user *e2bmodels.CreatedTeamAPIKey
	if sc.keys == nil {
		user = anonymousUser
	} else {
		rawAPIKey := keys.ToStoredRawAPIKey(apiKey)
		loaded, ok := sc.keys.LoadByKey(ctx, rawAPIKey)
		if !ok {
			log.Info("failed to load key by API key")
			return ctx, apiError(http.StatusUnauthorized, "Invalid API Key")
		}
		user = loaded
	}

	if sandboxID := r.PathValue("sandboxId"); sandboxID != "" {
		owner, ok := sc.manager.GetOwnerOfSandbox(sandboxID)
		if !ok {
			return ctx, apiErrorf(http.StatusNotFound, "sandbox not found: %s", sandboxID)
		}
		if owner != anonymousUser.ID.String() && owner != user.ID.String() {
			return ctx, apiErrorf(http.StatusForbidden, "the caller is not the owner of sandbox: %s", sandboxID)
		}
	}

	ctx = klog.NewContext(ctx, klog.FromContext(ctx).WithValues("user", user.Name))
	ctx = context.WithValue(ctx, userContextKey{}, user)
	return ctx, nil
}

func userFromContext(ctx context.Context) *e2bmodels.CreatedTeamAPIKey {
	user, _ := ctx.Value(userContextKey{}).(*e2bmodels.CreatedTeamAPIKey)
	return user
}

// namespaceOfUser mirrors e2b.Controller.getNamespaceOfUser: the admin team
// operates cluster-scoped (empty namespace, i.e. no namespace filter), every
// other team is namespaced by its team name.
func (sc *Controller) namespaceOfUser(user *e2bmodels.CreatedTeamAPIKey) string {
	team := keys.TeamForKey(user)
	if team.Name == e2bmodels.AdminTeamName {
		return ""
	}
	return team.Name
}
