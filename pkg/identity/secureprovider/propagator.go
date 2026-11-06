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

package secureprovider

import (
	"context"
	"crypto/md5" //nolint:gosec // md5 is used purely as a non-cryptographic, deterministic identifier hash for sandbox token files
	"encoding/json"
	"errors"
	"fmt"

	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/utils/runtime"
)

// SecurityTokenPropagator is the propagator function signature used by the secure
// identity provider.
//
// It mirrors identity.SecurityTokenPropagator on purpose, but the secure provider
// keeps its own registry in this package so that its side effects only run when the
// secure provider is installed, and never through the default identity provider.
//
// rtOpts is the transport the caller resolved for this sandbox (see
// runtime.TransportOptionsFor). A propagator that reaches the sandbox runtime must
// forward it to every runtime call it makes so the credential rides the same
// transport as the /init handshake and the CSI mounts; an empty slice keeps the
// legacy plaintext path.
type SecurityTokenPropagator func(ctx context.Context, sbx *agentsv1alpha1.Sandbox, tokenResp *identity.TokenResponse,
	rtOpts ...runtime.Option) error

// propagators holds every SecurityTokenPropagator of the secure provider. It is mutated
// only during init() phase via RegisterPropagator and read live from
// secureIdentityProvider.PropagateSecurityToken — by the time PropagateSecurityToken
// runs, init() has long completed and the slice is therefore immutable in practice.
var propagators []SecurityTokenPropagator

// RegisterPropagator appends a propagator to the secure provider registry. It is
// intended to be called from init() of files inside this package.
// Concurrent calls are unsafe and not supported — registration must complete during
// the single-threaded init() phase.
func RegisterPropagator(p SecurityTokenPropagator) {
	propagators = append(propagators, p)
	klog.Infof("secure provider security token propagator registered, total: %d", len(propagators))
}

// DefaultSecurityTokenFilePath is the parent directory inside the sandbox runtime under
// which the agent security token file is materialized. Per-sandbox files are named after
// an MD5 digest of "<namespace>-<name>" so collisions across sandboxes are
// effectively impossible while keeping the path stable across sandbox recreations.
const DefaultSecurityTokenFilePath = "/var/opt/sandbox/agent-token/" //nolint:gosec // G101 false positive: a directory path, not a credential

// init registers WriteSecurityTokenToRuntime as the propagator that
// runs after every successful IssueToken call.
func init() {
	RegisterPropagator(WriteSecurityTokenToRuntime)
}

// WriteSecurityTokenToRuntime persists the issued security token onto the sandbox
// runtime as a JSON file at DefaultSecurityTokenFilePath/<hash>.token, where <hash> is
// the MD5 digest of "<namespace>-<name>".
//
// It is wired as a propagator via init() and is invoked synchronously during the claim
// flow right after the remote identity provider issues a token. The file write goes
// through the standard runtime.WriteFileWithRuntime helper so that all sandbox-runtime
// HTTP plumbing (URL resolution, X-Access-Token header, multipart body, timeouts) lives
// in a single place.
//
// Right after the write succeeds it prunes every *other* token file in the directory,
// so exactly one token file — this sandbox's — remains. A sandbox cloned from a
// checkpoint restores the source sandbox's token file, and the runtime derives the
// sandbox identity from the file name, so a leftover file would let the sandbox be
// resolved as a different one. Pruning after the write (rather than before) keeps the
// current credential present at every instant, which matters because propagation also
// runs periodically while the agent is live.
//
// Behavior contract:
//   - tokenResp must be non-nil and carry a non-empty AccessToken; otherwise the call
//     fails fast with an error so the caller can decide whether to retry.
//   - When the sandbox does not yet have a runtime URL annotation (e.g. pod still being
//     scheduled), the call is a no-op and returns nil — propagation is best-effort and
//     must not block claim.
//   - All other failures (JSON marshal, runtime HTTP error, stale-file prune failure)
//     are surfaced as errors with enough context to be actionable in operator logs.
//     The one tolerated prune failure is a sandbox image lacking the binaries the prune
//     needs, which is logged and skipped.
//
// rtOpts is forwarded to both the write and the prune so a TLS-capable sandbox
// receives — and is cleaned up over — HTTPS rather than the plaintext runtime
// port. This function never resolves the transport itself: the decision belongs
// to the caller that owns the TLS bundle.
func WriteSecurityTokenToRuntime(ctx context.Context, sbx *agentsv1alpha1.Sandbox, tokenResp *identity.TokenResponse,
	rtOpts ...runtime.Option) error {
	if sbx == nil {
		return fmt.Errorf("sandbox is nil, cannot write security token")
	}
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))

	if tokenResp == nil || tokenResp.AccessToken == "" {
		return fmt.Errorf("tokenResp is nil or AccessToken is empty, cannot write security token")
	}

	// Skip when the sandbox runtime URL is not yet exposed via annotation. The propagator
	// is invoked during claim and the runtime URL annotation is patched by the controller
	// asynchronously; treating "not yet ready" as a soft skip avoids spurious claim
	// failures.
	if runtime.GetRuntimeURL(sbx) == "" {
		log.Info("runtime URL not present on sandbox, skipping security token write")
		return nil
	}

	identStr := fmt.Sprintf("%s-%s", sbx.Namespace, sbx.Name)
	hash := fmt.Sprintf("%x", md5.Sum([]byte(identStr))) //nolint:gosec // identifier-only, not a security boundary
	fileName := fmt.Sprintf("%s.token", hash)
	filePath := DefaultSecurityTokenFilePath + fileName

	content, err := json.Marshal(tokenResp)
	if err != nil {
		return fmt.Errorf("failed to marshal tokenResp to json: %w", err)
	}

	res, err := runtime.WriteFileWithRuntime(ctx, runtime.WriteFileArgs{
		Sbx:      sbx,
		FilePath: filePath,
		Content:  content,
		// Owner-only intent for this credential file. Not transmitted today, so the file
		// still lands with the runtime default — see runtime.WriteFileArgs.Permissions.
		Permissions: 0600,
	}, rtOpts...)
	if err != nil {
		// The runtime error is deliberately not wrapped: the upload body is the sandbox
		// access token, and a runtime that echoes a rejected upload back in its error body
		// would put the credential into the log PropagateSecurityToken writes.
		return fmt.Errorf("failed to write security token to runtime: %s (path %s)",
			describeRemoteFailure(err, res.StatusCode), filePath)
	}

	log.Info("security token written to runtime", "filePath", filePath)

	// Drop any token file left behind by a checkpoint restore now that the current one
	// is in place, keeping the file just written. A runtime that does not serve the
	// Filesystem service is tolerated: an older agent-runtime cannot prune, and failing
	// here would break claim, resume and refresh permanently for that runtime.
	if err := pruneStaleSecurityTokenFilesFn(ctx, sbx, fileName, rtOpts...); err != nil {
		if errors.Is(err, runtime.ErrRuntimeFilesystemUnsupported) {
			log.Info("skipped stale security token prune", "reason", err.Error())
			return nil
		}
		return fmt.Errorf("failed to prune stale security token files: %w", err)
	}
	return nil
}
