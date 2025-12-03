package e2b

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager"
	innererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils"
	managerutils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"
)

// CreateSandbox allocates a Pod as a new sandbox
func (sc *Controller) CreateSandbox(r *http.Request) (web.ApiResponse[*models.Sandbox], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)
	start := time.Now()
	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "User is empty",
		}
	}
	var request models.NewSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Message: err.Error(),
		}
	}

	for k := range request.Metadata {
		if errLists := validation.IsQualifiedName(k); len(errLists) > 0 {
			return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: fmt.Sprintf("Invalid metadata key [%s]: %s", k, strings.Join(errLists, ", ")),
			}
		}

		if err := managerutils.ValidatedCustomLabelKey(k); err != nil {
			return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: fmt.Sprintf("Invalid metadata key [%s]: %s", k, err),
			}
		}
	}

	if request.Timeout == 0 {
		request.Timeout = 300
	}

	if request.Timeout < 30 || request.Timeout > 7200 {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "timeout should between 30 and 7200",
		}
	}

	claimStart := time.Now()
	sbx, err := sc.manager.ClaimSandbox(ctx, user.ID.String(), request.TemplateID, request.Timeout)
	if err != nil {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Message: err.Error(),
		}
	}
	claimCost := time.Since(claimStart)

	initStart := time.Now()
	pool, ok := sc.manager.GetInfra().GetPoolByObject(sbx)
	if !ok {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: "Failed to get sandbox pool",
		}
	}
	var wg sync.WaitGroup
	var errCh = make(chan error, 3)
	if pool.GetAnnotations()[AnnotationShouldInitEnvd] == utils.True {
		wg.Add(1)
		go func() {
			start := time.Now()
			defer wg.Done()
			if err := sc.initEnvd(ctx, sbx, request.EnvVars); err != nil {
				klog.ErrorS(err, "Failed to init envd")
				errCh <- err
			}
			log.Info("init envd done", "cost", time.Since(start))
		}()
	}
	wg.Add(1)
	go func() {
		start := time.Now()
		defer wg.Done()
		if err := sbx.PatchLabels(ctx, request.Metadata); err != nil {
			klog.ErrorS(err, "Failed to add sandbox metadata to sbx")
			errCh <- err
			return
		}
		log.Info("patch sandbox done", "cost", time.Since(start))
	}()
	wg.Wait()
	close(errCh)
	var errList error
	for err := range errCh {
		errList = errors.Join(errList, err)
	}
	if errList != nil {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Message: errList.Error(),
		}
	}
	initCost := time.Since(initStart)

	log.Info("sandbox allocated", "sbx", klog.KObj(sbx), "totalCost", time.Since(start),
		"claimCost", claimCost, "initCost", initCost)
	return web.ApiResponse[*models.Sandbox]{
		Code: http.StatusCreated,
		Body: sc.convertToE2BSandbox(r.Context(), sbx),
	}, nil
}

func (sc *Controller) PauseSandbox(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	return sc.pauseAndResumeSandbox(r, true)
}

func (sc *Controller) ResumeSandbox(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	return sc.pauseAndResumeSandbox(r, false)
}

// ListSandboxes returns a list of all created sandboxes (allocated pods) This API is not ready now.
func (sc *Controller) ListSandboxes(r *http.Request) (web.ApiResponse[[]*models.Sandbox], *web.ApiError) {
	var request models.ListSandboxesRequest
	request.Metadata = make(map[string]string)
	for key, values := range r.URL.Query() {
		if len(values) == 0 {
			continue
		}
		switch key {
		case "state":
			state := values[0]
			if state != models.SandboxStateRunning && state != models.SandboxStatePaused {
				return web.ApiResponse[[]*models.Sandbox]{}, &web.ApiError{
					Code:    http.StatusBadRequest,
					Message: fmt.Sprintf("Invalid state: %v", values[0]),
				}
			}
			request.State = values[0]
		case "nextToken":
			request.NextToken = values[0]
		case "limit":
			limit, err := strconv.Atoi(values[0])
			if err != nil || limit <= 0 || limit > 100 {
				return web.ApiResponse[[]*models.Sandbox]{}, &web.ApiError{
					Code:    http.StatusBadRequest,
					Message: fmt.Sprintf("Invalid limit: %v", values[0]),
				}
			}
			request.Limit = int32(limit)
		default:
			request.Metadata[key] = values[0]
		}
	}
	pods, err := sc.manager.ListClaimedSandboxes(request.State, request.Metadata)
	if err != nil {
		return web.ApiResponse[[]*models.Sandbox]{}, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: fmt.Sprintf("Failed to list sandboxes: %v", err),
		}
	}
	sandboxes := make([]*models.Sandbox, 0, len(pods))
	for _, pod := range pods {
		sandbox := sc.convertToE2BSandbox(r.Context(), pod)
		if sandbox.Created() {
			sandboxes = append(sandboxes, sandbox)
		}
	}
	return web.ApiResponse[[]*models.Sandbox]{
		Body: sandboxes,
	}, nil
}

// DescribeSandbox returns details of a specific sandbox
// This API is not used by demo, should delay
func (sc *Controller) DescribeSandbox(r *http.Request) (web.ApiResponse[*models.Sandbox], *web.ApiError) {
	id := r.PathValue("sandboxID")
	pod, err := sc.manager.GetClaimedSandbox(id)
	if err != nil {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: fmt.Sprintf("Sandbox with id %s not found: %v", id, err),
		}
	}
	return web.ApiResponse[*models.Sandbox]{
		Body: sc.convertToE2BSandbox(r.Context(), pod),
	}, nil
}

// DeleteSandbox deletes a specific sandbox
func (sc *Controller) DeleteSandbox(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	id := r.PathValue("sandboxID")
	log := klog.FromContext(r.Context())
	err := sc.manager.DeleteClaimedSandbox(r.Context(), id)
	if err != nil {
		log.Error(err, "failed to delete sandbox", "id", id)
		switch innererrors.GetErrCode(err) {
		case innererrors.ErrorBadRequest:
			fallthrough // e2b 协议不支持这里返回 400，统一返回 404
		case innererrors.ErrorNotFound:
			return web.ApiResponse[struct{}]{}, &web.ApiError{
				Code:    http.StatusNotFound,
				Message: fmt.Sprintf("Sandbox %s not found", id),
			}
		default:
			return web.ApiResponse[struct{}]{}, &web.ApiError{
				Message: fmt.Sprintf("Failed to delete sandbox: %v", err),
			}
		}
	}
	log.Info("sandbox deleted", "id", id)
	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}

func (sc *Controller) SetSandboxTimeout(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)
	id := r.PathValue("sandboxID")
	var request models.SetTimeoutRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Message: err.Error(),
		}
	}
	if request.TimeoutSeconds <= 0 || request.TimeoutSeconds > 3600 {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Message: "timeout seconds cannot be smaller than 0 or larger than 3600",
		}
	}
	sbx, err := sc.manager.GetClaimedSandbox(id)
	if err != nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: fmt.Sprintf("Sandbox %s not found", id),
		}
	}
	if err := sc.manager.SetSandboxTimeout(ctx, sbx, request.TimeoutSeconds); err != nil {
		log.Error(err, "failed to set sandbox timeout", "id", id, "timeout", request.TimeoutSeconds)
		switch innererrors.GetErrCode(err) {
		case innererrors.ErrorNotFound:
			return web.ApiResponse[struct{}]{}, &web.ApiError{
				Code:    http.StatusNotFound,
				Message: fmt.Sprintf("Sandbox %s not found", id),
			}
		default:
			return web.ApiResponse[struct{}]{}, &web.ApiError{
				Message: fmt.Sprintf("Failed to set sandbox timeout: %v", err),
			}
		}
	}
	log.Info("sandbox timeout set", "id", id, "timeout", request.TimeoutSeconds)
	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}

type browserHandShake struct {
	Browser              string `json:"Browser"`
	ProtocolVersion      string `json:"Protocol-Version"`
	UserAgent            string `json:"User-Agent"`
	V8Version            string `json:"V8-Version"`
	WebKitVersion        string `json:"WebKit-Version"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// BrowserUse is a cdp entry for browser_use to create a session
// Use case:
//
//	```python
//	browser_session = BrowserSession(cdp_url=f"https://api.{CDP_DOMAIN}/browser/{sandbox_id}")
//	```
func (sc *Controller) BrowserUse(r *http.Request) (web.ApiResponse[*browserHandShake], *web.ApiError) {
	sandboxID := r.PathValue("sandboxID")
	sbx, err := sc.manager.GetClaimedSandbox(sandboxID)
	if err != nil {
		return web.ApiResponse[*browserHandShake]{}, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: err.Error(),
		}
	}

	resp, err := sbx.Request(r, "/json/version", models.CDPPort)
	if err != nil {
		return web.ApiResponse[*browserHandShake]{}, &web.ApiError{
			Message: fmt.Sprintf("Failed to proxy request to sandbox port %d: %v", models.CDPPort, err),
		}
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return web.ApiResponse[*browserHandShake]{}, &web.ApiError{
			Message: fmt.Sprintf("Failed to read response body: %v", err),
		}
	}
	var h browserHandShake
	if err = json.Unmarshal(body, &h); err != nil {
		return web.ApiResponse[*browserHandShake]{}, &web.ApiError{
			Message: fmt.Sprintf("Failed to unmarshal response body: %v", err),
		}
	}

	h.WebSocketDebuggerURL = browserWebSocketReplacer.ReplaceAllString(h.WebSocketDebuggerURL,
		fmt.Sprintf("wss://%s", managerutils.GetSandboxAddress(sandboxID, sc.domain, models.CDPPort)))
	return web.ApiResponse[*browserHandShake]{
		Code: resp.StatusCode,
		Body: &h,
	}, nil
}

func (sc *Controller) Debug(_ *http.Request) (web.ApiResponse[sandbox_manager.DebugInfo], *web.ApiError) {
	return web.ApiResponse[sandbox_manager.DebugInfo]{
		Body: sc.manager.GetDebugInfo(),
	}, nil
}

func (sc *Controller) ListAPIKeys(r *http.Request) (web.ApiResponse[[]*models.TeamAPIKey], *web.ApiError) {
	ctx := r.Context()
	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[[]*models.TeamAPIKey]{}, &web.ApiError{
			Message: "User not found",
		}
	}
	apiKeys := sc.keys.ListByOwner(user.ID)

	return web.ApiResponse[[]*models.TeamAPIKey]{
		Body: apiKeys,
	}, nil
}

func (sc *Controller) CreateAPIKey(r *http.Request) (web.ApiResponse[*models.CreatedTeamAPIKey], *web.ApiError) {
	var request models.NewTeamAPIKey
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return web.ApiResponse[*models.CreatedTeamAPIKey]{}, &web.ApiError{
			Message: err.Error(),
		}
	}

	ctx := r.Context()
	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[*models.CreatedTeamAPIKey]{}, &web.ApiError{
			Message: "User not found",
		}
	}
	createdAPIKey, err := sc.keys.CreateKey(ctx, user, request.Name)
	if err != nil {
		return web.ApiResponse[*models.CreatedTeamAPIKey]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: fmt.Sprintf("Failed to create API key: %v", err),
		}
	}

	return web.ApiResponse[*models.CreatedTeamAPIKey]{
		Code: http.StatusCreated,
		Body: createdAPIKey,
	}, nil
}

func (sc *Controller) DeleteAPIKey(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	apiKeyID := r.PathValue("apiKeyID")

	ctx := r.Context()
	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: "User not found",
		}
	}

	key, ok := sc.keys.LoadByID(apiKeyID)
	if !ok {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: "API key not found",
		}
	}
	if key.CreatedBy == nil || key.CreatedBy.ID != user.ID && key.ID != user.ID {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "You are not allowed to delete this API key",
		}
	}
	if err := sc.keys.DeleteKey(ctx, key); err != nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: fmt.Sprintf("Failed to delete API key: %v", err),
		}
	}

	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}
