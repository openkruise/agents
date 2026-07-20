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

// Package e2b provides HTTP controllers for handling E2B sandbox requests.
package e2b

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils"
)

var (
	browserWebSocketReplacer = regexp.MustCompile(`^wss?://[^/]+`)
	claimedSandboxStates     = []string{agentsv1alpha1.SandboxStateRunning, agentsv1alpha1.SandboxStatePaused, agentsv1alpha1.SandboxStateDead}
	liveSandboxStates        = []string{agentsv1alpha1.SandboxStateRunning, agentsv1alpha1.SandboxStatePaused}
)

func (sc *Controller) getSandboxOfUser(ctx context.Context, sandboxID string, expectedStates []string) (infra.Sandbox, *web.ApiError) {
	log := klog.FromContext(ctx).WithValues("sandboxID", sandboxID)
	log.Info("getting sandbox of user")
	user := GetUserFromContext(ctx)
	if user == nil {
		log.Error(nil, "user not found")
		return nil, &web.ApiError{
			Message: "User not found",
		}
	}
	getCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	opts := infra.GetSandboxOptions{
		Namespace: sc.getNamespaceOfUser(user),
		SandboxID: sandboxID,
	}
	sbx, err := sc.manager.GetSandbox(getCtx, user.ID.String(), expectedStates, opts)
	if err != nil {
		log.Error(err, "failed to get sandbox")
		return nil, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: fmt.Sprintf("Cannot get sandbox %s: %v", sandboxID, err),
		}
	}
	if utils.IsReservedFailedSandbox(sbx.GetLabels()) {
		log.Info("sandbox is reserved after failure")
		return nil, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: fmt.Sprintf("Cannot get sandbox %s: sandbox not found", sandboxID),
		}
	}
	log.Info("sandbox found", "sandbox", klog.KObj(sbx))
	return sbx, nil
}

func (sc *Controller) getNamespaceOfUser(user *models.CreatedTeamAPIKey) string {
	team := keys.TeamForKey(user)
	// Keys in the admin team can access resources in cluster scope
	if team.Name == models.AdminTeamName {
		return ""
	}
	return team.Name
}

func (sc *Controller) convertToE2BSandbox(sbx infra.Sandbox, accessToken, domain string) *models.Sandbox {
	sandbox := &models.Sandbox{
		SandboxID:       sbx.GetSandboxID(),
		TemplateID:      sbx.GetTemplate(),
		Domain:          domain,
		EnvdVersion:     "0.2.10",
		EnvdAccessToken: accessToken,
		// TrafficAccessToken is the transient access token minted during claim; it
		// is empty unless the sandbox opted into access-token issuance, so omitempty
		// hides it on paths (list, etc.) that carry no token.
		TrafficAccessToken:           sbx.GetTrafficAccessToken(),
		TrafficAccessTokenExpiration: sbx.GetTrafficAccessTokenExpiration(),
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
	if annotations[models.ExtensionKeyReturnPodIP] == agentsv1alpha1.True {
		if ip := sbx.GetRoute().IP; ip != "" {
			sandbox.Metadata[models.MetadataKeyPodIP] = ip
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
	sandbox.CPUCount, sandbox.MemoryMB, sandbox.DiskSizeMB = e2bResource(sbx.GetResource())
	return sandbox
}

func e2bResource(resource infra.SandboxResource) (int64, int64, int64) {
	cpuMilli := resource.Limits.CPUMilli
	if cpuMilli == 0 {
		cpuMilli = resource.Requests.CPUMilli
	}
	memoryMB := resource.Limits.MemoryMB
	if memoryMB == 0 {
		memoryMB = resource.Requests.MemoryMB
	}
	diskSizeMB := resource.Limits.DiskSizeMB
	if diskSizeMB == 0 {
		diskSizeMB = resource.Requests.DiskSizeMB
	}
	return cpuMilli / 1000, memoryMB, diskSizeMB
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

// resolveSandboxDomain maps adapter domain-resolution errors to the E2B HTTP
// boundary. Callers must invoke it before state-changing operations so that a
// bad request cannot leave behind a partially created or resumed sandbox.
func (sc *Controller) resolveSandboxDomain(r *http.Request) (string, *web.ApiError) {
	if sc.domain != "" {
		return sc.domain, nil
	}
	domain, err := sc.adapter.GetDomain(r.Host, r.URL.Path)
	if err != nil {
		return "", &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: err.Error(),
		}
	}
	return domain, nil
}
