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

package wake

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	"github.com/openkruise/agents/pkg/sandbox-gateway/server"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

// Waker resumes paused sandboxes by reusing the existing sandbox-manager
// connect Resume implementation (sandboxcr.Sandbox.Resume), then syncs the
// route locally and to peer gateways.
type Waker struct {
	cache cache.Provider
}

var defaultWaker atomic.Pointer[Waker]

// InitWaker initializes the package-level Waker with the given cache provider.
// Gateway startup guarantees a non-nil cacheProvider after cache.NewCache
// succeeds; passing nil clears the Waker (used by tests to restore the
// default state).
func InitWaker(cacheProvider cache.Provider) {
	if cacheProvider == nil {
		defaultWaker.Store(nil)
		return
	}
	defaultWaker.Store(&Waker{cache: cacheProvider})
}

// GetWaker returns the package-level Waker. Returns nil if InitWaker has not
// been called yet.
func GetWaker() *Waker {
	return defaultWaker.Load()
}

// SandboxUIDMatches reports whether the informer cache holds a sandbox at
// namespace/name whose UID equals uid. It returns false when uid is empty,
// the waker is nil, or the object cannot be read: a registry route pointing
// at a deleted object must never trigger a wake, and a recreated same-name
// sandbox carries a different UID.
func (w *Waker) SandboxUIDMatches(ctx context.Context, namespace, name string, uid types.UID) bool {
	if w == nil || uid == "" {
		return false
	}
	cli := w.cache.GetClient()
	var sbx agentsv1alpha1.Sandbox
	if err := cli.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sbx); err != nil {
		if !errors.IsNotFound(err) {
			klog.FromContext(ctx).Error(err, "read sandbox for stale-route UID check",
				"sandbox", klog.KRef(namespace, name))
		}
		return false
	}
	return sbx.UID == uid
}

// WakeEnabled checks the informer cache for the wake-on-ingress-traffic
// resume rule on the sandbox spec. This is a fallback for when the gateway
// controller's route registry hasn't yet synced the spec change. Returns
// false if the waker is nil or the sandbox cannot be read from cache.
func (w *Waker) WakeEnabled(ctx context.Context, namespace, name string) bool {
	if w == nil {
		return false
	}
	cli := w.cache.GetClient()
	var sbx agentsv1alpha1.Sandbox
	if err := cli.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sbx); err != nil {
		if !errors.IsNotFound(err) {
			klog.FromContext(ctx).Error(err, "read sandbox for wake-enabled check",
				"sandbox", klog.KRef(namespace, name))
		}
		return false
	}
	return utils.WakeOnIngressTrafficEnabled(&sbx)
}

// Wake resumes a paused sandbox by delegating to sandboxcr.Sandbox.Resume().
// The caller's context is used directly so that cancellation stops the wait
// for this caller only, without affecting other concurrent or future callers.
// Resume itself provides first-writer-wins dedup via retryUpdate.
//
// The caller derives the wake deadline itself (the filter wraps a detached
// context with Config.GetWakeTimeoutSeconds()); Wake does not wrap ctx again.
func (w *Waker) Wake(ctx context.Context, namespace, name string) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KRef(namespace, name))

	cli := w.cache.GetClient()

	// Read sandbox from informer cache (fast) to resolve the wake rule.
	var sbx agentsv1alpha1.Sandbox
	if err := cli.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sbx); err != nil {
		return err
	}

	// Reuse the existing sandbox-manager connect Resume implementation.
	// AsSandbox wraps the sandbox with the cache provider + storage registry.
	// Resume refreshes from API reader, so the sandbox object is re-fetched
	// with the latest state before patching.
	sandbox := sandboxcr.AsSandbox(&sbx, w.cache)

	// Determine the sandbox timeout mode, mirroring ParseTimeout in the
	// E2B connect path. This ensures we only touch deadlines of auto-pause
	// sandboxes and never convert never-timeout or shutdown-only sandboxes
	// into auto-pause mode.
	autoPause := sbx.Spec.PauseTime != nil
	hasDeadline := autoPause || sbx.Spec.ShutdownTime != nil

	var opts infra.ResumeOptions
	if hasDeadline && autoPause {
		// Auto-pause sandbox: ShutdownTime is set directly to the "forever"
		// retention horizon (now + 100 years): traffic woke the sandbox, so
		// it must not be auto-deleted by a stale or soon-to-expire retained
		// ShutdownTime; the next pause recomputes ShutdownTime from the
		// paused-retention policy. Without a ShutdownTime here, setTimeout()
		// inside Resume would nil it out.
		opts.Timeout = &timeout.Options{
			ShutdownTime: time.Now().Add(timeout.ForeverReservePausedSandboxDuration),
		}
		// Re-arm auto-pause only when the wake rule carries a positive
		// PauseTimeout; the rule is never defaulted. Without it the stale
		// PauseTime is cleared so the controller does not re-pause
		// immediately, and the sandbox keeps running until it is paused or
		// deleted again.
		if pauseTimeout := utils.WakeOnIngressTrafficPauseTimeout(&sbx); pauseTimeout > 0 {
			// The create API allows wake timeouts as short as 30s; reuse the
			// same Resume timeout floor the E2B Connect/Resume paths apply so
			// the fresh PauseTime cannot expire while the sandbox is still
			// resuming (the controller checks PauseTime before Resume handling
			// and would re-pause it mid-resume).
			requestedSeconds := int(pauseTimeout / time.Second)
			effectiveSeconds := timeout.ApplyResumeTimeoutFloor(requestedSeconds, timeout.DefaultMinResumeTimeoutSeconds)
			if effectiveSeconds != requestedSeconds {
				log.Info("wake timeout floor applied",
					"requestedSeconds", requestedSeconds,
					"effectiveSeconds", effectiveSeconds)
			}
			opts.Timeout.PauseTime = time.Now().Add(time.Duration(effectiveSeconds) * time.Second)
		}
	}
	// For never-timeout sandboxes (no PauseTime, no ShutdownTime) and
	// non-auto-pause sandboxes (ShutdownTime only, no PauseTime), we skip
	// setting a timeout. This preserves never-timeout semantics and avoids
	// injecting a PauseTime that would convert a shutdown-only sandbox into
	// auto-pause mode.
	log.Info("waking sandbox via traffic", "autoPause", autoPause,
		"hasDeadline", hasDeadline, "rearmPauseTime", opts.Timeout != nil && !opts.Timeout.PauseTime.IsZero())
	if err := sandbox.Resume(ctx, opts); err != nil {
		return err
	}
	log.Info("sandbox resumed successfully")

	// After Resume succeeds, sync route locally and with peers.
	// This mirrors the manager's syncRoute flow:
	//   1. Get route from refreshed sandbox
	//   2. Update local registry
	//   3. Sync to peer gateways
	route, err := sandbox.GetRoute()
	if err != nil {
		return fmt.Errorf("project route after wake: %w", err)
	}
	result := registry.GetRegistry().Upsert(route)
	sandboxroute.LogMutation(log, "upsert", route, result)

	if pm := server.GetPeerManager(); pm != nil {
		if err := proxy.SyncRouteWithPeers(ctx, pm, route, server.GetPeerOutbound()); err != nil {
			// Log but don't fail the wake — the local registry is updated,
			// and peers will eventually catch up via their own informers.
			log.Error(err, "failed to sync route with peers after wake")
		}
	}

	return nil
}
