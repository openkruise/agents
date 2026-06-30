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
	"reflect"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	quotaspec "github.com/openkruise/agents/pkg/sandbox-manager/quota/spec"
)

const eventReconcileTimeout = 2 * time.Second

type AntiDriftConfig struct {
	Interval     time.Duration
	Grace        time.Duration
	CycleTimeout time.Duration
}

type leakedObservation struct {
	firstSeen time.Time
	confirmed bool
}

type AntiDriftDriver struct {
	cfg      AntiDriftConfig
	primary  PrimaryChecker
	subjects quotaspec.SubjectLister
	source   infra.QuotaSandboxSource
	backend  Backend

	mu            sync.Mutex
	subscription  infra.QuotaSandboxSubscription
	runDone       chan struct{}
	cycleCancel   context.CancelFunc
	seenLeaked    map[string]leakedObservation
	limitedOwners map[string]struct{}
	stopped       bool
	now           func() time.Time

	runOnce  sync.Once
	stopOnce sync.Once
	stopCh   chan struct{}
}

func NewAntiDriftDriver(cfg AntiDriftConfig, primary PrimaryChecker, subjects quotaspec.SubjectLister, source infra.QuotaSandboxSource, backend Backend) *AntiDriftDriver {
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
		cfg:           cfg,
		primary:       primary,
		subjects:      subjects,
		source:        source,
		backend:       backend,
		seenLeaked:    map[string]leakedObservation{},
		limitedOwners: map[string]struct{}{},
		now:           time.Now,
		stopCh:        make(chan struct{}),
	}
}

func (d *AntiDriftDriver) SetSubscription(sub infra.QuotaSandboxSubscription) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		if sub != nil {
			if err := sub.Remove(); err != nil {
				klog.Background().Error(err, "failed to remove quota anti-drift subscription after stop")
			}
		}
		return
	}
	d.subscription = sub
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
		d.runDone = make(chan struct{})
		d.mu.Unlock()

		go func() {
			defer close(d.runDone)
			// Derive a context that cancels when stopCh closes, so WaitPrimary
			// unblocks on Stop() even if the parent context is still alive.
			runCtx, cancel := context.WithCancel(ctx)
			go func() {
				select {
				case <-d.stopCh:
					cancel()
				case <-runCtx.Done():
				}
			}()
			defer cancel()
			d.runLoop(runCtx)
		}()
	})
}

func (d *AntiDriftDriver) runLoop(ctx context.Context) {
	for {
		if err := d.primary.WaitPrimary(ctx); err != nil {
			return
		}
		if !d.runWhilePrimary(ctx) {
			return
		}
	}
}

func (d *AntiDriftDriver) runWhilePrimary(ctx context.Context) bool {
	d.runCycle(ctx) // immediate cycle on primary acquire

	ticker := time.NewTicker(d.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-d.stopCh:
			return false
		case <-d.primary.PrimaryChanged():
			if !d.primary.IsPrimary() {
				d.cancelActiveCycleAndClearLeaked()
				return true // outer loop waits for primary again
			}
		case <-ticker.C:
			d.runCycle(ctx)
		}
	}
}

func (d *AntiDriftDriver) cancelActiveCycleAndClearLeaked() {
	d.mu.Lock()
	cycleCancel := d.cycleCancel
	d.cycleCancel = nil
	clear(d.seenLeaked)
	d.mu.Unlock()
	if cycleCancel != nil {
		cycleCancel()
	}
	antiDriftSkippedTotal.WithLabelValues("not_primary").Inc()
}

func (d *AntiDriftDriver) runCycle(ctx context.Context) {
	cycleCtx, cancel := context.WithTimeout(ctx, d.cycleTimeout())
	if !d.setCycleCancel(cancel) {
		cancel()
		return
	}
	if err := d.RunOnce(cycleCtx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		klog.FromContext(ctx).Error(err, "quota anti-drift cycle failed")
	}
	d.clearCycleCancel()
	cancel()
}

func (d *AntiDriftDriver) RunOnce(ctx context.Context) error {
	if d == nil {
		return nil
	}
	if !d.stillPrimary() {
		antiDriftSkippedTotal.WithLabelValues("not_primary").Inc()
		d.clearLeaked()
		return nil
	}
	if d.subjects == nil || d.source == nil || d.backend == nil {
		antiDriftSkippedTotal.WithLabelValues("not_ready").Inc()
		return nil
	}

	subjects, err := d.subjects.ListLimited(ctx)
	if err != nil {
		antiDriftSkippedTotal.WithLabelValues("key_store_error").Inc()
		d.clearLeaked()
		return nil
	}
	d.replaceLimitedOwners(limitedOwnerIDs(subjects))

	now := d.now()
	var firstErr error
	for _, subject := range subjects {
		if !d.stillPrimary() {
			antiDriftSkippedTotal.WithLabelValues("not_primary").Inc()
			d.clearLeaked()
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if subject.Quota == nil || !subject.Quota.IsLimited() {
			continue
		}
		stop, err := d.reconcileLimitedSubject(ctx, subject, now)
		if stop {
			return err
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

func limitedOwnerIDs(subjects []quotaspec.Subject) map[string]struct{} {
	limitedOwners := map[string]struct{}{}
	for _, subject := range subjects {
		if subject.Quota != nil && subject.Quota.IsLimited() {
			limitedOwners[subject.User] = struct{}{}
		}
	}
	return limitedOwners
}

func (d *AntiDriftDriver) reconcileLimitedSubject(ctx context.Context, subject quotaspec.Subject, now time.Time) (bool, error) {
	user := subject.User
	liveSandboxes, err := d.source.ListLiveQuotaSandboxesByOwner(ctx, user)
	if err != nil {
		antiDriftErrorsTotal.WithLabelValues("list_live").Inc()
		d.clearLeakedForKey(user)
		return false, err
	}

	haveEntries, err := d.backend.ListEntries(ctx, user)
	if err != nil {
		antiDriftErrorsTotal.WithLabelValues("list_entries").Inc()
		d.clearLeakedForKey(user)
		return false, err
	}

	var firstErr error
	liveLocks := make(map[string]struct{}, len(liveSandboxes))
	nextLeaked := map[string]leakedObservation{}
	for _, snapshot := range liveSandboxes {
		lockString := snapshot.LockString
		if lockString == "" {
			continue
		}
		liveLocks[lockString] = struct{}{}

		want := entryForSnapshot(snapshot)
		have, ok := haveEntries[lockString]
		if ok && entriesEqual(have, want) {
			continue
		}

		if !d.stillPrimary() {
			antiDriftSkippedTotal.WithLabelValues("not_primary").Inc()
			d.clearLeaked()
			return true, nil
		}
		if err := d.backend.Acquire(ctx, AcquireParams{
			User:       user,
			LockString: lockString,
			Footprint:  want.Footprint,
			Scopes:     want.Scopes,
			Enforce:    false,
			Limits:     subject.Quota.LimitedPairs(),
		}); err != nil {
			antiDriftErrorsTotal.WithLabelValues("acquire").Inc()
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	healthy := d.source.Healthy()
	for lockString := range haveEntries {
		if _, ok := liveLocks[lockString]; ok {
			continue
		}
		if !healthy {
			continue
		}

		obs := d.leakedObservation(user, lockString)
		if obs.firstSeen.IsZero() {
			obs.firstSeen = now
		}
		seenPreviousSuccessfulPass := obs.confirmed
		obs.confirmed = true
		nextLeaked[lockString] = obs

		if !seenPreviousSuccessfulPass || now.Sub(obs.firstSeen) < d.cfg.Grace {
			continue
		}
		if !d.stillPrimary() {
			antiDriftSkippedTotal.WithLabelValues("not_primary").Inc()
			d.clearLeaked()
			return true, nil
		}
		if err := d.backend.Release(ctx, user, lockString); err != nil {
			antiDriftErrorsTotal.WithLabelValues("release").Inc()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		delete(nextLeaked, lockString)
	}

	d.replaceLeakedForKey(user, nextLeaked)
	return false, firstErr
}

func (d *AntiDriftDriver) QuotaEventHandler() func(infra.QuotaSandboxEvent) {
	return func(event infra.QuotaSandboxEvent) {
		d.onQuotaEvent(event)
	}
}

func (d *AntiDriftDriver) Stop() {
	if d == nil {
		return
	}

	d.stopOnce.Do(func() {
		d.mu.Lock()
		d.stopped = true
		subscription := d.subscription
		d.subscription = nil
		d.seenLeaked = map[string]leakedObservation{}
		d.limitedOwners = map[string]struct{}{}
		done := d.runDone
		cycleCancel := d.cycleCancel
		close(d.stopCh)
		d.mu.Unlock()

		if cycleCancel != nil {
			cycleCancel()
		}
		if subscription != nil {
			if err := subscription.Remove(); err != nil {
				klog.Background().Error(err, "failed to remove quota anti-drift subscription")
			}
		}
		if done != nil {
			<-done
		}
	})
}

func (d *AntiDriftDriver) stillPrimary() bool {
	return d == nil || d.primary == nil || d.primary.IsPrimary()
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

func (d *AntiDriftDriver) onQuotaEvent(event infra.QuotaSandboxEvent) {
	if d == nil || d.backend == nil {
		return
	}
	if !d.stillPrimary() {
		antiDriftSkippedTotal.WithLabelValues("not_primary").Inc()
		d.clearLeaked()
		return
	}

	user := event.Snapshot.Owner
	lockString := event.Snapshot.LockString
	if user == "" || lockString == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), eventReconcileTimeout)
	defer cancel()
	if !d.ensureKnownLimited(ctx, user) {
		return
	}

	if event.Deleted || !event.Snapshot.Live {
		if d.source == nil || !d.source.Healthy() {
			antiDriftEventReleaseTotal.WithLabelValues("skipped_unhealthy").Inc()
			return
		}
		if !d.stillPrimary() {
			antiDriftSkippedTotal.WithLabelValues("not_primary").Inc()
			d.clearLeaked()
			return
		}
		if err := d.backend.Release(ctx, user, lockString); err != nil {
			antiDriftErrorsTotal.WithLabelValues("event_release").Inc()
			antiDriftEventReleaseTotal.WithLabelValues("error").Inc()
			return
		}
		antiDriftEventReleaseTotal.WithLabelValues("released").Inc()
		return
	}

	if !d.stillPrimary() {
		antiDriftSkippedTotal.WithLabelValues("not_primary").Inc()
		d.clearLeaked()
		return
	}
	want := entryForSnapshot(event.Snapshot)
	if err := d.backend.Acquire(ctx, AcquireParams{
		User:       user,
		LockString: lockString,
		Footprint:  want.Footprint,
		Scopes:     want.Scopes,
		Enforce:    false,
	}); err != nil {
		antiDriftErrorsTotal.WithLabelValues("event_acquire").Inc()
	}
}

func (d *AntiDriftDriver) clearLeaked() {
	d.mu.Lock()
	defer d.mu.Unlock()
	clear(d.seenLeaked)
}

func (d *AntiDriftDriver) replaceLimitedOwners(limitedOwners map[string]struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.limitedOwners = limitedOwners
}

func (d *AntiDriftDriver) isKnownLimited(user string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.limitedOwners[user]
	return ok
}

func (d *AntiDriftDriver) ensureKnownLimited(ctx context.Context, user string) bool {
	if d.isKnownLimited(user) {
		return true
	}
	if d.subjects == nil {
		return false
	}
	subject, ok := d.subjects.Load(ctx, user)
	if !ok || subject.Quota == nil || !subject.Quota.IsLimited() {
		return false
	}
	d.mu.Lock()
	d.limitedOwners[user] = struct{}{}
	d.mu.Unlock()
	return true
}

func (d *AntiDriftDriver) leakedObservation(user, lockString string) leakedObservation {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.seenLeaked[leakedKey(user, lockString)]
}

func (d *AntiDriftDriver) clearLeakedForKey(user string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deleteLeakedForKeyLocked(user)
}

func (d *AntiDriftDriver) replaceLeakedForKey(user string, leaked map[string]leakedObservation) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deleteLeakedForKeyLocked(user)
	for lockString, obs := range leaked {
		d.seenLeaked[leakedKey(user, lockString)] = obs
	}
}

func (d *AntiDriftDriver) deleteLeakedForKeyLocked(user string) {
	prefix := user + "\x00"
	for key := range d.seenLeaked {
		if strings.HasPrefix(key, prefix) {
			delete(d.seenLeaked, key)
		}
	}
}

func leakedKey(user, lockString string) string {
	return user + "\x00" + lockString
}

func entryForSnapshot(s infra.QuotaSandboxSnapshot) Entry {
	var scopes []quotaspec.QuotaScope
	if s.Running {
		scopes = []quotaspec.QuotaScope{quotaspec.ScopeRunning}
	}
	return Entry{
		Footprint: FootprintFromResource(s.Resource),
		Scopes:    scopes,
	}
}

func normalizeEntry(entry Entry) Entry {
	return Entry{
		Footprint: normalizeFootprint(entry.Footprint),
		Scopes:    normalizeScopes(entry.Scopes),
	}
}

func entriesEqual(have, want Entry) bool {
	have = normalizeEntry(have)
	want = normalizeEntry(want)
	return reflect.DeepEqual(have.Footprint, want.Footprint) && reflect.DeepEqual(have.Scopes, want.Scopes)
}
