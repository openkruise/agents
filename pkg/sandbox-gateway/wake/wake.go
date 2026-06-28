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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	"github.com/openkruise/agents/pkg/sandbox-gateway/server"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
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
// Must be called once during gateway startup before any Wake calls.
func InitWaker(cacheProvider cache.Provider) {
	defaultWaker.Store(&Waker{cache: cacheProvider})
}

// GetWaker returns the package-level Waker. Returns nil if InitWaker has not
// been called yet.
func GetWaker() *Waker {
	return defaultWaker.Load()
}

// Wake resumes a paused sandbox by calling the existing sandbox-manager
// connect Resume implementation, then syncs the route locally and to peers.
// It does NOT reimplement spec patching or wait-for-running — it delegates
// entirely to sandboxcr.Sandbox.Resume().
//
// Resume internally handles:
//   - refreshFromAPIReader (fresh fetch from API server)
//   - IsSandboxResumable validation
//   - NewSandboxResumeTask (pre-acquired wait task)
//   - retryUpdate: patches Spec.Paused=false + setTimeout(PauseTime)
//   - resumeTask.Wait() — blocks until Ready condition is True
//   - InplaceRefresh + expectations
//   - Concurrent dedup: first-writer-wins via retryUpdate optimistic lock
//
// After Resume succeeds, syncRoute mirrors the manager's syncRoute flow:
//   - Get updated route from the refreshed sandbox (sandbox.GetRoute())
//   - Update local gateway registry (registry.GetRegistry().Update)
//   - Sync route to peer gateways (proxy.SyncRouteWithPeers)
func (w *Waker) Wake(ctx context.Context, namespace, name string, defaultWakeTimeout time.Duration) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KRef(namespace, name))

	cli := w.cache.GetClient()

	// Read sandbox from informer cache (fast) to get annotations.
	var sbx agentsv1alpha1.Sandbox
	if err := cli.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sbx); err != nil {
		return err
	}

	// Determine wake timeout: prefer annotation, fall back to filter default.
	wakeTimeout := defaultWakeTimeout
	if timeoutStr := sbx.Annotations[agentsv1alpha1.AnnotationWakeTimeoutSeconds]; timeoutStr != "" {
		if secs, err := strconv.Atoi(timeoutStr); err == nil && secs > 0 {
			wakeTimeout = time.Duration(secs) * time.Second
		}
	}

	// Reuse the existing sandbox-manager connect Resume implementation.
	// AsSandbox wraps the sandbox with the cache provider + storage registry.
	// Resume refreshes from API reader, so the sandbox object is re-fetched
	// with the latest state before patching.
	sandbox := sandboxcr.AsSandbox(&sbx, w.cache)

	var opts infra.ResumeOptions
	if wakeTimeout > 0 {
		opts.Timeout = &timeout.Options{
			PauseTime: time.Now().Add(wakeTimeout),
		}
		// Preserve existing ShutdownTime so timed sandboxes retain their
		// auto-delete deadline after wake. Without this, setTimeout()
		// inside Resume would nil out ShutdownTime, causing resource leaks.
		if sbx.Spec.ShutdownTime != nil {
			opts.Timeout.ShutdownTime = sbx.Spec.ShutdownTime.Time
		}
	}
	log.Info("waking sandbox via traffic", "wakeTimeout", wakeTimeout)

	// Retry Resume if the sandbox is still transitioning to paused state
	// (SandboxIsPausing). The controller auto-pause sets Spec.Paused=true
	// immediately, but the actual checkpointing takes time. During this
	// window, IsSandboxResumable returns "SandboxIsPausing". Retry with
	// a short backoff until the pause completes or the context expires.
	// Resume internally calls refreshFromAPIReader, so each attempt
	// re-fetches the latest sandbox state from the API server.
	const retryInterval = 2 * time.Second
	var resumeErr error
	for {
		resumeErr = sandbox.Resume(ctx, opts)
		if resumeErr == nil {
			break
		}
		// Only retry on "SandboxIsPausing" — other errors are fatal.
		if !strings.Contains(resumeErr.Error(), "SandboxIsPausing") {
			return resumeErr
		}
		log.Info("sandbox still pausing, retrying wake", "error", resumeErr)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryInterval):
			// Continue retrying; Resume will re-fetch from API server.
		}
	}

	log.Info("sandbox resumed successfully")

	// After Resume succeeds, sync route locally and with peers.
	// This mirrors the manager's syncRoute flow:
	//   1. Get route from refreshed sandbox
	//   2. Update local registry
	//   3. Sync to peer gateways
	route := sandbox.GetRoute()
	sandboxID := sandbox.GetSandboxID()
	registry.GetRegistry().Update(sandboxID, route)

	if pm := server.GetPeerManager(); pm != nil {
		if err := proxy.SyncRouteWithPeers(pm, route); err != nil {
			// Log but don't fail the wake — the local registry is updated,
			// and peers will eventually catch up via their own informers.
			log.Error(err, "failed to sync route with peers after wake")
		}
	}

	return nil
}
