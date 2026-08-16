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
	"net/http"

	"github.com/openkruise/agents/pkg/servers/web"
)

// pathPrefix namespaces every OpenSandbox route under /v1, per the lifecycle
// spec's basePath, so it can never collide with the E2B API's paths when both
// are registered on the same mux.
const pathPrefix = "/v1"

func (sc *Controller) registerRoutes() {
	registerRoute(sc.mux, http.MethodPost, "/sandboxes", sc.CreateSandbox, sc.CheckAPIKey)
	registerRoute(sc.mux, http.MethodGet, "/sandboxes", sc.ListSandboxes, sc.CheckAPIKey)
	registerRoute(sc.mux, http.MethodGet, "/sandboxes/{sandboxId}", sc.GetSandbox, sc.CheckAPIKey)
	registerRoute(sc.mux, http.MethodDelete, "/sandboxes/{sandboxId}", sc.DeleteSandbox, sc.CheckAPIKey)
	registerRoute(sc.mux, http.MethodPost, "/sandboxes/{sandboxId}/pause", sc.PauseSandbox, sc.CheckAPIKey)
	registerRoute(sc.mux, http.MethodPost, "/sandboxes/{sandboxId}/resume", sc.ResumeSandbox, sc.CheckAPIKey)
	registerRoute(sc.mux, http.MethodPost, "/sandboxes/{sandboxId}/renew-expiration", sc.RenewSandboxExpiration, sc.CheckAPIKey)
	registerRoute(sc.mux, http.MethodPatch, "/sandboxes/{sandboxId}/metadata", sc.PatchSandboxMetadata, sc.CheckAPIKey)

	registerRoute(sc.mux, http.MethodPost, "/sandboxes/{sandboxId}/snapshots", sc.CreateSnapshot, sc.CheckAPIKey)
	registerRoute(sc.mux, http.MethodGet, "/snapshots", sc.ListSnapshots, sc.CheckAPIKey)

	registerRoute(sc.mux, http.MethodGet, "/sandboxes/{sandboxId}/diagnostics/logs", sc.GetDiagnosticLogs, sc.CheckAPIKey)
	registerRoute(sc.mux, http.MethodGet, "/sandboxes/{sandboxId}/diagnostics/events", sc.GetDiagnosticEvents, sc.CheckAPIKey)
}

func registerRoute[T any](mux *http.ServeMux, method, path string, handler web.Handler[T], middlewares ...web.MiddleWare) {
	web.RegisterRoute(mux, method, pathPrefix+path, handler, middlewares...)
}
