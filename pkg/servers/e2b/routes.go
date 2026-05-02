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

	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
)

func (sc *Controller) registerRoutes() {
	sc.mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, err := fmt.Fprintf(w, "OK")
		if err != nil {
			klog.ErrorS(err, "Failed to write health check response")
		}
	})

	// Prometheus metrics endpoint for exporting metrics
	sc.mux.Handle("GET /metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))

	// Sandbox management endpoints
	RegisterE2BRoute(sc.mux, http.MethodPost, "/sandboxes", sc.CreateSandbox, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodGet, "/v2/sandboxes", sc.ListSandboxes, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodGet, "/sandboxes/{sandboxID}", sc.DescribeSandbox, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodDelete, "/sandboxes/{sandboxID}", sc.DeleteSandbox, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodPost, "/sandboxes/{sandboxID}/pause", sc.PauseSandbox, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodPost, "/sandboxes/{sandboxID}/resume", sc.ResumeSandbox, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodPost, "/sandboxes/{sandboxID}/connect", sc.ConnectSandbox, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodPost, "/sandboxes/{sandboxID}/timeout", sc.SetSandboxTimeout, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodPost, "/sandboxes/{sandboxID}/snapshots", sc.CreateSnapshot, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodGet, "/snapshots", sc.ListSnapshots, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodGet, "/templates", sc.ListTemplates, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodGet, "/templates/{templateID}", sc.GetTemplate, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodDelete, "/templates/{templateID}", sc.DeleteTemplate, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodGet, "/browser/{sandboxID}/json/version", sc.BrowserUse, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodGet, "/debug", sc.Debug, sc.CheckApiKey)

	// API Keys management endpoints
	if sc.keyCfg != nil {
		RegisterE2BRoute(sc.mux, http.MethodGet, "/teams", sc.ListTeams, sc.CheckApiKey)
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

// AnonymousUser is used only when authentication is disabled. It has the same Key as Admin,
// allowing for subsequent restrictions on Admin user request interfaces.
var AnonymousUser = &models.CreatedTeamAPIKey{
	ID:   keys.AdminKeyID,
	Name: "auth-disabled",
	Team: models.AdminTeam(),
}

// CheckApiKey implements common ApiKey validation
func (sc *Controller) CheckApiKey(ctx context.Context, r *http.Request) (context.Context, *web.ApiError) {
	logger := klog.FromContext(ctx)
	middleWareLog := logger.WithValues("middleware", "CheckApiKey").V(consts.DebugLogLevel)
	apiKey := r.Header.Get("X-API-KEY")
	var user *models.CreatedTeamAPIKey
	var ok bool
	if sc.keys == nil {
		user = AnonymousUser
	} else {
		user, ok = sc.keys.LoadByKey(ctx, apiKey)
		if !ok {
			middleWareLog.Info("failed to load key by API-KEY")
			return ctx, &web.ApiError{
				Code:    http.StatusUnauthorized,
				Message: fmt.Sprintf("Invalid API Key: %s", apiKey),
			}
		}
	}
	if sandboxID := r.PathValue("sandboxID"); sandboxID != "" {
		owner, ok := sc.manager.GetOwnerOfSandbox(sandboxID)
		if !ok {
			middleWareLog.Info("failed to get owner of sandbox")
			return ctx, &web.ApiError{
				Code:    http.StatusNotFound,
				Message: fmt.Sprintf("Sandbox owner not found: %s", sandboxID),
			}
		}
		if owner != AnonymousUser.ID.String() && owner != user.ID.String() {
			return ctx, &web.ApiError{
				Code:    http.StatusUnauthorized,
				Message: fmt.Sprintf("The user of API key is not the owner of sandbox: %s", sandboxID),
			}
		}
	}
	return context.WithValue(klog.NewContext(ctx, logger.WithValues("user", user.Name)), "user", user), nil
}

const (
	newAPIKeyRequestContextKey = "newAPIKeyRequest"
	targetAPIKeyContextKey     = "targetAPIKey"
)

func (sc *Controller) CheckCreateAPIKeyPermission(ctx context.Context, r *http.Request) (context.Context, *web.ApiError) {
	log := klog.FromContext(ctx).WithValues("middleware", "CheckCreateAPIKeyPermission").V(consts.DebugLogLevel)
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
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return ctx, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: err.Error(),
		}
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
	isAdmin := callerTeam.ID == models.AdminTeamID
	if !isAdmin && targetTeamName != callerTeam.Name {
		return ctx, &web.ApiError{
			Code:    http.StatusForbidden,
			Message: "You are not allowed to create an API key for another team",
		}
	}

	// Validate namespace of target team exists
	_, found, err := sc.keys.FindTeamByName(ctx, targetTeamName)
	if err != nil {
		return ctx, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: fmt.Sprintf("Failed to find team: %v", err),
		}
	}
	if !found {
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
	middleWareLog := logger.WithValues("middleware", "CheckDeleteAPIKeyPermission").V(consts.DebugLogLevel)
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
	if userTeam.ID != targetTeam.ID && userTeam.ID != models.AdminTeamID {
		return ctx, &web.ApiError{
			Code:    http.StatusForbidden,
			Message: "You are not allowed to delete this API key",
		}
	}
	return context.WithValue(ctx, targetAPIKeyContextKey, key), nil
}

func (sc *Controller) validateTeamNamespace(ctx context.Context, teamName string) *web.ApiError {
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
	value := ctx.Value("user")
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
