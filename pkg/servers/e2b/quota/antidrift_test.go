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
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	cachepkg "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

type fakeLimitedKeyStore struct {
	keys []*models.CreatedTeamAPIKey
	err  error
}

func (f fakeLimitedKeyStore) ListLimited(context.Context) ([]*models.CreatedTeamAPIKey, error) {
	return f.keys, f.err
}

type blockingLimitedKeyStore struct {
	started chan struct{}
	done    chan struct{}
}

func newBlockingLimitedKeyStore() *blockingLimitedKeyStore {
	return &blockingLimitedKeyStore{
		started: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (f *blockingLimitedKeyStore) ListLimited(ctx context.Context) ([]*models.CreatedTeamAPIKey, error) {
	close(f.started)
	<-ctx.Done()
	close(f.done)
	return nil, ctx.Err()
}

type fakeLiveCache struct {
	live        []cachepkg.LiveLockstring
	liveByOwner map[string][]cachepkg.LiveLockstring
	liveErr     error
	removeSafe  bool
	sawDeadline *atomic.Bool
}

func (f fakeLiveCache) ListLiveLockstringsByOwner(ctx context.Context, opts cachepkg.ListLiveLockstringsByOwnerOptions) ([]cachepkg.LiveLockstring, error) {
	if f.sawDeadline != nil {
		if _, ok := ctx.Deadline(); ok {
			f.sawDeadline.Store(true)
		}
	}
	if f.liveByOwner != nil {
		return f.liveByOwner[opts.Owner], f.liveErr
	}
	return f.live, f.liveErr
}

func (f fakeLiveCache) RemoveSafe() bool {
	return f.removeSafe
}

type fakePrimary struct {
	primary bool
}

func (f fakePrimary) IsPrimary() bool {
	return f.primary
}

type recordingBackend struct {
	have               map[string]map[string]time.Time
	added              map[string][]string
	removed            map[string][]string
	listErr            error
	addErr             error
	releaseErr         error
	releaseSawDeadline atomic.Bool
}

func newRecordingBackend() *recordingBackend {
	return &recordingBackend{
		have:    map[string]map[string]time.Time{},
		added:   map[string][]string{},
		removed: map[string][]string{},
	}
}

func (r *recordingBackend) Acquire(context.Context, string, string, int64) error {
	return nil
}

func (r *recordingBackend) AddObserved(_ context.Context, apiKeyID, lockString string, acquiredAt time.Time) error {
	if r.addErr != nil {
		return r.addErr
	}
	r.added[apiKeyID] = append(r.added[apiKeyID], lockString)
	if r.have[apiKeyID] == nil {
		r.have[apiKeyID] = map[string]time.Time{}
	}
	r.have[apiKeyID][lockString] = acquiredAt
	return nil
}

func (r *recordingBackend) List(_ context.Context, apiKeyID string) (map[string]time.Time, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}

	out := map[string]time.Time{}
	for lock, ts := range r.have[apiKeyID] {
		out[lock] = ts
	}
	return out, nil
}

func (r *recordingBackend) Release(ctx context.Context, apiKeyID, lockString string) error {
	_, err := r.ReleaseResult(ctx, apiKeyID, lockString)
	return err
}

func (r *recordingBackend) ReleaseResult(ctx context.Context, apiKeyID, lockString string) (bool, error) {
	if _, ok := ctx.Deadline(); ok {
		r.releaseSawDeadline.Store(true)
	}
	if r.releaseErr != nil {
		return false, r.releaseErr
	}
	_, existed := r.have[apiKeyID][lockString]
	r.removed[apiKeyID] = append(r.removed[apiKeyID], lockString)
	delete(r.have[apiKeyID], lockString)
	return existed, nil
}

func (r *recordingBackend) DeleteSubject(context.Context, string) error {
	return nil
}

type blockingReleaseBackend struct {
	*recordingBackend

	started chan struct{}
	unblock chan struct{}
	once    sync.Once
}

func newBlockingReleaseBackend() *blockingReleaseBackend {
	return &blockingReleaseBackend{
		recordingBackend: newRecordingBackend(),
		started:          make(chan struct{}),
		unblock:          make(chan struct{}),
	}
}

func (b *blockingReleaseBackend) ReleaseResult(ctx context.Context, apiKeyID, lockString string) (bool, error) {
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.unblock:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return b.recordingBackend.ReleaseResult(ctx, apiKeyID, lockString)
}

type recordingRegistration struct {
	removed atomic.Bool
}

func (r *recordingRegistration) HasSynced() bool {
	return true
}

func (r *recordingRegistration) Remove() error {
	r.removed.Store(true)
	return nil
}

func TestAntiDriftAddsMissingLiveEntriesWithoutLimitCheck(t *testing.T) {
	key := limitedKey(1)
	backend := newRecordingBackend()
	driver := NewAntiDriftDriver(
		AntiDriftConfig{Grace: time.Minute},
		fakePrimary{primary: true},
		fakeLimitedKeyStore{keys: []*models.CreatedTeamAPIKey{key}},
		fakeLiveCache{
			removeSafe: true,
			live: []cachepkg.LiveLockstring{{
				LockString:        "lock-1",
				CreationTimestamp: time.Now().Add(-2 * time.Minute),
			}},
		},
		backend,
	)

	require.NoError(t, driver.RunOnce(context.Background()))
	assert.Contains(t, backend.added[key.ID.String()], "lock-1")
}

func TestAntiDriftRemovesStaleRedisEntriesWhenCacheHealthy(t *testing.T) {
	key := limitedKey(1)
	backend := newRecordingBackend()
	backend.have[key.ID.String()] = map[string]time.Time{
		"leaked": time.Now().Add(-2 * time.Minute),
	}
	driver := NewAntiDriftDriver(
		AntiDriftConfig{Grace: time.Minute},
		fakePrimary{primary: true},
		fakeLimitedKeyStore{keys: []*models.CreatedTeamAPIKey{key}},
		fakeLiveCache{removeSafe: true},
		backend,
	)

	require.NoError(t, driver.RunOnce(context.Background()))
	assert.Contains(t, backend.removed[key.ID.String()], "leaked")
}

func TestAntiDriftAddsEvenWhenRemoveUnsafe(t *testing.T) {
	key := limitedKey(1)
	backend := newRecordingBackend()
	backend.have[key.ID.String()] = map[string]time.Time{
		"leaked": time.Now().Add(-2 * time.Minute),
	}
	driver := NewAntiDriftDriver(
		AntiDriftConfig{Grace: time.Minute},
		fakePrimary{primary: true},
		fakeLimitedKeyStore{keys: []*models.CreatedTeamAPIKey{key}},
		fakeLiveCache{
			removeSafe: false,
			live: []cachepkg.LiveLockstring{{
				LockString:        "lock-1",
				CreationTimestamp: time.Now().Add(-2 * time.Minute),
			}},
		},
		backend,
	)

	require.NoError(t, driver.RunOnce(context.Background()))
	assert.Contains(t, backend.added[key.ID.String()], "lock-1")
	assert.Empty(t, backend.removed[key.ID.String()])
}

func TestAntiDriftEnumerationErrorReturnsErrorAndNoMutations(t *testing.T) {
	backend := newRecordingBackend()
	driver := NewAntiDriftDriver(
		AntiDriftConfig{Grace: time.Minute},
		fakePrimary{primary: true},
		fakeLimitedKeyStore{err: errors.New("key store down")},
		fakeLiveCache{removeSafe: true},
		backend,
	)

	err := driver.RunOnce(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key store down")
	assert.Empty(t, backend.added)
	assert.Empty(t, backend.removed)
}

func TestAntiDriftSkipsKeyWhenLiveListErrors(t *testing.T) {
	key := limitedKey(1)
	backend := newRecordingBackend()
	backend.have[key.ID.String()] = map[string]time.Time{
		"leaked": time.Now().Add(-2 * time.Minute),
	}
	driver := NewAntiDriftDriver(
		AntiDriftConfig{Grace: time.Minute},
		fakePrimary{primary: true},
		fakeLimitedKeyStore{keys: []*models.CreatedTeamAPIKey{key}},
		fakeLiveCache{removeSafe: true, liveErr: errors.New("informer cold")},
		backend,
	)

	require.NoError(t, driver.RunOnce(context.Background()))
	assert.Empty(t, backend.added[key.ID.String()])
	assert.Empty(t, backend.removed[key.ID.String()])
}

func TestAntiDriftSkipsKeyWhenRedisListErrors(t *testing.T) {
	key := limitedKey(1)
	backend := newRecordingBackend()
	backend.have[key.ID.String()] = map[string]time.Time{
		"leaked": time.Now().Add(-2 * time.Minute),
	}
	backend.listErr = ErrBackendUnavailable
	driver := NewAntiDriftDriver(
		AntiDriftConfig{Grace: time.Minute},
		fakePrimary{primary: true},
		fakeLimitedKeyStore{keys: []*models.CreatedTeamAPIKey{key}},
		fakeLiveCache{
			removeSafe: true,
			live: []cachepkg.LiveLockstring{{
				LockString:        "lock-1",
				CreationTimestamp: time.Now().Add(-2 * time.Minute),
			}},
		},
		backend,
	)

	require.NoError(t, driver.RunOnce(context.Background()))
	assert.Empty(t, backend.added[key.ID.String()])
	assert.Empty(t, backend.removed[key.ID.String()])
}

func TestAntiDriftSkipsRemoveWhenAddObservedFails(t *testing.T) {
	key := limitedKey(1)
	backend := newRecordingBackend()
	backend.addErr = errors.New("redis write down")
	backend.have[key.ID.String()] = map[string]time.Time{
		"leaked": time.Now().Add(-2 * time.Minute),
	}
	driver := NewAntiDriftDriver(
		AntiDriftConfig{Grace: time.Minute},
		fakePrimary{primary: true},
		fakeLimitedKeyStore{keys: []*models.CreatedTeamAPIKey{key}},
		fakeLiveCache{
			removeSafe: true,
			live: []cachepkg.LiveLockstring{{
				LockString:        "lock-1",
				CreationTimestamp: time.Now().Add(-2 * time.Minute),
			}},
		},
		backend,
	)

	require.NoError(t, driver.RunOnce(context.Background()))
	assert.Empty(t, backend.removed[key.ID.String()], "remove must skip for the owner after add fails")
	assert.Contains(t, backend.have[key.ID.String()], "leaked")
}

func TestAntiDriftLiveListUsesOwnerFilter(t *testing.T) {
	keyA := limitedKey(1)
	keyB := limitedKey(1)
	backend := newRecordingBackend()
	driver := NewAntiDriftDriver(
		AntiDriftConfig{Grace: time.Minute},
		fakePrimary{primary: true},
		fakeLimitedKeyStore{keys: []*models.CreatedTeamAPIKey{keyA, keyB}},
		fakeLiveCache{
			removeSafe: true,
			liveByOwner: map[string][]cachepkg.LiveLockstring{
				keyA.ID.String(): {{
					LockString:        "lock-a",
					CreationTimestamp: time.Now().Add(-2 * time.Minute),
				}},
			},
		},
		backend,
	)

	require.NoError(t, driver.RunOnce(context.Background()))
	assert.Contains(t, backend.added[keyA.ID.String()], "lock-a")
	assert.Empty(t, backend.added[keyB.ID.String()])
}

func TestAntiDriftEventUpdateToNotLiveReleasesOnlyAfterProbeConfirmsGone(t *testing.T) {
	owner := uuid.New().String()
	tests := []struct {
		name   string
		mutate func(*agentsv1alpha1.Sandbox)
	}{
		{
			name: "deletion timestamp",
			mutate: func(sbx *agentsv1alpha1.Sandbox) {
				sbx.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			},
		},
		{
			name: "terminating phase",
			mutate: func(sbx *agentsv1alpha1.Sandbox) {
				sbx.Status.Phase = agentsv1alpha1.SandboxTerminating
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := newRecordingBackend()
			backend.have[owner] = map[string]time.Time{"lock-1": time.Now()}
			driver := NewAntiDriftDriver(
				AntiDriftConfig{Grace: time.Minute},
				fakePrimary{primary: true},
				fakeLimitedKeyStore{},
				fakeLiveCache{removeSafe: true},
				backend,
			)

			oldObj := sandboxForQuotaEvent(owner, "lock-1")
			newObj := sandboxForQuotaEvent(owner, "lock-1")
			tt.mutate(newObj)
			driver.SandboxEventHandler().OnUpdate(oldObj, newObj)

			assert.Contains(t, backend.removed[owner], "lock-1")
		})
	}
}

func TestAntiDriftDeleteTombstoneReleasesOnlyAfterProbeConfirmsGone(t *testing.T) {
	owner := uuid.New().String()
	backend := newRecordingBackend()
	backend.have[owner] = map[string]time.Time{"lock-1": time.Now()}
	driver := NewAntiDriftDriver(
		AntiDriftConfig{Grace: time.Minute},
		fakePrimary{primary: true},
		fakeLimitedKeyStore{},
		fakeLiveCache{removeSafe: true},
		backend,
	)

	driver.SandboxEventHandler().OnDelete(toolscache.DeletedFinalStateUnknown{Obj: sandboxForQuotaEvent(owner, "lock-1")})
	assert.Contains(t, backend.removed[owner], "lock-1")
}

func TestAntiDriftEventReleaseSkipCases(t *testing.T) {
	owner := uuid.New().String()
	tests := []struct {
		name          string
		primary       bool
		removeSafe    bool
		liveErr       error
		live          []cachepkg.LiveLockstring
		owner         string
		lockString    string
		expectRelease bool
	}{
		{name: "not primary", removeSafe: true, owner: owner, lockString: "lock-1"},
		{name: "remove unsafe", primary: true, owner: owner, lockString: "lock-1"},
		{name: "missing owner", primary: true, removeSafe: true, lockString: "lock-1"},
		{name: "missing lock string", primary: true, removeSafe: true, owner: owner},
		{name: "probe error", primary: true, removeSafe: true, liveErr: errors.New("probe failed"), owner: owner, lockString: "lock-1"},
		{
			name:       "lock still live",
			primary:    true,
			removeSafe: true,
			live: []cachepkg.LiveLockstring{{
				LockString: "lock-1",
			}},
			owner:      owner,
			lockString: "lock-1",
		},
		{name: "probe confirms gone", primary: true, removeSafe: true, owner: owner, lockString: "lock-1", expectRelease: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := newRecordingBackend()
			backend.have[owner] = map[string]time.Time{"lock-1": time.Now()}
			driver := NewAntiDriftDriver(
				AntiDriftConfig{Grace: time.Minute},
				fakePrimary{primary: tt.primary},
				fakeLimitedKeyStore{},
				fakeLiveCache{removeSafe: tt.removeSafe, liveErr: tt.liveErr, live: tt.live},
				backend,
			)

			driver.SandboxEventHandler().OnDelete(sandboxForQuotaEvent(tt.owner, tt.lockString))
			if tt.expectRelease {
				assert.Contains(t, backend.removed[owner], "lock-1")
				return
			}
			assert.Empty(t, backend.removed[owner])
		})
	}
}

func TestAntiDriftEventReleaseMetricsAndBoundedContext(t *testing.T) {
	owner := uuid.New().String()
	probeSawDeadline := &atomic.Bool{}
	backend := newRecordingBackend()
	backend.have[owner] = map[string]time.Time{"lock-1": time.Now()}
	driver := NewAntiDriftDriver(
		AntiDriftConfig{Grace: time.Minute},
		fakePrimary{primary: true},
		fakeLimitedKeyStore{},
		fakeLiveCache{removeSafe: true, sawDeadline: probeSawDeadline},
		backend,
	)

	beforeReleased := testutil.ToFloat64(antiDriftEventReleaseTotal.WithLabelValues("released"))
	beforeProbeAttempt := testutil.ToFloat64(antiDriftEventProbeTotal.WithLabelValues("attempt"))
	beforeProbeGone := testutil.ToFloat64(antiDriftEventProbeTotal.WithLabelValues("gone"))
	driver.SandboxEventHandler().OnDelete(sandboxForQuotaEvent(owner, "lock-1"))

	assert.True(t, probeSawDeadline.Load())
	assert.True(t, backend.releaseSawDeadline.Load())
	assert.Equal(t, beforeReleased+1, testutil.ToFloat64(antiDriftEventReleaseTotal.WithLabelValues("released")))
	assert.Equal(t, beforeProbeAttempt+1, testutil.ToFloat64(antiDriftEventProbeTotal.WithLabelValues("attempt")))
	assert.Equal(t, beforeProbeGone+1, testutil.ToFloat64(antiDriftEventProbeTotal.WithLabelValues("gone")))

	noopOwner := uuid.New().String()
	noopBackend := newRecordingBackend()
	noopDriver := NewAntiDriftDriver(
		AntiDriftConfig{Grace: time.Minute},
		fakePrimary{primary: true},
		fakeLimitedKeyStore{},
		fakeLiveCache{removeSafe: true},
		noopBackend,
	)
	beforeNoop := testutil.ToFloat64(antiDriftEventReleaseTotal.WithLabelValues("noop"))
	noopDriver.SandboxEventHandler().OnDelete(sandboxForQuotaEvent(noopOwner, "lock-1"))
	assert.Equal(t, beforeNoop+1, testutil.ToFloat64(antiDriftEventReleaseTotal.WithLabelValues("noop")))
}

func TestAntiDriftRunStartsNonBlockingAndStopRemovesRegistration(t *testing.T) {
	registration := &recordingRegistration{}
	driver := NewAntiDriftDriver(
		AntiDriftConfig{Interval: time.Hour, Grace: time.Minute},
		fakePrimary{primary: true},
		fakeLimitedKeyStore{},
		fakeLiveCache{removeSafe: true},
		newRecordingBackend(),
	)
	driver.SetEventRegistration(registration)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	returned := make(chan struct{})
	go func() {
		driver.Run(ctx)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Run must return without waiting for the loop")
	}

	cancel()
	driver.Stop()
	assert.True(t, registration.removed.Load())
}

func TestAntiDriftSetEventRegistrationAfterStopRemovesRegistration(t *testing.T) {
	driver := NewAntiDriftDriver(
		AntiDriftConfig{Interval: time.Hour, Grace: time.Minute},
		fakePrimary{primary: true},
		fakeLimitedKeyStore{},
		fakeLiveCache{removeSafe: true},
		newRecordingBackend(),
	)
	registration := &recordingRegistration{}

	driver.Stop()
	driver.SetEventRegistration(registration)
	driver.Stop()
	assert.True(t, registration.removed.Load())
}

func TestAntiDriftStopCancelsAndWaitsForActiveCycle(t *testing.T) {
	keys := newBlockingLimitedKeyStore()
	driver := NewAntiDriftDriver(
		AntiDriftConfig{Interval: time.Millisecond, Grace: time.Minute, CycleTimeout: time.Hour},
		fakePrimary{primary: true},
		keys,
		fakeLiveCache{removeSafe: true},
		newRecordingBackend(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	driver.Run(ctx)

	select {
	case <-keys.started:
	case <-time.After(time.Second):
		t.Fatal("anti-drift cycle did not start")
	}

	stopped := make(chan struct{})
	go func() {
		driver.Stop()
		close(stopped)
	}()

	select {
	case <-keys.done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel active anti-drift cycle")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not wait for the anti-drift run loop to exit")
	}
}

func TestAntiDriftStopWaitsForInFlightEventRelease(t *testing.T) {
	owner := uuid.New().String()
	backend := newBlockingReleaseBackend()
	backend.have[owner] = map[string]time.Time{"lock-1": time.Now()}
	driver := NewAntiDriftDriver(
		AntiDriftConfig{Interval: time.Hour, Grace: time.Minute},
		fakePrimary{primary: true},
		fakeLimitedKeyStore{},
		fakeLiveCache{removeSafe: true},
		backend,
	)

	eventDone := make(chan struct{})
	go func() {
		driver.SandboxEventHandler().OnDelete(sandboxForQuotaEvent(owner, "lock-1"))
		close(eventDone)
	}()

	select {
	case <-backend.started:
	case <-time.After(time.Second):
		t.Fatal("event release did not start")
	}

	stopped := make(chan struct{})
	go func() {
		driver.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("Stop returned before the in-flight event release completed")
	case <-time.After(100 * time.Millisecond):
	}

	close(backend.unblock)

	select {
	case <-eventDone:
	case <-time.After(time.Second):
		t.Fatal("event release did not finish after unblock")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after event release finished")
	}
}

func TestAntiDriftCycleTimeoutUsesIntervalCap(t *testing.T) {
	tests := []struct {
		name   string
		cfg    AntiDriftConfig
		expect time.Duration
	}{
		{
			name:   "interval caps timeout",
			cfg:    AntiDriftConfig{Interval: 10 * time.Millisecond, Grace: time.Minute, CycleTimeout: time.Second},
			expect: 10 * time.Millisecond,
		},
		{
			name:   "cycle timeout remains when shorter",
			cfg:    AntiDriftConfig{Interval: time.Minute, Grace: time.Minute, CycleTimeout: time.Second},
			expect: time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			driver := NewAntiDriftDriver(
				tt.cfg,
				fakePrimary{primary: true},
				fakeLimitedKeyStore{},
				fakeLiveCache{removeSafe: true},
				newRecordingBackend(),
			)
			assert.Equal(t, tt.expect, driver.cycleTimeout())
		})
	}
}

func TestAntiDriftRunOnceHonorsCancellation(t *testing.T) {
	key := limitedKey(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	driver := NewAntiDriftDriver(
		AntiDriftConfig{Grace: time.Minute},
		fakePrimary{primary: true},
		fakeLimitedKeyStore{keys: []*models.CreatedTeamAPIKey{key}},
		fakeLiveCache{
			removeSafe: true,
			live: []cachepkg.LiveLockstring{{
				LockString:        "lock-1",
				CreationTimestamp: time.Now().Add(-2 * time.Minute),
			}},
		},
		newRecordingBackend(),
	)

	require.ErrorIs(t, driver.RunOnce(ctx), context.Canceled)
}

func limitedKey(limitValue int64) *models.CreatedTeamAPIKey {
	return &models.CreatedTeamAPIKey{
		ID: uuid.New(),
		QuotaSpec: &models.QuotaSpec{
			Limits: []models.QuotaLimit{{
				Dimension: models.DimSandboxCount,
				Limit:     &limitValue,
			}},
		},
	}
}

func sandboxForQuotaEvent(owner, lockString string) *agentsv1alpha1.Sandbox {
	annotations := map[string]string{}
	if owner != "" {
		annotations[agentsv1alpha1.AnnotationOwner] = owner
	}
	if lockString != "" {
		annotations[agentsv1alpha1.AnnotationLock] = lockString
	}

	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "sbx-1",
			Namespace:   "default",
			Annotations: annotations,
		},
	}
}
