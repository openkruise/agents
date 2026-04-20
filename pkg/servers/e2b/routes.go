package e2b

import (
	"context"
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/klog/v2"
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
	if sc.keys != nil {
		RegisterE2BRoute(sc.mux, http.MethodGet, "/api-keys", sc.ListAPIKeys, sc.CheckApiKey, sc.CheckAdminKey)
		RegisterE2BRoute(sc.mux, http.MethodPost, "/api-keys", sc.CreateAPIKey, sc.CheckApiKey, sc.CheckAdminKey)
		RegisterE2BRoute(sc.mux, http.MethodDelete, "/api-keys/{apiKeyID}", sc.DeleteAPIKey, sc.CheckApiKey, sc.CheckAdminKey)
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

// CheckAdminKey must be called after CheckApiKey. It checks if the user is an admin.
func (sc *Controller) CheckAdminKey(ctx context.Context, _ *http.Request) (context.Context, *web.ApiError) {
	logger := klog.FromContext(ctx)
	middleWareLog := logger.WithValues("middleware", "CheckAdminKey").V(consts.DebugLogLevel)
	user := GetUserFromContext(ctx)
	if user == nil {
		middleWareLog.Info("failed to get user from context")
		return ctx, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "User not found",
		}
	}
	if user.ID != keys.AdminKeyID {
		middleWareLog.Info("user is not admin")
		return ctx, &web.ApiError{
			Code:    http.StatusForbidden,
			Message: "User is not admin",
		}
	}
	return ctx, nil
}

func GetUserFromContext(ctx context.Context) *models.CreatedTeamAPIKey {
	value := ctx.Value("user")
	user, ok := value.(*models.CreatedTeamAPIKey)
	if !ok {
		return nil
	}
	return user
}
