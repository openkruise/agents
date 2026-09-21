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

	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/web"
)

// RoutePrefix is the path prefix the OpenSandbox Python SDK appends to the
// configured base URL (`ConnectionConfig._API_VERSION = "v1"`). All Phase 1
// routes are registered under this prefix so the SDK's default URL composition
// resolves correctly without client-side customization.
const RoutePrefix = "/v1"

// Deps carries everything the OpenSandbox API layer needs to register its
// routes on an existing mux. It is assembled by the sandbox-manager entrypoint
// after the E2B controller has finished Init(), so the manager and key store
// are already wired.
//
// Deps is deliberately a plain struct rather than a functional-options builder:
// the OpenSandbox layer has exactly one construction site (cmd/sandbox-manager)
// and a handful of fields, so an options pattern would add ceremony without
// adding safety.
type Deps struct {
	// Mux is the HTTP mux the OpenSandbox routes register on. It is shared
	// with the E2B API so both protocols are served from the same listener
	// (issue #690: "Both API servers (E2B + OpenSandbox) can coexist in the
	// same sandbox-manager process, serving different path prefixes").
	Mux *http.ServeMux
	// Manager is the protocol-neutral sandbox orchestrator. The OpenSandbox
	// layer calls it directly; it must not reach into infra implementations.
	Manager *sandboxmanager.SandboxManager
	// Keys is the shared API-key storage. Nil disables authentication and
	// runs every request as the canonical anonymous caller, matching the E2B
	// layer's behavior when --e2b-enable-auth=false.
	Keys keys.KeyStorage
	// ImageAliases maps OpenSandbox image URIs to agents SandboxTemplate
	// names. It is injected at startup via the OPENSANDBOX_IMAGE_ALIASES
	// environment variable (typically a ConfigMap via envFrom) and is
	// read-only at request time. An empty or nil table makes every create
	// request fail with 500 (the reference server's status for an
	// unavailable creation source), which is the intended Phase 1 behavior: the
	// operator must explicitly declare which images are supported.
	ImageAliases map[string]string
	// MaxTimeout bounds the accepted `timeout` field in seconds. It mirrors
	// the E2B --e2b-max-timeout flag so both protocols share the same
	// operator-visible ceiling. A non-positive value disables the upper bound
	// (the lower bound still applies).
	MaxTimeout int
}

// Server holds the resolved dependencies for the OpenSandbox API handlers.
// Handlers are methods on *Server so they close over the dependency set
// without threading it through every call, mirroring the E2B layer's
// Controller pattern but without the lifecycle machinery (Init/Run) that the
// shared mux already owns.
type Server struct {
	manager      *sandboxmanager.SandboxManager
	keys         keys.KeyStorage
	imageAliases map[string]string
	maxTimeout   int
}

// RegisterRoutes validates deps and registers the Phase 1 OpenSandbox routes
// on deps.Mux. It returns an error when deps is missing a required field so
// the entrypoint fails loudly at startup instead of serving a half-wired
// route.
//
// Phase 1 registers exactly one route:
//
//	POST /v1/sandboxes → Server.CreateSandbox
//
// Later phases add describe/list/pause/resume/delete/timeout/metadata under
// the same prefix; each addition must be accompanied by a proposal update
// (see docs/proposals/20260918-opensandbox-compat.md).
func RegisterRoutes(deps Deps) error {
	if deps.Mux == nil {
		return fmt.Errorf("opensandbox: Deps.Mux is required")
	}
	if deps.Manager == nil {
		return fmt.Errorf("opensandbox: Deps.Manager is required")
	}

	s := &Server{
		manager:      deps.Manager,
		keys:         deps.Keys,
		imageAliases: deps.ImageAliases,
		maxTimeout:   deps.MaxTimeout,
	}

	auth := CheckApiKey(s.keys)
	web.RegisterRoute(deps.Mux, http.MethodPost, RoutePrefix+"/sandboxes", s.CreateSandbox, auth)
	return nil
}
