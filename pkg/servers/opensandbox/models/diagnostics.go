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

package models

// DiagnosticKind distinguishes the two diagnostics endpoints.
type DiagnosticKind string

const (
	DiagnosticKindLogs   DiagnosticKind = "logs"
	DiagnosticKindEvents DiagnosticKind = "events"
)

// DiagnosticDelivery reports how DiagnosticContentResponse carries its
// payload. This initial adapter only ever produces "inline"; "url" is
// reserved for a future large-payload path (e.g. presigned log storage).
type DiagnosticDelivery string

const (
	DiagnosticDeliveryInline DiagnosticDelivery = "inline"
	DiagnosticDeliveryURL    DiagnosticDelivery = "url"
)

// DiagnosticContentResponse is the response body for both
// GET /v1/sandboxes/{sandboxId}/diagnostics/logs and .../diagnostics/events.
type DiagnosticContentResponse struct {
	SandboxID     string             `json:"sandboxId"`
	Kind          DiagnosticKind     `json:"kind"`
	Scope         string             `json:"scope"`
	Delivery      DiagnosticDelivery `json:"delivery"`
	ContentType   string             `json:"contentType"`
	Content       string             `json:"content,omitempty"`
	ContentURL    string             `json:"contentUrl,omitempty"`
	ContentLength int64              `json:"contentLength,omitempty"`
	ExpiresAt     string             `json:"expiresAt,omitempty"`
	Truncated     bool               `json:"truncated"`
	Warnings      []string           `json:"warnings,omitempty"`
}

// DiagnosticScopeAll is the "all" scope selector; the other spec values
// (container, lifecycle, runtime, network, process) are accepted but not yet
// differentiated by this initial adapter (see the opensandbox package's
// AGENTS.md for the tracked follow-up).
const DiagnosticScopeAll = "all"
