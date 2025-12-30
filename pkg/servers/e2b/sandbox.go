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

	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"k8s.io/klog/v2"
)

var (
	browserWebSocketReplacer = regexp.MustCompile(`^ws://[^/]+`)
)

func (sc *Controller) getSandboxOfUser(ctx context.Context, sandboxID string) (infra.Sandbox, *web.ApiError) {
	user := GetUserFromContext(ctx)
	if user == nil {
		return nil, &web.ApiError{
			Message: "User not found",
		}
	}
	sbx, err := sc.manager.GetClaimedSandbox(ctx, user.ID.String(), sandboxID)
	if err != nil {
		return nil, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: fmt.Sprintf("Sandbox %s not found", sandboxID),
		}
	}
	return sbx, nil
}

func (sc *Controller) initEnvd(ctx context.Context, sbx infra.Sandbox, envVars models.EnvVars, accessToken string) error {
	start := time.Now()
	log := klog.FromContext(ctx).WithValues("sandboxID", sbx.GetName(), "envVars", envVars)
	initBody, err := json.Marshal(map[string]any{
		"envVars":     envVars,
		"accessToken": accessToken,
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

func (sc *Controller) convertToE2BSandbox(sbx infra.Sandbox, accessToken string) *models.Sandbox {
	sandbox := &models.Sandbox{
		SandboxID:       sbx.GetSandboxID(),
		TemplateID:      sbx.GetTemplate(),
		Domain:          sc.domain,
		EnvdVersion:     "0.1.1",
		EnvdAccessToken: accessToken,
	}
	sandbox.State, _ = sbx.GetState()
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
	_, endAt := ParseTimeout(sbx)
	sandbox.EndAt = endAt.Format(time.RFC3339)
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

func ParseTimeout(sbx infra.Sandbox) (autoPause bool, timeoutAt time.Time) {
	timeout := sbx.GetTimeout()
	if timeout.PauseTime.IsZero() {
		return false, timeout.ShutdownTime
	}
	return true, timeout.PauseTime
}
