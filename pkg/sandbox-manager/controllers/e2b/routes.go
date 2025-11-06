package e2b

import (
	"context"
	"fmt"
	"net/http"

	"github.com/openkruise/agents/pkg/sandbox-manager/controllers/e2b/models"
	"github.com/openkruise/agents/pkg/sandbox-manager/web"
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
	web.RegisterRoute(sc.mux, "GET /api-keys", sc.ListAPIKeys, sc.CheckApiKey)
	web.RegisterRoute(sc.mux, "POST /api-keys", sc.CreateAPIKey, sc.CheckApiKey)
	web.RegisterRoute(sc.mux, "DELETE /api-keys/{apiKeyID}", sc.DeleteAPIKey, sc.CheckApiKey)
}

// CheckApiKey 实现了 Demo 的 ApiKey 验证，试验阶段写死 "GG" 为唯一合法值
func (sc *Controller) CheckApiKey(ctx context.Context, r *http.Request) (context.Context, *web.ApiError) {
	apiKey := r.Header.Get("X-API-KEY")
	user, ok := sc.keys.LoadByKey(apiKey)
	if !ok {
		return ctx, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: fmt.Sprintf("Invalid API Key: %s", apiKey),
		}
	}
	if sandboxID := r.PathValue("sandboxID"); sandboxID != "" {
		owner, ok := sc.manager.GetOwnerOfSandbox(sandboxID)
		if !ok {
			return ctx, &web.ApiError{
				Code:    http.StatusNotFound,
				Message: fmt.Sprintf("Sandbox not found: %s", sandboxID),
			}
		}
		if owner != user.ID.String() {
			return ctx, &web.ApiError{
				Code:    http.StatusUnauthorized,
				Message: fmt.Sprintf("The user of API key is not the owner of sandbox: %s", apiKey),
			}
		}
	}
	logger := klog.FromContext(ctx).WithValues("user", user.Name)
	return NewContextWithUser(klog.NewContext(ctx, logger), user), nil
}

func GetUserFromContext(ctx context.Context) *models.CreatedTeamAPIKey {
	value := ctx.Value("user")
	user, ok := value.(*models.CreatedTeamAPIKey)
	if !ok {
		return nil
	}
	return user
}

func NewContextWithUser(ctx context.Context, user *models.CreatedTeamAPIKey) context.Context {
	return context.WithValue(ctx, "user", user)
}
