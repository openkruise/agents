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

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/web"
)

// RoutePrefix is the path prefix the OpenSandbox Python SDK appends to the
// configured base URL (`ConnectionConfig._API_VERSION = "v1"`). All Phase 1
// routes are registered under this prefix so the SDK's default URL composition
// resolves correctly without client-side customization.
const RoutePrefix = "/v1"

// pathValueSandboxID is the Go ServeMux wildcard name for the sandbox identifier
// in sandbox-scoped routes. It matches the OpenSandbox spec's `{sandboxId}`
// path parameter (specs/sandbox-lifecycle.yml).
const pathValueSandboxID = "sandboxId"

// State sets forwarded to Manager.GetSandbox by loadOwnedSandbox. They mirror
// the E2B layer's claimedSandboxStates / liveSandboxStates so both protocols
// accept the same sandbox states on equivalent routes:
//   - claimedSandboxStates includes dead so a transitional or recently-dead
//     sandbox can still be inspected (describe); the
//     read handlers apply isSandboxViewable to hide genuinely gone sandboxes.
//   - liveSandboxStates excludes dead for operations that need a live sandbox
//     (resume, renew-expiration). Delete accepts any owned state for cleanup.
var (
	claimedSandboxStates = []string{
		agentsv1alpha1.SandboxStateRunning,
		agentsv1alpha1.SandboxStatePaused,
		agentsv1alpha1.SandboxStateDead,
	}
	liveSandboxStates = []string{
		agentsv1alpha1.SandboxStateRunning,
		agentsv1alpha1.SandboxStatePaused,
	}
)

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
	// MaxTimeout bounds the accepted `timeout` field in seconds. It mirrors
	// the E2B --e2b-max-timeout flag so both protocols share the same
	// operator-visible ceiling. The entrypoint validates this positive value
	// before assembling either API, as it does for E2B.
	MaxTimeout int
}

// Server holds the resolved dependencies for the OpenSandbox API handlers.
// Handlers are methods on *Server so they close over the dependency set
// without threading it through every call, mirroring the E2B layer's
// Controller pattern but without the lifecycle machinery (Init/Run) that the
// shared mux already owns.
type Server struct {
	manager    *sandboxmanager.SandboxManager
	keys       keys.KeyStorage
	maxTimeout int
}

// RegisterRoutes validates deps and registers the OpenSandbox routes on
// deps.Mux. It returns an error when deps is missing a required field so the
// entrypoint fails loudly at startup instead of serving a half-wired route.
//
// The route table includes create and the consolidated lifecycle handlers.
// Every sandbox-scoped route authenticates and loads an owned sandbox before
// the handler runs. Further endpoint integration is tracked in the proposal.
func RegisterRoutes(deps Deps) error {
	if deps.Mux == nil {
		return fmt.Errorf("opensandbox: Deps.Mux is required")
	}
	if deps.Manager == nil {
		return fmt.Errorf("opensandbox: Deps.Manager is required")
	}

	s := &Server{
		manager:    deps.Manager,
		keys:       deps.Keys,
		maxTimeout: deps.MaxTimeout,
	}

	auth := CheckApiKey(s.keys)
	sandboxes := RoutePrefix + "/sandboxes"
	sandboxByID := sandboxes + "/{" + pathValueSandboxID + "}"

	// Create.
	web.RegisterRouteWithErrorFormatter(deps.Mux, http.MethodPost, sandboxes, s.CreateSandbox, formatAPIError, auth)

	// List is owner-scoped inside Manager.ListSandboxes, so
	// it needs no per-sandbox owner middleware; the sandbox-scoped routes load
	// and ownership-check the target before the handler runs.
	web.RegisterRouteWithErrorFormatter(deps.Mux, http.MethodGet, sandboxes, s.ListSandboxes, formatAPIError, auth)
	web.RegisterRouteWithErrorFormatter(deps.Mux, http.MethodGet, sandboxByID, s.DescribeSandbox, formatAPIError, auth, s.loadOwnedSandbox(claimedSandboxStates))
	web.RegisterRouteWithErrorFormatter(deps.Mux, http.MethodDelete, sandboxByID, s.DeleteSandbox, formatAPIError, auth, s.loadOwnedSandbox(nil))
	web.RegisterRouteWithErrorFormatter(deps.Mux, http.MethodPost, sandboxByID+"/pause", s.PauseSandbox, formatAPIError, auth, s.loadOwnedSandbox(claimedSandboxStates))
	web.RegisterRouteWithErrorFormatter(deps.Mux, http.MethodPost, sandboxByID+"/resume", s.ResumeSandbox, formatAPIError, auth, s.loadOwnedSandbox(liveSandboxStates))
	web.RegisterRouteWithErrorFormatter(deps.Mux, http.MethodPost, sandboxByID+"/renew-expiration", s.RenewSandboxExpiration, formatAPIError, auth, s.loadOwnedSandbox(liveSandboxStates))
	return nil
}
