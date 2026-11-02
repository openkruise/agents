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

package sandboxcr

import (
	"context"
	"time"

	infracache "github.com/openkruise/agents/pkg/cache"
	claimengine "github.com/openkruise/agents/pkg/lifecycle/claim"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

// The authoritative claim engine now lives in pkg/lifecycle/claim so the
// independent Sandbox controller can reuse it without depending on
// sandbox-manager. The clone path stays in sandboxcr and reaches the engine
// through these thin adapters, which preserve the sandboxcr-local names its
// tests override (DefaultCreateSandbox, DefaultCleanupTimeout) and translate
// between the fat *sandboxcr.Sandbox and the neutral *claim.Sandbox view.

var (
	// DefaultCreateSandbox is the clone-path create hook. Tests override it to
	// intercept creation.
	DefaultCreateSandbox = claimengine.CreateSandbox
	// DefaultCleanupTimeout bounds the detached context used to clean up an
	// abandoned checkpoint or a failed clone. Tests override it.
	DefaultCleanupTimeout = claimengine.DefaultCleanupTimeout
)

// waitForSandboxReady delegates to the neutral engine and copies the refreshed
// CR back onto the sandboxcr wrapper so subsequent clone steps observe the
// ready pod's capabilities (see the transport-resolution ordering in CloneSandbox).
func waitForSandboxReady(ctx context.Context, sbx *Sandbox, opts infra.ClaimSandboxOptions, cache infracache.Provider, observeCreation bool) (time.Duration, *infra.SandboxCreationResult, error) {
	view := claimengine.NewSandbox(sbx.Sandbox, sbx.Cache)
	cost, creation, err := claimengine.WaitForSandboxReady(ctx, view, opts, cache, observeCreation)
	sbx.Sandbox = view.Sandbox
	return cost, creation, err
}

// clearFailedSandbox delegates cleanup/reserve of a failed clone to the neutral
// engine so both paths share one admission-release and retention policy.
func clearFailedSandbox(ctx context.Context, sbx *Sandbox, err error, reserveFor *time.Duration, admission *infra.SandboxAdmission, lockString string) {
	if sbx == nil {
		return
	}
	claimengine.ClearFailedSandbox(ctx, claimengine.NewSandbox(sbx.Sandbox, sbx.Cache), err, reserveFor, admission, lockString)
}
