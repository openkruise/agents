package e2b

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
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

	// Sandbox management endpoints
	RegisterE2BRoute(sc.mux, http.MethodPost, "/sandboxes", sc.CreateSandbox, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodGet, "/v2/sandboxes", sc.ListSandboxes, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodGet, "/sandboxes/{sandboxID}", sc.DescribeSandbox, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodDelete, "/sandboxes/{sandboxID}", sc.DeleteSandbox, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodPost, "/sandboxes/{sandboxID}/pause", sc.PauseSandbox, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodPost, "/sandboxes/{sandboxID}/resume", sc.ResumeSandbox, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodPost, "/sandboxes/{sandboxID}/connect", sc.ConnectSandbox, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodPost, "/sandboxes/{sandboxID}/timeout", sc.SetSandboxTimeout, sc.CheckApiKey)
	RegisterE2BRoute(sc.mux, http.MethodGet, "/browser/{sandboxID}/json/version", sc.BrowserUse)
	RegisterE2BRoute(sc.mux, http.MethodGet, "/debug", sc.Debug, sc.CheckApiKey)

	// API Keys management endpoints
	if sc.keys != nil {
		RegisterE2BRoute(sc.mux, http.MethodGet, "/api-keys", sc.ListAPIKeys, sc.CheckApiKey)
		RegisterE2BRoute(sc.mux, http.MethodPost, "/api-keys", sc.CreateAPIKey, sc.CheckApiKey)
		RegisterE2BRoute(sc.mux, http.MethodDelete, "/api-keys/{apiKeyID}", sc.DeleteAPIKey, sc.CheckApiKey)
	}
}

func RegisterE2BRoute[T any](mux *http.ServeMux, method, path string, handler web.Handler[T], middlewares ...web.MiddleWare) {
	// Native E2B API
	web.RegisterRoute(mux, method, path, handler, middlewares...)
	// Customized E2B API
	web.RegisterRoute(mux, method, adapters.CustomPrefix+"/api"+path, handler, middlewares...)
}

var AnonymousUser = &models.CreatedTeamAPIKey{
	ID:   uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"), // Meaningless random number, used to represent anonymous users in non-authentication mode
	Name: "auth-disabled",
}

// CheckApiKey implements Demo's ApiKey validation
func (sc *Controller) CheckApiKey(ctx context.Context, r *http.Request) (context.Context, *web.ApiError) {
	logger := klog.FromContext(ctx)
	middleWareLog := logger.WithValues("middleware", "CheckApiKey").V(consts.DebugLogLevel)
	apiKey := r.Header.Get("X-API-KEY")
	var user *models.CreatedTeamAPIKey
	var ok bool
	if sc.keys == nil {
		user = AnonymousUser
	} else {
		user, ok = sc.keys.LoadByKey(apiKey)
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

func GetUserFromContext(ctx context.Context) *models.CreatedTeamAPIKey {
	value := ctx.Value("user")
	user, ok := value.(*models.CreatedTeamAPIKey)
	if !ok {
		return nil
	}
	return user
}
