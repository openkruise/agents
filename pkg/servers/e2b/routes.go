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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/openkruise/agents/pkg/sandboxid"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils"
)

func (sc *Controller) registerRoutes() {
	healthHandler := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, err := fmt.Fprintf(w, "OK")
		if err != nil {
			klog.ErrorS(err, "Failed to write health check response")
		}
	}
	sc.mux.HandleFunc("GET /health", healthHandler)
	// Also register under the /kruise/api prefix so requests routed through
	// the sandbox-gateway (which forwards /kruise/api/* to the manager) work.
	sc.mux.HandleFunc("GET "+adapters.CustomPrefix+"/api/health", healthHandler)

	// Prometheus metrics endpoint for exporting metrics
	sc.mux.Handle("GET /metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))

	// Sandbox management endpoints
	RegisterE2BRoute(sc.mux, http.MethodPost, "/sandboxes", sc.CreateSandbox, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodGet, "/v2/sandboxes", sc.ListSandboxes, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodGet, "/sandboxes/{sandboxID}", sc.DescribeSandbox, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodDelete, "/sandboxes/{sandboxID}", sc.DeleteSandbox, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodPut, "/sandboxes/{sandboxID}/network", sc.UpdateSandboxNetwork, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodPost, "/sandboxes/{sandboxID}/pause", sc.PauseSandbox, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodPost, "/sandboxes/{sandboxID}/resume", sc.ResumeSandbox, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodPost, "/sandboxes/{sandboxID}/connect", sc.ConnectSandbox, sc.CheckApiKey)
	web.RegisterRoute(sc.mux, http.MethodPost, adapters.CustomPrefix+"/api/sandboxes/{sandboxID}/traffic-access-token", sc.RefreshTrafficAccessToken, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodPost, "/sandboxes/{sandboxID}/timeout", sc.SetSandboxTimeout, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodPost, "/sandboxes/{sandboxID}/snapshots", sc.CreateSnapshot, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodGet, "/snapshots", sc.ListSnapshots, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodGet, "/templates", sc.ListTemplates, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodGet, "/templates/{templateID}", sc.GetTemplate, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodDelete, "/templates/{templateID}", sc.DeleteTemplate, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodGet, "/browser/{sandboxID}/json/version", sc.BrowserUse, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodGet, "/debug", sc.Debug, sc.CheckApiKey)

	// Volume management endpoints
	// Temporarily disabled.
	// RegisterE2BRoute(sc.mux, http.MethodPost, "/volumes", sc.CreateVolume, sc.CheckApiKey)
	// RegisterE2BRoute(sc.mux, http.MethodGet, "/volumes", sc.ListVolumes, sc.CheckApiKey)
	// RegisterE2BRoute(sc.mux, http.MethodGet, "/volumes/{volumeID}", sc.GetVolume, sc.CheckApiKey)
	// RegisterE2BRoute(sc.mux, http.MethodDelete, "/volumes/{volumeID}", sc.DeleteVolume, sc.CheckApiKey)

	// API Keys management endpoints
	if sc.keyCfg != nil {
		RegisterE2BRoute(sc.mux, http.MethodGet, "/teams", sc.ListTeams, sc.CheckApiKey)
		RegisterE2BRoute(sc.mux, http.MethodGet, "/api-keys/compatible", sc.GetCompatibleAPIKey, sc.CheckApiKey)
		RegisterE2BRoute(sc.mux, http.MethodGet, "/api-keys", sc.ListAPIKeys, sc.CheckApiKey)
		RegisterE2BRoute(sc.mux, http.MethodPost, "/api-keys", sc.CreateAPIKey, sc.CheckApiKey, sc.CheckCreateAPIKeyPermission)
		RegisterE2BRoute(sc.mux, http.MethodDelete, "/api-keys/{apiKeyID}", sc.DeleteAPIKey, sc.CheckApiKey, sc.CheckDeleteAPIKeyPermission)
	}
}

func RegisterE2BRoute[T any](mux *http.ServeMux, method, path string, handler web.Handler[T], middlewares ...web.MiddleWare) {
	// Native E2B API
	web.RegisterRoute(mux, method, path, handler, middlewares...)
	// Customized E2B API
	web.RegisterRoute(mux, method, adapters.CustomPrefix+"/api"+path, handler, middlewares...)
}

// AnonymousUser owns resources created while authentication is disabled. Reusing AdminKeyID
// lets the canonical admin key access those resources after authentication is enabled.
var AnonymousUser = &models.CreatedTeamAPIKey{
	ID:   keys.AdminKeyID,
	Name: "auth-disabled",
	Team: models.AdminTeam(),
}

// CheckApiKey implements common ApiKey validation
func (sc *Controller) CheckApiKey(ctx context.Context, r *http.Request) (context.Context, *web.ApiError) {
	logger := klog.FromContext(ctx)
	middleWareLog := logger.WithValues("middleware", "CheckApiKey")
	apiKey := r.Header.Get(models.HeaderApiKey)
	var user *models.CreatedTeamAPIKey
	var ok bool
	if sc.keys == nil {
		user = AnonymousUser
	} else {
		// There's no such an existing key that can be decoded in the world,
		// so it's unnecessary to fallback to the original api-key. Check raw api-key only.
		rawAPIKey := keys.ToStoredRawAPIKey(apiKey)
		user, ok = sc.keys.LoadByKey(ctx, rawAPIKey)
		if !ok {
			middleWareLog.V(utils.DebugLogLevel).Info("failed to load key by API-KEY")
			return ctx, &web.ApiError{
				Code:    http.StatusUnauthorized,
				Message: "Invalid API Key",
			}
		}
	}
	if sandboxID := r.PathValue("sandboxID"); sandboxID != "" {
		middleWareLog = middleWareLog.WithValues("sandboxID", sandboxID)
		owner, ok := sc.manager.GetOwnerOfSandbox(sandboxID)
		if !ok {
			middleWareLog.V(utils.DebugLogLevel).Info("failed to get owner of sandbox")
			return ctx, &web.ApiError{
				Code:    http.StatusNotFound,
				Message: fmt.Sprintf("Sandbox route not found, maybe it is crashed or killed: %s", sandboxID),
			}
		}
		// An ownership mismatch returns the same not-found response as a missing route so
		// authenticated callers cannot probe which sandbox IDs exist. That makes this log
		// the only signal separating a denial from a genuine miss.
		if owner != user.ID.String() {
			middleWareLog.Info("sandbox owner mismatch", "owner", owner, "user", user.ID.String())
			return ctx, &web.ApiError{
				Code:    http.StatusNotFound,
				Message: fmt.Sprintf("Sandbox route not found, maybe it is crashed or killed: %s", sandboxID),
			}
		}
	}
	if volumeID := r.PathValue("volumeID"); volumeID != "" {
		middleWareLog = middleWareLog.WithValues("volumeID", volumeID)
		namespace := sc.getNamespaceOfUser(user)
		if namespace == "" {
			namespace = sc.mgrOpts.SystemNamespace
		}
		owner, ok := sc.manager.GetOwnerOfVolume(ctx, namespace, volumeID)
		if !ok {
			middleWareLog.V(utils.DebugLogLevel).Info("failed to get owner of volume")
			return ctx, &web.ApiError{
				Code:    http.StatusNotFound,
				Message: fmt.Sprintf("Volume not found: %s", volumeID),
			}
		}
		// Same anti-enumeration rule as the sandbox check above: ownership mismatch is
		// indistinguishable from a missing volume.
		if owner != user.ID.String() {
			middleWareLog.Info("volume owner mismatch", "owner", owner, "user", user.ID.String())
			return ctx, &web.ApiError{
				Code:    http.StatusNotFound,
				Message: fmt.Sprintf("Volume not found: %s", volumeID),
			}
		}
	}
	ctx = klog.NewContext(ctx, logger.WithValues("user", user.Name))
	ctx = context.WithValue(ctx, userContextKey, user)
	return ctx, nil
}

// contextKey is an unexported type for context keys defined in this package,
// so they cannot collide with keys set by other packages on the same request.
type contextKey string

const (
	userContextKey             contextKey = "user"
	newAPIKeyRequestContextKey contextKey = "newAPIKeyRequest"
	targetAPIKeyContextKey     contextKey = "targetAPIKey"
)

func (sc *Controller) CheckCreateAPIKeyPermission(ctx context.Context, r *http.Request) (context.Context, *web.ApiError) {
	log := klog.FromContext(ctx).WithValues("middleware", "CheckCreateAPIKeyPermission").V(utils.DebugLogLevel)
	user := GetUserFromContext(ctx)
	if user == nil {
		log.Info("failed to get user from context")
		return ctx, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "User not found",
		}
	}

	// Parse caller team and target team
	var request models.NewTeamAPIKey
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return ctx, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: err.Error(),
		}
	}
	if apiErr := validateCreateAPIKeyRequest(&request); apiErr != nil {
		return ctx, apiErr
	}

	callerTeam := keys.TeamForKey(user)
	targetTeamName := request.TeamName
	if targetTeamName == "" {
		targetTeamName = callerTeam.Name
	}
	if targetTeamName == "" {
		return ctx, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "teamName is required",
		}
	}

	// Only admin can create API key for other team
	isAdmin := callerTeam.Name == models.AdminTeamName
	if request.QuotaSpec != nil && !isAdmin {
		return ctx, &web.ApiError{
			Code:    http.StatusForbidden,
			Message: "only admin can set api-key quota",
		}
	}
	if !isAdmin && targetTeamName != callerTeam.Name {
		return ctx, &web.ApiError{
			Code:    http.StatusForbidden,
			Message: "You are not allowed to create an API key for another team",
		}
	}

	// Namespace-scoped teams must still have a namespace; admin is cluster-scoped.
	if targetTeamName != models.AdminTeamName {
		if apiErr := sc.validateTeamNamespace(ctx, targetTeamName); apiErr != nil {
			return ctx, apiErr
		}
	}

	request.TeamName = targetTeamName
	ctx = context.WithValue(ctx, newAPIKeyRequestContextKey, &request)
	return ctx, nil
}

func (sc *Controller) CheckDeleteAPIKeyPermission(ctx context.Context, r *http.Request) (context.Context, *web.ApiError) {
	logger := klog.FromContext(ctx)
	middleWareLog := logger.WithValues("middleware", "CheckDeleteAPIKeyPermission").V(utils.DebugLogLevel)
	user := GetUserFromContext(ctx)
	if user == nil {
		middleWareLog.Info("failed to get user from context")
		return ctx, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "User not found",
		}
	}
	apiKeyID := r.PathValue("apiKeyID")
	key, ok := sc.keys.LoadByID(ctx, apiKeyID)
	if !ok {
		return ctx, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: "API key not found",
		}
	}

	userTeam := keys.TeamForKey(user)
	targetTeam := keys.TeamForKey(key)
	if userTeam.Name != targetTeam.Name && userTeam.Name != models.AdminTeamName {
		return ctx, &web.ApiError{
			Code:    http.StatusForbidden,
			Message: "You are not allowed to delete this API key",
		}
	}
	return context.WithValue(ctx, targetAPIKeyContextKey, key), nil
}

func (sc *Controller) validateTeamNamespace(ctx context.Context, teamName string) *web.ApiError {
	if teamName == "" {
		return &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "namespace must not be empty",
		}
	}
	if strings.Contains(teamName, sandboxid.LegacySeparator) {
		return &web.ApiError{
			Code: http.StatusBadRequest,
			Message: fmt.Sprintf(
				"namespace %q must not contain %q: this sequence is reserved as the sandbox ID separator",
				teamName,
				sandboxid.LegacySeparator,
			),
		}
	}
	namespace := &corev1.Namespace{}
	if err := sc.cache.GetClient().Get(ctx, client.ObjectKey{Name: teamName}, namespace); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
			return &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: fmt.Sprintf("Kubernetes namespace %q does not exist", teamName),
			}
		}
		return &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: fmt.Sprintf("Failed to validate Kubernetes namespace %q: %v", teamName, err),
		}
	}

	return nil
}

func GetUserFromContext(ctx context.Context) *models.CreatedTeamAPIKey {
	value := ctx.Value(userContextKey)
	user, ok := value.(*models.CreatedTeamAPIKey)
	if !ok {
		return nil
	}
	return user
}

func GetNewAPIKeyRequestFromContext(ctx context.Context) (*models.NewTeamAPIKey, bool) {
	value := ctx.Value(newAPIKeyRequestContextKey)
	request, ok := value.(*models.NewTeamAPIKey)
	return request, ok
}

func GetTargetAPIKeyFromContext(ctx context.Context) *models.CreatedTeamAPIKey {
	value := ctx.Value(targetAPIKeyContextKey)
	apiKey, ok := value.(*models.CreatedTeamAPIKey)
	if !ok {
		return nil
	}
	return apiKey
}
