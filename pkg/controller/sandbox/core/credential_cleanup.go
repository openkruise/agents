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

package core

import (
	"context"
	"fmt"
	"time"

	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
)

const (
	// securityCredentialCleanupMaxRetries is the number of attempts at removing the
	// propagated security credential before the caller gives up. It matches
	// csiResetSignalMaxRetries on purpose: both run in the same window, against the
	// same runtime, and fail for the same transient reason.
	securityCredentialCleanupMaxRetries = 3
)

// securityCredentialCleanupRetryInterval is the backoff between credential removal
// attempts. It is a var rather than a const so tests can shorten it, matching
// csiResetSignalRetryInterval.
var securityCredentialCleanupRetryInterval = 1 * time.Second

// cleanupSecurityTokenFunc and securityTokenCleanerCountFunc are package-level
// seams over the identity cleaner registry so tests can drive credential removal
// without a live sandbox, matching writeRuntimeFileFunc. The registry itself is
// append-only by design, so the seams are the only way to exercise both the
// community path (no cleaners) and a registered one from here.
var (
	cleanupSecurityTokenFunc      = identity.CleanupSecurityToken
	securityTokenCleanerCountFunc = identity.SecurityTokenCleanerCount
)

// removePropagatedCredential deletes the security credential the identity
// provider wrote into the sandbox during claim, at a lifecycle point where the
// sandbox is about to stop being reachable or stop belonging to the claim the
// credential was issued for.
//
// Three call sites deliver that credential (claim, clone, and post-resume
// initialization, all through identity.ProcessSandboxToken) and #659 item 3 asks
// for it to be removed again "when the sandbox is paused or deleted", plus on
// recycle, where the sandbox changes hands. Every one of those runs through here,
// so the retry policy and the opt-in gate stay in one place.
//
// It is a no-op when the sandbox never opted into an ID token, gating on
// identity.IsIDTokenRequested exactly as the three issuance sites do, and a no-op
// again when no cleaner is registered, which is the community default.
//
// reason names the lifecycle event in the log line, so an operator reading a
// sandbox's history can tell a pause from a recycle.
//
// The caller decides what a failure means, and the three callers do not agree,
// on purpose. Recycle refuses to return the sandbox to the pool and pause refuses
// to proceed to the checkpoint or the pod deletion, because in both cases the
// sandbox survives the event and a credential left behind survives with it.
// Deletion is best effort: the object is going away, and a runtime that cannot be
// reached is a worse reason to wedge a sandbox behind its finalizer than it is to
// leave a file on a Pod that is being destroyed anyway.
func removePropagatedCredential(ctx context.Context, box *agentsv1alpha1.Sandbox, reason string) error {
	if !identity.IsIDTokenRequested(box) {
		return nil
	}
	if securityTokenCleanerCountFunc() == 0 {
		return nil
	}

	var lastErr error
	for attempt := 1; attempt <= securityCredentialCleanupMaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = cleanupSecurityTokenFunc(ctx, box)
		if lastErr == nil {
			klog.FromContext(ctx).Info("Removed propagated security credential",
				"sandbox", klog.KObj(box), "reason", reason, "attempt", attempt)
			return nil
		}
		if attempt < securityCredentialCleanupMaxRetries {
			klog.FromContext(ctx).Info("Failed to remove propagated security credential, will retry",
				"sandbox", klog.KObj(box), "reason", reason, "attempt", attempt, "error", lastErr)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(securityCredentialCleanupRetryInterval):
			}
		}
	}
	return fmt.Errorf("failed to remove propagated security credential before %s after %d attempts: %w",
		reason, securityCredentialCleanupMaxRetries, lastErr)
}

// Lifecycle events passed to removePropagatedCredential as reason.
const (
	credentialCleanupReasonRecycle = "recycle"
	credentialCleanupReasonPause   = "pause"
	credentialCleanupReasonDelete  = "delete"
)
