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

// Package opensandbox implements an OpenSandbox-compatible REST API layer
// that coexists in the same process as the E2B API (pkg/servers/e2b) and
// reuses its Manager-layer orchestration and team/API-key identity system
// instead of duplicating them, following the adapter pattern established by
// pkg/servers/e2b.
package opensandbox

import (
	"net/http"

	"github.com/openkruise/agents/pkg/cache"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
)

// Dependencies wires an already-initialized E2B Controller's orchestration
// into a new OpenSandbox Controller, so the two protocol adapters share one
// SandboxManager, one cache, and one API-key/team identity system rather than
// each building their own.
type Dependencies struct {
	// Manager is the shared sandbox-manager. Required.
	Manager *sandboxmanager.SandboxManager
	// Cache is the shared Kubernetes cache/client. Required.
	Cache cache.Provider
	// Keys is the shared E2B API-key storage. Nil disables OpenSandbox
	// authentication, mirroring E2B's --e2b-enable-auth=false behavior: every
	// caller is treated as the anonymous admin identity.
	Keys keys.KeyStorage
	// SystemNamespace is the namespace admin-identity requests fall back to,
	// same as E2B's --system-namespace.
	SystemNamespace string
	// MaxTimeoutSeconds bounds CreateSandboxRequest.Timeout. Zero disables the
	// ceiling (only the spec's 60s floor applies).
	MaxTimeoutSeconds int
	// DefaultTemplate is the SandboxTemplate an image-based CreateSandboxRequest
	// claims from before applying the requested image as an in-place update
	// (see sandbox.go's CreateSandbox). OpenSandbox's create contract has no
	// template/pool concept of its own — a caller supplies only a container
	// image — so this backend needs one pre-provisioned template to claim a
	// pod shape (resources, service account, volumes, ...) from. Required for
	// image-based creation to work; snapshot-restore creation does not need it.
	DefaultTemplate string
}

// Controller handles OpenSandbox-compatible sandbox-related operations. It
// holds no orchestration state of its own: every operation delegates to the
// shared Manager from Dependencies.
type Controller struct {
	manager           *sandboxmanager.SandboxManager
	cache             cache.Provider
	keys              keys.KeyStorage
	systemNamespace   string
	maxTimeoutSeconds int
	defaultTemplate   string

	mux *http.ServeMux
}

// NewController builds a Controller from deps but does not register routes;
// call RegisterRoutes with the mux to serve on (typically the same mux the
// E2B Controller already listens on, via its Mux() accessor).
func NewController(deps Dependencies) *Controller {
	return &Controller{
		manager:           deps.Manager,
		cache:             deps.Cache,
		keys:              deps.Keys,
		systemNamespace:   deps.SystemNamespace,
		maxTimeoutSeconds: deps.MaxTimeoutSeconds,
		defaultTemplate:   deps.DefaultTemplate,
	}
}

// RegisterRoutes registers every OpenSandbox route on mux. Safe to call with
// a mux that already serves other routes (e.g. the E2B API's), since every
// path is under the /v1 prefix reserved for this adapter.
func (sc *Controller) RegisterRoutes(mux *http.ServeMux) {
	sc.mux = mux
	sc.registerRoutes()
}
