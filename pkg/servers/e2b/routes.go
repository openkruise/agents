package e2b

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"k8s.io/klog/v2"
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
	web.RegisterRoute(sc.mux, "POST /sandboxes", sc.CreateSandbox, sc.CheckApiKey)
	web.RegisterRoute(sc.mux, "GET /v2/sandboxes", sc.ListSandboxes, sc.CheckApiKey)
	web.RegisterRoute(sc.mux, "GET /sandboxes/{sandboxID}", sc.DescribeSandbox, sc.CheckApiKey)
	web.RegisterRoute(sc.mux, "DELETE /sandboxes/{sandboxID}", sc.DeleteSandbox, sc.CheckApiKey)
	web.RegisterRoute(sc.mux, "POST /sandboxes/{sandboxID}/pause", sc.PauseSandbox, sc.CheckApiKey)
	web.RegisterRoute(sc.mux, "POST /sandboxes/{sandboxID}/resume", sc.ResumeSandbox, sc.CheckApiKey)
	web.RegisterRoute(sc.mux, "POST /sandboxes/{sandboxID}/timeout", sc.SetSandboxTimeout, sc.CheckApiKey)
	web.RegisterRoute(sc.mux, "GET /browser/{sandboxID}/json/version", sc.BrowserUse)
	web.RegisterRoute(sc.mux, "GET /debug", sc.Debug, sc.CheckApiKey)

	// API Keys management endpoints
	if sc.keys != nil {
		web.RegisterRoute(sc.mux, "GET /api-keys", sc.ListAPIKeys, sc.CheckApiKey)
		web.RegisterRoute(sc.mux, "POST /api-keys", sc.CreateAPIKey, sc.CheckApiKey)
		web.RegisterRoute(sc.mux, "DELETE /api-keys/{apiKeyID}", sc.DeleteAPIKey, sc.CheckApiKey)
	}
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
				Message: fmt.Sprintf("The user of API key is not the owner of sandbox: %s", apiKey),
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
