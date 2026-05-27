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
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
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

// isCustomizedRequest reports whether the inbound request uses the customized
// path-prefix adapter (e.g. /kruise/api/...). It mirrors
// adapters.E2BAdapter.ChooseAdapter so that resolveSandboxDomain and BrowserUse
// agree on the deployment shape for the same request.
func (sc *Controller) isCustomizedRequest(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, adapters.CustomPrefix)
}

// splitHostPort splits a "host[:port]" authority without requiring a port.
// It preserves bracketed IPv6 hosts and treats raw IPv6 as a host without
// an explicit port.
func splitHostPort(authority string) (host, port string) {
	if strings.HasPrefix(authority, "[") {
		end := strings.Index(authority, "]")
		if end < 0 {
			return authority, ""
		}
		host = authority[:end+1]
		if len(authority) > end+1 && authority[end+1] == ':' {
			return host, authority[end+2:]
		}
		return authority, ""
	}
	if strings.Count(authority, ":") > 1 {
		return authority, ""
	}
	idx := strings.LastIndex(authority, ":")
	if idx < 0 {
		return authority, ""
	}
	return authority[:idx], authority[idx+1:]
}

// resolveSandboxDomain derives the user-facing E2B domain for the response
// body or sandbox URL. When --e2b-domain is configured (sc.domain != ""),
// that static value is returned as-is. Otherwise the resolver reads the
// inbound Host header and applies native vs. customized rules per
// docs/specs/2026-05-27-dynamic-sandbox-domain-design.md.
//
// Returns *web.ApiError with HTTP 400 when no domain can be derived from the
// inbound request host. The
// resolver never mutates server state; callers must invoke it before any
// state-changing operation so that a 400 cannot leave behind a partially
// created or resumed sandbox.
func (sc *Controller) resolveSandboxDomain(r *http.Request) (string, *web.ApiError) {
	if sc.domain != "" {
		return sc.domain, nil
	}
	authority := r.Host
	if authority == "" {
		return "", &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "cannot resolve sandbox domain: empty host",
		}
	}
	if sc.isCustomizedRequest(r) {
		return authority, nil
	}
	host, port := splitHostPort(authority)
	host = strings.ToLower(host)
	host = strings.TrimPrefix(host, "api.")
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return "", &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "cannot resolve sandbox domain: empty host",
		}
	}
	if port == "" {
		return host, nil
	}
	return host + ":" + port, nil
}
