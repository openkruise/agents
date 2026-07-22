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
// The cacheProvider argument is guaranteed to be non-nil by gateway startup
// after cache.NewCache succeeds.
func InitWaker(cacheProvider cache.Provider) {
	defaultWaker.Store(&Waker{cache: cacheProvider})
}

// GetWaker returns the package-level Waker. Returns nil if InitWaker has not
// been called yet.
func GetWaker() *Waker {
	return defaultWaker.Load()
}

// HasWakeAnnotation checks the informer cache for the wake-on-traffic annotation.
// This is a fallback for when the gateway controller's route registry hasn't
// yet synced the annotation change. Returns false if the waker is nil or the
// sandbox cannot be read from cache.
func (w *Waker) HasWakeAnnotation(ctx context.Context, namespace, name string) bool {
	if w == nil {
		return false
	}
	cli := w.cache.GetClient()
	var sbx agentsv1alpha1.Sandbox
	if err := cli.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sbx); err != nil {
		return false
	}
	return sbx.GetAnnotations()[agentsv1alpha1.AnnotationWakeOnTraffic] == agentsv1alpha1.True
}

// Wake resumes a paused sandbox by delegating to sandboxcr.Sandbox.Resume().
// The caller's context is used directly so that cancellation stops the wait
// for this caller only, without affecting other concurrent or future callers.
// Resume itself provides first-writer-wins dedup via retryUpdate.
//
// defaultWakeTimeout must be positive.  The caller (typically the filter) is
// responsible for providing a valid timeout via Config.GetWakeTimeoutSeconds()
// which defaults to 60s when the configured value is <= 0.
func (w *Waker) Wake(ctx context.Context, namespace, name string, defaultWakeTimeout time.Duration) error {
	wakeCtx, wakeCancel := context.WithTimeout(ctx, defaultWakeTimeout)
	defer wakeCancel()
	return w.wakeInternal(wakeCtx, namespace, name, defaultWakeTimeout)
}

// wakeInternal performs the actual wake work: reads annotations from cache,
// calls sandbox.Resume, and syncs the route.
func (w *Waker) wakeInternal(ctx context.Context, namespace, name string, defaultWakeTimeout time.Duration) error {
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
	if err := sandbox.Resume(ctx, opts); err != nil {
		return err
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
