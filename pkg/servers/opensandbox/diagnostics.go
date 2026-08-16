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

package opensandbox

import (
	"fmt"
	"net/http"

	"github.com/openkruise/agents/pkg/servers/opensandbox/models"
	"github.com/openkruise/agents/pkg/servers/web"
)

// diagnosticScope resolves the required "scope" query parameter, defaulting
// to "all" when absent (the spec requires the parameter but every scope value
// maps to the same content in this initial adapter, see below).
func diagnosticScope(r *http.Request) string {
	if scope := r.URL.Query().Get("scope"); scope != "" {
		return scope
	}
	return models.DiagnosticScopeAll
}

// diagnosticSummary builds the one piece of diagnostic content this initial
// adapter can honestly produce from the infra.Sandbox interface today: its
// current lifecycle state, reason, and pod IP. It does not differentiate by
// scope (container/lifecycle/runtime/network/process) and does not include
// actual container log lines, since neither is exposed by infra.Sandbox yet
// (streaming pod logs would need a new Infra capability — see the package
// AGENTS.md "Known Gaps" and the design proposal's Non-Goals section).
func (sc *Controller) diagnosticSummary(sandboxID string, sbx interface {
	GetState() (string, string)
	GetIP() string
	Phase() string
}) string {
	state, reason := sbx.GetState()
	return fmt.Sprintf("sandboxId=%s state=%s reason=%s phase=%s podIP=%s",
		sandboxID, state, reason, sbx.Phase(), sbx.GetIP())
}

// GetDiagnosticLogs implements GET /v1/sandboxes/{sandboxId}/diagnostics/logs.
func (sc *Controller) GetDiagnosticLogs(r *http.Request) (web.ApiResponse[*models.DiagnosticContentResponse], *web.ApiError) {
	sandboxID := r.PathValue("sandboxId")
	sbx, apiErr := sc.getSandboxOfUser(r.Context(), sandboxID)
	if apiErr != nil {
		return web.ApiResponse[*models.DiagnosticContentResponse]{}, apiErr
	}
	content := sc.diagnosticSummary(sandboxID, sbx)
	return web.ApiResponse[*models.DiagnosticContentResponse]{
		Body: &models.DiagnosticContentResponse{
			SandboxID:     sandboxID,
			Kind:          models.DiagnosticKindLogs,
			Scope:         diagnosticScope(r),
			Delivery:      models.DiagnosticDeliveryInline,
			ContentType:   "text/plain; charset=utf-8",
			Content:       content,
			ContentLength: int64(len(content)),
			Truncated:     false,
			Warnings:      []string{"this initial adapter reports lifecycle state, not container log lines; see AGENTS.md Known Gaps"},
		},
	}, nil
}

// GetDiagnosticEvents implements GET /v1/sandboxes/{sandboxId}/diagnostics/events.
func (sc *Controller) GetDiagnosticEvents(r *http.Request) (web.ApiResponse[*models.DiagnosticContentResponse], *web.ApiError) {
	sandboxID := r.PathValue("sandboxId")
	sbx, apiErr := sc.getSandboxOfUser(r.Context(), sandboxID)
	if apiErr != nil {
		return web.ApiResponse[*models.DiagnosticContentResponse]{}, apiErr
	}
	content := sc.diagnosticSummary(sandboxID, sbx)
	return web.ApiResponse[*models.DiagnosticContentResponse]{
		Body: &models.DiagnosticContentResponse{
			SandboxID:     sandboxID,
			Kind:          models.DiagnosticKindEvents,
			Scope:         diagnosticScope(r),
			Delivery:      models.DiagnosticDeliveryInline,
			ContentType:   "text/plain; charset=utf-8",
			Content:       content,
			ContentLength: int64(len(content)),
			Truncated:     false,
			Warnings:      []string{"this initial adapter reports lifecycle state, not Kubernetes Events; see AGENTS.md Known Gaps"},
		},
	}, nil
}
