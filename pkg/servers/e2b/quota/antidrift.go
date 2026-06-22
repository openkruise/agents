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

package quota

import (
	"context"
	"errors"
	"sync"
	"time"

	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	cachepkg "github.com/openkruise/agents/pkg/cache"
)

const eventReleaseTimeout = 5 * time.Second

type AntiDriftConfig struct {
	Interval     time.Duration
	Grace        time.Duration
	CycleTimeout time.Duration
}

type AntiDriftDriver struct {
	cfg     AntiDriftConfig
	primary PrimaryChecker
	keys    LimitedKeyStore
	cache   LiveLockstringCache
	backend Backend

	mu           sync.Mutex
	registration cachepkg.SandboxEventHandlerRegistration
	runDone      chan struct{}
	cycleCancel  context.CancelFunc
	stopped      bool
	eventWG      sync.WaitGroup

	runOnce  sync.Once
	stopOnce sync.Once
	stopCh   chan struct{}
}

type releaseResultBackend interface {
	ReleaseResult(ctx context.Context, apiKeyID, lockString string) (bool, error)
}

func NewAntiDriftDriver(cfg AntiDriftConfig, primary PrimaryChecker, keys LimitedKeyStore, liveCache LiveLockstringCache, backend Backend) *AntiDriftDriver {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.Grace <= 0 {
		cfg.Grace = 10 * time.Minute
	}
	if cfg.CycleTimeout <= 0 {
		cfg.CycleTimeout = 30 * time.Second
	}

	return &AntiDriftDriver{
		cfg:     cfg,
		primary: primary,
		keys:    keys,
		cache:   liveCache,
		backend: backend,
		stopCh:  make(chan struct{}),
	}
}

func (d *AntiDriftDriver) SetEventRegistration(reg cachepkg.SandboxEventHandlerRegistration) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		if reg != nil {
			_ = reg.Remove()
		}
		return
	}
	d.registration = reg
	d.mu.Unlock()
}

func (d *AntiDriftDriver) Run(ctx context.Context) {
	if d == nil {
		return
	}

	d.runOnce.Do(func() {
		d.mu.Lock()
		if d.stopped {
			d.mu.Unlock()
			return
		}
		done := make(chan struct{})
		d.runDone = done
		d.mu.Unlock()

		go func() {
			defer close(done)
			d.runLoop(ctx)
		}()
	})
}

func (d *AntiDriftDriver) runLoop(ctx context.Context) {
	ticker := time.NewTicker(d.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stopCh:
			return
		case <-ticker.C:
			cycleCtx, cancel := context.WithTimeout(ctx, d.cycleTimeout())
			if !d.setCycleCancel(cancel) {
				cancel()
				continue
			}
			if err := d.RunOnce(cycleCtx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				klog.FromContext(ctx).Error(err, "quota anti-drift cycle failed")
			}
			d.clearCycleCancel()
			cancel()
		}
	}
}

func (d *AntiDriftDriver) cycleTimeout() time.Duration {
	timeout := d.cfg.CycleTimeout
	if d.cfg.Interval < timeout {
		timeout = d.cfg.Interval
	}
	return timeout
}

func (d *AntiDriftDriver) setCycleCancel(cancel context.CancelFunc) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return false
	}
	d.cycleCancel = cancel
	return true
}

func (d *AntiDriftDriver) clearCycleCancel() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cycleCancel = nil
}

func (d *AntiDriftDriver) Stop() {
	if d == nil {
		return
	}

	d.stopOnce.Do(func() {
		d.mu.Lock()
		d.stopped = true
		registration := d.registration
		d.registration = nil
		done := d.runDone
		cycleCancel := d.cycleCancel
		close(d.stopCh)
		d.mu.Unlock()

		if cycleCancel != nil {
			cycleCancel()
		}
		if registration != nil {
			_ = registration.Remove()
		}
		if done != nil {
			<-done
		}
		d.eventWG.Wait()
	})
}

func (d *AntiDriftDriver) beginEventRelease() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return false
	}
	d.eventWG.Add(1)
	return true
}

func (d *AntiDriftDriver) runEventRelease(fn func(context.Context)) {
	if !d.beginEventRelease() {
		return
	}
	defer d.eventWG.Done()

	ctx, cancel := context.WithTimeout(context.Background(), eventReleaseTimeout)
	defer cancel()
	fn(ctx)
}

func (d *AntiDriftDriver) RunOnce(ctx context.Context) error {
	if d == nil {
		return nil
	}
	if d.primary != nil && !d.primary.IsPrimary() {
		antiDriftSkippedTotal.WithLabelValues("not_primary").Inc()
		return nil
	}
	if d.keys == nil || d.cache == nil || d.backend == nil {
		return nil
	}

	now := time.Now()
	keys, err := d.keys.ListLimited(ctx)
	if err != nil {
		antiDriftSkippedTotal.WithLabelValues("list_limited_error").Inc()
		antiDriftErrorsTotal.WithLabelValues("list_limited").Inc()
		return err
	}

	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		if key == nil || key.QuotaSpec == nil || !key.QuotaSpec.IsLimited() {
			continue
		}

		keyID := key.ID.String()
		live, err := d.cache.ListLiveLockstringsByOwner(ctx, cachepkg.ListLiveLockstringsByOwnerOptions{Owner: keyID})
		if err != nil {
			antiDriftSkippedTotal.WithLabelValues("live_list_error").Inc()
			antiDriftErrorsTotal.WithLabelValues("live_list").Inc()
			klog.FromContext(ctx).Error(err, "quota anti-drift live probe failed", "apiKeyID", keyID)
			continue
		}

		redisLive, err := d.backend.List(ctx, keyID)
		if err != nil {
			antiDriftSkippedTotal.WithLabelValues("backend_list_error").Inc()
			antiDriftErrorsTotal.WithLabelValues("backend_list").Inc()
			klog.FromContext(ctx).Error(err, "quota anti-drift backend list failed", "apiKeyID", keyID)
			continue
		}

		cacheLive := make(map[string]time.Time, len(live))
		addFailed := false
		for _, entry := range live {
			if err := ctx.Err(); err != nil {
				return err
			}
			cacheLive[entry.LockString] = entry.CreationTimestamp
			if _, exists := redisLive[entry.LockString]; exists {
				continue
			}
			if now.Sub(entry.CreationTimestamp) < d.cfg.Grace {
				continue
			}
			if err := d.backend.AddObserved(ctx, keyID, entry.LockString, entry.CreationTimestamp); err != nil {
				addFailed = true
				antiDriftErrorsTotal.WithLabelValues("add_observed").Inc()
				klog.FromContext(ctx).Error(err, "quota anti-drift add observed failed", "apiKeyID", keyID, "lockString", entry.LockString)
			}
		}

		if addFailed {
			antiDriftSkippedTotal.WithLabelValues("add_observed_error").Inc()
			continue
		}
		if !d.cache.RemoveSafe() {
			antiDriftSkippedTotal.WithLabelValues("remove_unsafe").Inc()
			continue
		}

		for lockString, acquiredAt := range redisLive {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, exists := cacheLive[lockString]; exists {
				continue
			}
			if now.Sub(acquiredAt) < d.cfg.Grace {
				continue
			}
			if err := d.backend.Release(ctx, keyID, lockString); err != nil {
				antiDriftErrorsTotal.WithLabelValues("release").Inc()
				klog.FromContext(ctx).Error(err, "quota anti-drift release failed", "apiKeyID", keyID, "lockString", lockString)
			}
		}
	}

	return nil
}

func (d *AntiDriftDriver) SandboxEventHandler() toolscache.ResourceEventHandler {
	return toolscache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj any) {
			d.runEventRelease(func(ctx context.Context) {
				d.releaseOnTransitionToNotLive(ctx, oldObj, newObj)
			})
		},
		DeleteFunc: func(obj any) {
			d.runEventRelease(func(ctx context.Context) {
				d.releaseDeleted(ctx, obj)
			})
		},
	}
}

func (d *AntiDriftDriver) releaseOnTransitionToNotLive(ctx context.Context, oldObj, newObj any) {
	newSandbox, ok := newObj.(*agentsv1alpha1.Sandbox)
	if !ok || newSandbox == nil || cachepkg.IsLiveForQuota(newSandbox) {
		return
	}

	oldSandbox, ok := oldObj.(*agentsv1alpha1.Sandbox)
	if ok && oldSandbox != nil && !cachepkg.IsLiveForQuota(oldSandbox) {
		return
	}

	owner, lockString := quotaIdentityFromSandbox(newSandbox)
	d.releaseWithProbe(ctx, owner, lockString)
}

func (d *AntiDriftDriver) releaseDeleted(ctx context.Context, obj any) {
	switch deleted := obj.(type) {
	case *agentsv1alpha1.Sandbox:
		owner, lockString := quotaIdentityFromSandbox(deleted)
		d.releaseWithProbe(ctx, owner, lockString)
	case toolscache.DeletedFinalStateUnknown:
		sbx, ok := deleted.Obj.(*agentsv1alpha1.Sandbox)
		if !ok || sbx == nil {
			return
		}
		owner, lockString := quotaIdentityFromSandbox(sbx)
		d.releaseWithProbe(ctx, owner, lockString)
	case *toolscache.DeletedFinalStateUnknown:
		if deleted == nil {
			return
		}
		sbx, ok := deleted.Obj.(*agentsv1alpha1.Sandbox)
		if !ok || sbx == nil {
			return
		}
		owner, lockString := quotaIdentityFromSandbox(sbx)
		d.releaseWithProbe(ctx, owner, lockString)
	}
}

func (d *AntiDriftDriver) releaseWithProbe(ctx context.Context, owner, lockString string) {
	if d == nil {
		return
	}
	if d.primary != nil && !d.primary.IsPrimary() {
		antiDriftEventReleaseTotal.WithLabelValues("skip_not_primary").Inc()
		return
	}
	if d.cache == nil || !d.cache.RemoveSafe() {
		antiDriftEventReleaseTotal.WithLabelValues("skip_remove_unsafe").Inc()
		return
	}
	if owner == "" {
		antiDriftEventReleaseTotal.WithLabelValues("skip_missing_owner").Inc()
		return
	}
	if lockString == "" {
		antiDriftEventReleaseTotal.WithLabelValues("skip_missing_lockstring").Inc()
		return
	}

	antiDriftEventProbeTotal.WithLabelValues("attempt").Inc()
	live, err := d.cache.ListLiveLockstringsByOwner(ctx, cachepkg.ListLiveLockstringsByOwnerOptions{Owner: owner})
	if err != nil {
		antiDriftEventProbeTotal.WithLabelValues("error").Inc()
		antiDriftEventReleaseTotal.WithLabelValues("skip_probe_error").Inc()
		antiDriftErrorsTotal.WithLabelValues("event_probe").Inc()
		klog.FromContext(ctx).Error(err, "quota anti-drift event probe failed", "apiKeyID", owner, "lockString", lockString)
		return
	}
	for _, entry := range live {
		if entry.LockString == lockString {
			antiDriftEventProbeTotal.WithLabelValues("still_live").Inc()
			antiDriftEventReleaseTotal.WithLabelValues("skip_still_live").Inc()
			return
		}
	}
	antiDriftEventProbeTotal.WithLabelValues("gone").Inc()

	if d.backend == nil {
		return
	}
	result, err := d.releaseQuotaEntry(ctx, owner, lockString)
	if err != nil {
		antiDriftEventReleaseTotal.WithLabelValues("error").Inc()
		antiDriftErrorsTotal.WithLabelValues("event_release").Inc()
		klog.FromContext(ctx).Error(err, "quota anti-drift event release failed", "apiKeyID", owner, "lockString", lockString)
		return
	}

	antiDriftEventReleaseTotal.WithLabelValues(result).Inc()
}

func (d *AntiDriftDriver) releaseQuotaEntry(ctx context.Context, owner, lockString string) (string, error) {
	if backend, ok := d.backend.(releaseResultBackend); ok {
		deleted, err := backend.ReleaseResult(ctx, owner, lockString)
		if err != nil {
			return "", err
		}
		if deleted {
			return "released", nil
		}
		return "noop", nil
	}

	if err := d.backend.Release(ctx, owner, lockString); err != nil {
		return "", err
	}
	return "released", nil
}

func quotaIdentityFromSandbox(sbx *agentsv1alpha1.Sandbox) (string, string) {
	if sbx == nil {
		return "", ""
	}

	annotations := sbx.GetAnnotations()
	if len(annotations) == 0 {
		return "", ""
	}

	return annotations[agentsv1alpha1.AnnotationOwner], annotations[agentsv1alpha1.AnnotationLock]
}
