// Package e2b provides HTTP controllers for handling E2B sandbox requests.
package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/controllers/e2b/models"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/web"
	"k8s.io/klog/v2"
)

var (
	Namespace = "default"
)

var (
	browserWebSocketReplacer = regexp.MustCompile(`^ws://[^/]+`)
)

func (sc *Controller) initEnvd(ctx context.Context, sbx infra.Sandbox, envVars models.EnvVars) error {
	start := time.Now()
	log := klog.FromContext(ctx).WithValues("sandboxID", sbx.GetName(), "envVars", envVars)
	initBody, err := json.Marshal(map[string]any{
		"envVars": envVars,
	})
	if err != nil {
		log.Error(err, "failed to marshal initBody")
		return err
	}
	request, err := http.NewRequest(http.MethodPost, "/init", bytes.NewBuffer(initBody))
	if err != nil {
		log.Error(err, "failed to create init request")
		return err
	}
	_, err = sbx.Request(request, "/init", models.EnvdPort)
	if err != nil {
		log.Error(err, "failed to init envd")
		return err
	}
	log.Info("envd inited", "cost", time.Since(start))
	return nil
}

func (sc *Controller) pauseAndResumeSandbox(r *http.Request, pause bool) (web.ApiResponse[struct{}], *web.ApiError) {
	id := r.PathValue("sandboxID")
	ctx := r.Context()
	log := klog.FromContext(ctx).WithValues("sandboxID", id)
	sbx, err := sc.manager.GetClaimedSandbox(id)
	if err != nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: fmt.Sprintf("Sandbox %s not found", id),
		}
	}
	if pause {
		if state := sbx.GetState(); state != consts.SandboxStateRunning {
			return web.ApiResponse[struct{}]{}, &web.ApiError{
				Code:    http.StatusConflict,
				Message: fmt.Sprintf("Sandbox %s is not running", id),
			}
		}
		if err = sbx.Pause(ctx); err != nil {
			return web.ApiResponse[struct{}]{}, &web.ApiError{
				Message: fmt.Sprintf("Failed to pause sandbox: %v", err),
			}
		}
		log.Info("sandbox paused")
	} else {
		if state := sbx.GetState(); state != consts.SandboxStatePaused {
			return web.ApiResponse[struct{}]{}, &web.ApiError{
				Code:    http.StatusConflict,
				Message: fmt.Sprintf("Sandbox %s is not paused", id),
			}
		}
		if err = sbx.Resume(ctx); err != nil {
			return web.ApiResponse[struct{}]{}, &web.ApiError{
				Message: fmt.Sprintf("Failed to resume sandbox: %v", err),
			}
		}
		log.Info("sandbox resumed")
	}
	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}

func (sc *Controller) convertToE2BSandbox(ctx context.Context, sbx infra.Sandbox) *models.Sandbox {
	log := klog.FromContext(ctx).V(DebugLevel).WithValues("sandbox", klog.KObj(sbx))

	sandbox := &models.Sandbox{
		SandboxID:   sbx.GetName(),
		TemplateID:  sbx.GetTemplate(),
		Domain:      sc.domain,
		EnvdVersion: "0.1.1",
		State:       sbx.GetState(),
	}
	route, ok := sc.manager.GetRoute(sbx.GetName())
	if ok {
		if sc.keys == nil {
			sandbox.EnvdAccessToken = "whatever"
		} else {
			if key, ok := sc.keys.LoadByID(route.Owner); ok {
				sandbox.EnvdAccessToken = key.Key
			} else {
				log.Info("skip convert sandbox route to e2b key: key for user not found")
			}
		}
	} else {
		log.Info("skip convert sandbox route to e2b key: route for sandbox not found")
	}
	// Just for example
	sandbox.StartedAt = sbx.GetCreationTimestamp().Format(time.RFC3339)
	sandbox.EndAt = sbx.GetCreationTimestamp().Add(1000 * time.Hour).Format(time.RFC3339)
	resource := sbx.GetResource()
	sandbox.CPUCount = resource.CPUMilli / 1000
	sandbox.MemoryMB = resource.MemoryMB
	return sandbox
}
