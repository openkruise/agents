// Package e2b provides HTTP controllers for handling E2B sandbox requests.
package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"k8s.io/klog/v2"
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
	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Message: "User not found",
		}
	}
	sbx, err := sc.manager.GetClaimedSandbox(ctx, user.ID.String(), id)
	if err != nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: fmt.Sprintf("Sandbox %s not found", id),
		}
	}
	if pause {
		if state, reason := sbx.GetState(); state != v1alpha1.SandboxStateRunning {
			log.Info("skip pause sandbox: sandbox is not running", "state", state, "reason", reason)
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
		if state, reason := sbx.GetState(); state != v1alpha1.SandboxStatePaused {
			log.Info("skip resume sandbox: sandbox is not paused", "state", state, "reason", reason)
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
	log := klog.FromContext(ctx).V(consts.DebugLogLevel).WithValues("sandbox", klog.KObj(sbx))

	sandbox := &models.Sandbox{
		SandboxID:   sbx.GetSandboxID(),
		TemplateID:  sbx.GetTemplate(),
		Domain:      sc.domain,
		EnvdVersion: "0.1.1",
	}
	sandbox.State, _ = sbx.GetState()
	route, ok := sc.manager.GetRoute(sbx.GetSandboxID())
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

	annotations := sbx.GetAnnotations()
	labels := sbx.GetLabels()

	sandbox.Metadata = make(map[string]string, len(annotations)+len(labels))

	// try to read labels as metadata for backward compatibility
	for key, val := range labels {
		if ValidateMetadataKey(key) {
			sandbox.Metadata[key] = val
		}
	}

	for key, val := range annotations {
		if ValidateMetadataKey(key) {
			sandbox.Metadata[key] = val
		}
	}

	claimTime, err := sbx.GetClaimTime()
	if err != nil {
		sandbox.StartedAt = "<unknown>"
	} else {
		sandbox.StartedAt = claimTime.Format(time.RFC3339)
	}
	sandbox.EndAt = sbx.GetTimeout().Format(time.RFC3339)
	resource := sbx.GetResource()
	sandbox.CPUCount = resource.CPUMilli / 1000
	sandbox.MemoryMB = resource.MemoryMB
	sandbox.DiskSizeMB = resource.DiskSizeMB
	return sandbox
}

func ValidateMetadataKey(key string) bool {
	for _, prefix := range BlackListPrefix {
		if strings.HasPrefix(key, prefix) {
			return false
		}
	}
	return true
}
