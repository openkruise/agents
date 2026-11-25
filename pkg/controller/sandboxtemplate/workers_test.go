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

package sandboxtemplate

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/openkruise/agents/api/v1alpha1"
)

func createTestBuildLease(t *testing.T, r *Reconciler, version *api.SandboxTemplate, holder string, renewed time.Time) *coordinationv1.Lease {
	t.Helper()
	key := buildLeaseKey(version)
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, UID: "lease-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplate", Name: version.Name, UID: version.UID, Controller: ptr.To(true)}}},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: ptr.To(holder), LeaseDurationSeconds: ptr.To(int32(180)), RenewTime: ptr.To(metav1.NewMicroTime(renewed))},
	}
	require.NoError(t, r.Create(t.Context(), lease))
	return lease
}

func leaseFixture(t *testing.T) (*Reconciler, *api.SandboxTemplate, *coordinationv1.Lease, *clocktesting.FakeClock) {
	t.Helper()
	version := modelFixtures()
	r := modelReconciler(t, version)
	clock := clocktesting.NewFakeClock(time.Now().UTC().Truncate(time.Microsecond))
	r.leaseClock = clock
	lease := createTestBuildLease(t, r, version, r.instanceID, clock.Now())
	r.workers = map[types.UID]*buildWorker{version.UID: {key: client.ObjectKeyFromObject(version), lease: lease.DeepCopy()}}
	return r, version, lease, clock
}

func runTestHeartbeat(t *testing.T, parent context.Context, r *Reconciler, version *api.SandboxTemplate, lease *coordinationv1.Lease, clock *clocktesting.FakeClock) (context.Context, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	r.workers[version.UID].cancel = cancel
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.keepBuildLease(ctx, version, lease, cancel)
	}()
	require.Eventually(t, func() bool { return clock.Waiters() == 2 }, 3*time.Second, time.Millisecond)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("heartbeat did not stop")
		}
	})
	return ctx, done
}

func TestBuildLeaseAcquisition(t *testing.T) {
	for _, tt := range []struct {
		name      string
		mode      string
		running   bool
		acquired  bool
		wantError bool
	}{
		{name: "new lease", acquired: true},
		{name: "active foreign holder", mode: "foreign"},
		{name: "same holder before execution", mode: "own", acquired: true},
		{name: "same holder cannot replay", mode: "own", running: true},
		{name: "expired holder", mode: "expired", acquired: true},
		{name: "wrong owner", mode: "wrong owner", wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			version := modelFixtures()
			r := modelReconciler(t, version)
			clock := clocktesting.NewFakeClock(time.Now().UTC().Truncate(time.Microsecond))
			r.leaseClock = clock
			if tt.mode != "" {
				holder, renewed := "other-process", clock.Now()
				if tt.mode == "own" {
					holder = r.instanceID
				}
				if tt.mode == "expired" {
					renewed = renewed.Add(-4 * time.Minute)
				}
				lease := createTestBuildLease(t, r, version, holder, renewed)
				if tt.mode == "wrong owner" {
					lease.OwnerReferences[0].UID = "replacement"
					require.NoError(t, r.Update(t.Context(), lease))
				}
			}
			lease, acquired, err := r.acquireBuildLease(t.Context(), version, tt.running)
			if tt.wantError {
				require.ErrorContains(t, err, "ownership changed")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.acquired, acquired)
			if acquired {
				require.Equal(t, r.instanceID, *lease.Spec.HolderIdentity)
				require.EqualValues(t, 180, *lease.Spec.LeaseDurationSeconds)
				require.True(t, ownedByVersion(lease, version))
			}
		})
	}
}

func TestBuildLeaseConcurrentAcquisition(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "create race", true: "expired lease race"}[expired], func(t *testing.T) {
			version := modelFixtures()
			first := modelReconciler(t, version)
			second := modelReconciler(t)
			clock := clocktesting.NewFakeClock(time.Now().UTC().Truncate(time.Microsecond))
			first.leaseClock, second.leaseClock = clock, clock
			if expired {
				createTestBuildLease(t, first, version, "old-process", clock.Now().Add(-4*time.Minute))
			}
			var reads atomic.Int32
			bothRead := make(chan struct{})
			shared := interceptor.NewClient(first.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				err := c.Get(ctx, key, obj, opts...)
				if _, ok := obj.(*coordinationv1.Lease); ok {
					if attempt := reads.Add(1); attempt <= 2 {
						if attempt == 2 {
							close(bothRead)
						}
						select {
						case <-bothRead:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
				}
				return err
			}})
			first.Client, second.Client = shared, shared
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			results := make(chan struct {
				acquired bool
				err      error
			}, 2)
			for _, r := range []*Reconciler{first, second} {
				go func(r *Reconciler) {
					_, acquired, err := r.acquireBuildLease(ctx, version, expired)
					results <- struct {
						acquired bool
						err      error
					}{acquired: acquired, err: err}
				}(r)
			}
			winners := 0
			for range 2 {
				select {
				case result := <-results:
					require.NoError(t, result.err)
					if result.acquired {
						winners++
					}
				case <-ctx.Done():
					t.Fatal("lease contenders did not finish")
				}
			}
			require.Equal(t, 1, winners)
		})
	}
}

func TestBuildLeaseLostCreateResponseDoesNotAuthorize(t *testing.T) {
	version := modelFixtures()
	r := modelReconciler(t, version)
	base := r.Client.(client.WithWatch)
	r.Client = interceptor.NewClient(base, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if err := c.Create(ctx, obj, opts...); err != nil {
			return err
		}
		return apierrors.NewTimeoutError("lease create response was lost", 0)
	}})
	_, acquired, err := r.acquireBuildLease(t.Context(), version, false)
	require.True(t, apierrors.IsTimeout(err), "%v", err)
	require.False(t, acquired)
	require.False(t, r.hasWorker(version.UID))
	lease := &coordinationv1.Lease{}
	require.NoError(t, base.Get(t.Context(), buildLeaseKey(version), lease))
	require.Equal(t, r.instanceID, *lease.Spec.HolderIdentity)
}

func TestBuildLeaseReplacementDoesNotAuthorize(t *testing.T) {
	for _, tt := range []struct {
		name        string
		missingTime bool
		sameHolder  bool
	}{
		{name: "earlier renewal by another holder"},
		{name: "missing renewal by another holder", missingTime: true},
		{name: "earlier renewal by the same holder", sameHolder: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, version, lease, clock := leaseFixture(t)
			require.NoError(t, r.Delete(t.Context(), lease))
			replacement := lease.DeepCopy()
			replacement.UID, replacement.ResourceVersion = "replacement-lease", ""
			if !tt.sameHolder {
				replacement.Spec.HolderIdentity = ptr.To("other-process")
			}
			replacement.Spec.RenewTime = ptr.To(metav1.NewMicroTime(clock.Now().Add(-time.Minute)))
			if tt.missingTime {
				replacement.Spec.RenewTime = nil
			}
			require.NoError(t, r.Create(t.Context(), replacement))
			var writes atomic.Int32
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*coordinationv1.Lease); ok {
					writes.Add(1)
				}
				return c.Update(ctx, obj, opts...)
			}})
			current, err := r.currentBuildLease(t.Context(), version, lease)
			require.ErrorIs(t, err, errExecutionInterrupted)
			require.Nil(t, current)
			_, err = r.renewBuildLease(t.Context(), version, lease)
			require.ErrorIs(t, err, errExecutionInterrupted)
			require.Zero(t, writes.Load())
			stored := &coordinationv1.Lease{}
			require.NoError(t, r.Get(t.Context(), buildLeaseKey(version), stored))
			require.Equal(t, replacement.UID, stored.UID)
			require.Equal(t, replacement.Spec.HolderIdentity, stored.Spec.HolderIdentity)
		})
	}
}

func TestBuildLeaseConfirmedRenewalSurvivesStaleCache(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "older cache value", true: "not yet cached"}[missing], func(t *testing.T) {
			r, version, lease, clock := leaseFixture(t)
			clock.Step(time.Minute)
			_, err := r.renewBuildLease(t.Context(), version, lease)
			require.NoError(t, err)
			base := r.Client.(client.WithWatch)
			r.Client = interceptor.NewClient(base, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if current, ok := obj.(*coordinationv1.Lease); ok {
					if missing {
						return apierrors.NewNotFound(coordinationv1.SchemeGroupVersion.WithResource("leases").GroupResource(), key.Name)
					}
					*current = *lease.DeepCopy()
					return nil
				}
				return c.Get(ctx, key, obj, opts...)
			}})
			clock.Step(2 * time.Minute)
			current, err := r.currentBuildLease(t.Context(), version, lease)
			require.NoError(t, err)
			require.True(t, current.Spec.RenewTime.Time.After(lease.Spec.RenewTime.Time))
			_, err = r.renewBuildLease(t.Context(), version, lease)
			require.NoError(t, err)
			require.NoError(t, base.Get(t.Context(), buildLeaseKey(version), current))
			require.True(t, clock.Now().Equal(current.Spec.RenewTime.Time))
		})
	}
}

func TestBuildLeaseStaleExpiryCannotInterruptRenewedHolder(t *testing.T) {
	first, version, _, _ := executionFixture(t)
	first.Options.Timeout = 30 * time.Minute
	clock := clocktesting.NewFakeClock(time.Now().UTC().Truncate(time.Microsecond))
	first.leaseClock = clock
	version.Status = api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseBuilding, ObservedGeneration: version.Generation, Conditions: []metav1.Condition{
		{Type: buildReadyCondition, Status: metav1.ConditionFalse, Reason: buildReasonRunning, LastTransitionTime: metav1.NewTime(clock.Now())},
	}}
	require.NoError(t, first.Status().Update(t.Context(), version))
	stale := createTestBuildLease(t, first, version, first.instanceID, clock.Now())
	clock.Step(time.Minute)
	_, err := first.renewBuildLease(t.Context(), version, stale)
	require.NoError(t, err)
	second := modelReconciler(t)
	second.Options.Timeout = first.Options.Timeout
	second.leaseClock = clock
	second.Client = interceptor.NewClient(first.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if current, ok := obj.(*coordinationv1.Lease); ok {
			*current = *stale.DeepCopy()
			return nil
		}
		return c.Get(ctx, key, obj, opts...)
	}})
	clock.Step(2 * time.Minute)
	result, err := second.ReconcileVersion(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(version)})
	require.NoError(t, err)
	require.Equal(t, retryInterval, result.RequeueAfter)
	current := &api.SandboxTemplate{}
	require.NoError(t, first.Get(t.Context(), client.ObjectKeyFromObject(version), current))
	require.Equal(t, api.SandboxTemplatePhaseBuilding, current.Status.Phase)
	require.Equal(t, buildReasonRunning, meta.FindStatusCondition(current.Status.Conditions, buildReadyCondition).Reason)
	lease := &coordinationv1.Lease{}
	require.NoError(t, first.Get(t.Context(), buildLeaseKey(version), lease))
	require.Equal(t, first.instanceID, *lease.Spec.HolderIdentity)
}

func TestBuildLeaseHeartbeatFailureWindow(t *testing.T) {
	for _, renewed := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial ownership", true: "renewed ownership"}[renewed], func(t *testing.T) {
			r, version, lease, clock := leaseFixture(t)
			if renewed {
				clock.Step(time.Minute)
				_, err := r.renewBuildLease(t.Context(), version, lease)
				require.NoError(t, err)
			}
			lastSuccess := clock.Now()
			observed := make(chan struct{}, 4)
			logger := funcr.New(func(_, message string) {
				if strings.Contains(message, "renew build lease") {
					observed <- struct{}{}
				}
			}, funcr.Options{})
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*coordinationv1.Lease); ok {
					return errors.New("renew unavailable")
				}
				return c.Update(ctx, obj, opts...)
			}})
			ctx, done := runTestHeartbeat(t, logr.NewContext(t.Context(), logger), r, version, lease, clock)
			for range 2 {
				clock.Step(time.Minute)
				select {
				case <-observed:
				case <-time.After(3 * time.Second):
					t.Fatal("renewal failure was not observed")
				}
				require.NoError(t, ctx.Err())
			}
			clock.Step(time.Minute)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("expired ownership did not cancel")
			}
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			require.Equal(t, 3*time.Minute, clock.Now().Sub(lastSuccess))
		})
	}
}

func TestBuildLeaseExpiryCancelsBlockedRenewal(t *testing.T) {
	r, version, lease, clock := leaseFixture(t)
	started := make(chan struct{})
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if _, ok := obj.(*coordinationv1.Lease); ok {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}
		return c.Update(ctx, obj, opts...)
	}})
	ctx, done := runTestHeartbeat(t, t.Context(), r, version, lease, clock)
	clock.Step(2*time.Minute + 50*time.Second)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("renewal did not start")
	}
	clock.Step(10 * time.Second)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("renewal request blocked ownership cancellation")
	}
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}

func TestBuildLeaseCurrentStateBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name      string
		mutate    func(*testing.T, *Reconciler, *api.SandboxTemplate, *coordinationv1.Lease)
		funcs     interceptor.Funcs
		wantError error
		plain     bool
	}{
		{
			name: "missing Version interrupts",
			mutate: func(t *testing.T, r *Reconciler, version *api.SandboxTemplate, _ *coordinationv1.Lease) {
				removeStoredVersion(t, r, version)
			},
			wantError: errExecutionInterrupted,
		},
		{
			name:      "Version read failure",
			funcs:     failGetFor(&api.SandboxTemplate{}, errors.New("version read unavailable")),
			wantError: errors.New("version read unavailable"),
		},
		{
			name: "changed generation interrupts",
			mutate: func(t *testing.T, r *Reconciler, version *api.SandboxTemplate, _ *coordinationv1.Lease) {
				current := &api.SandboxTemplate{}
				require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
				current.Generation = 2
				require.NoError(t, r.Update(t.Context(), current))
			},
			wantError: errExecutionInterrupted,
		},
		{
			name:      "lease read failure",
			funcs:     failGetFor(&coordinationv1.Lease{}, errors.New("lease read unavailable")),
			wantError: errors.New("lease read unavailable"),
		},
		{
			name: "foreign active holder interrupts",
			mutate: func(t *testing.T, r *Reconciler, version *api.SandboxTemplate, _ *coordinationv1.Lease) {
				current := &coordinationv1.Lease{}
				require.NoError(t, r.Get(t.Context(), buildLeaseKey(version), current))
				current.Spec.HolderIdentity = ptr.To("other-process")
				require.NoError(t, r.Update(t.Context(), current))
			},
			wantError: errExecutionInterrupted,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, version, lease, _ := leaseFixture(t)
			if tt.mutate != nil {
				tt.mutate(t, r, version, lease)
			}
			if tt.funcs.Get != nil {
				r.Client = interceptor.NewClient(r.Client.(client.WithWatch), tt.funcs)
			}
			current, err := r.currentBuildLease(t.Context(), version, lease)
			require.Nil(t, current)
			require.ErrorContains(t, err, tt.wantError.Error())
		})
	}
}

func TestBuildLeaseRenewalBoundaries(t *testing.T) {
	t.Run("update conflict retries", func(t *testing.T) {
		r, version, lease, clock := leaseFixture(t)
		var writes atomic.Int32
		r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*coordinationv1.Lease); ok && writes.Add(1) == 1 {
				return apierrors.NewConflict(coordinationv1.SchemeGroupVersion.WithResource("leases").GroupResource(), obj.GetName(), errors.New("edited"))
			}
			return c.Update(ctx, obj, opts...)
		}})
		clock.Step(time.Minute)
		renewed, err := r.renewBuildLease(t.Context(), version, lease)
		require.NoError(t, err)
		require.EqualValues(t, 2, writes.Load())
		require.True(t, clock.Now().Equal(renewed.Spec.RenewTime.Time))
	})
	t.Run("lease deleted between renewals interrupts", func(t *testing.T) {
		r, version, lease, clock := leaseFixture(t)
		require.NoError(t, r.Delete(t.Context(), lease))
		clock.Step(time.Minute)
		_, err := r.renewBuildLease(t.Context(), version, lease)
		require.ErrorIs(t, err, errExecutionInterrupted)
	})
	t.Run("expired takeover update fails", func(t *testing.T) {
		version := modelFixtures()
		r := modelReconciler(t, version)
		createTestBuildLease(t, r, version, "old-process", time.Now().Add(-4*time.Minute))
		r.Client = interceptor.NewClient(r.Client.(client.WithWatch), failUpdateFor(&coordinationv1.Lease{}, errors.New("acquire rejected")))
		lease, acquired, err := r.acquireBuildLease(t.Context(), version, false)
		require.Nil(t, lease)
		require.False(t, acquired)
		require.ErrorContains(t, err, "acquire expired build lease")
	})
	t.Run("lease ownership change blocks deletion", func(t *testing.T) {
		r, version, _, _ := leaseFixture(t)
		current := &coordinationv1.Lease{}
		require.NoError(t, r.Get(t.Context(), buildLeaseKey(version), current))
		current.OwnerReferences[0].UID = "other-version"
		require.NoError(t, r.Update(t.Context(), current))
		_, err := r.deleteBuildLease(t.Context(), version)
		require.ErrorContains(t, err, "build lease ownership changed")
	})
	t.Run("worker start without the manager", func(t *testing.T) {
		version := modelFixtures()
		r := modelReconciler(t, version)
		lease := createTestBuildLease(t, r, version, r.instanceID, time.Now())
		require.False(t, r.startWorker(version, lease))
		require.False(t, r.hasWorker(version.UID))
	})
	t.Run("worker start deduplicates", func(t *testing.T) {
		version := modelFixtures()
		r := modelReconciler(t, version)
		lease := createTestBuildLease(t, r, version, r.instanceID, time.Now())
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		r.workerCtx = ctx
		r.workers = map[types.UID]*buildWorker{version.UID: {key: client.ObjectKeyFromObject(version), cancel: cancel}}
		require.True(t, r.startWorker(version, lease))
	})
}

func TestBuildLeaseHeartbeatRenewal(t *testing.T) {
	t.Run("successful renewal resets the ownership timer", func(t *testing.T) {
		r, version, lease, clock := leaseFixture(t)
		ctx, _ := runTestHeartbeat(t, t.Context(), r, version, lease, clock)
		clock.Step(time.Minute)
		require.Eventually(t, func() bool {
			current := &coordinationv1.Lease{}
			if err := r.Get(t.Context(), buildLeaseKey(version), current); err != nil {
				return false
			}
			return clock.Now().Equal(current.Spec.RenewTime.Time)
		}, 3*time.Second, time.Millisecond)
		require.NoError(t, ctx.Err())
	})
	t.Run("interrupted renewal cancels the worker", func(t *testing.T) {
		r, version, lease, clock := leaseFixture(t)
		ctx, done := runTestHeartbeat(t, t.Context(), r, version, lease, clock)
		current := &coordinationv1.Lease{}
		require.NoError(t, r.Get(t.Context(), buildLeaseKey(version), current))
		current.Spec.HolderIdentity = ptr.To("other-process")
		require.NoError(t, r.Update(t.Context(), current))
		clock.Step(time.Minute)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("interrupted renewal did not cancel the worker")
		}
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	})
}

func TestRunningBuildLeaseObservations(t *testing.T) {
	for _, tt := range []struct {
		name         string
		lease        string
		local        bool
		budget       bool
		nearDeadline bool
		wantError    bool
		phase        api.SandboxTemplateBuildPhase
		reason       string
		wait         time.Duration
	}{
		{name: "active foreign holder waits", lease: "active", phase: api.SandboxTemplatePhaseBuilding, reason: buildReasonRunning, wait: buildLeaseDuration},
		{name: "foreign holder checks budget before lease expiry", lease: "active", nearDeadline: true, phase: api.SandboxTemplatePhaseBuilding, reason: buildReasonRunning},
		{name: "local worker waits for confirmed lease expiry", lease: "active", local: true, phase: api.SandboxTemplatePhaseBuilding, reason: buildReasonRunning, wait: buildLeaseDuration},
		{name: "local worker checks budget before lease expiry", lease: "active", local: true, nearDeadline: true, phase: api.SandboxTemplatePhaseBuilding, reason: buildReasonRunning},
		{name: "confirmed renewal extends next check despite stale cache", lease: "renewed", local: true, phase: api.SandboxTemplatePhaseBuilding, reason: buildReasonRunning, wait: time.Minute},
		{name: "expired holder fails", lease: "expired", phase: api.SandboxTemplatePhaseFailed, reason: buildReasonInterrupted, wait: retryInterval},
		{name: "missing lease fails", phase: api.SandboxTemplatePhaseFailed, reason: buildReasonInterrupted, wait: retryInterval},
		{name: "read failure does not prove loss", lease: "read error", wantError: true, phase: api.SandboxTemplatePhaseBuilding, reason: buildReasonRunning, wait: retryInterval},
		{name: "live heartbeat cannot extend total budget", lease: "active", budget: true, phase: api.SandboxTemplatePhaseFailed, reason: buildReasonTimeout, wait: retryInterval},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, version, _, _ := executionFixture(t)
			r.Options.Timeout = 30 * time.Minute
			clock := clocktesting.NewFakeClock(time.Now().UTC().Truncate(time.Microsecond))
			r.leaseClock = clock
			started := clock.Now()
			if tt.budget {
				started = started.Add(-31 * time.Minute)
			} else if tt.nearDeadline {
				started = started.Add(-r.Options.Timeout + time.Minute)
			}
			version.Status = api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseBuilding, ObservedGeneration: version.Generation, Conditions: []metav1.Condition{
				{Type: buildReadyCondition, Status: metav1.ConditionFalse, Reason: buildReasonRunning, LastTransitionTime: metav1.NewTime(started)},
			}}
			require.NoError(t, r.Status().Update(t.Context(), version))
			if tt.lease != "" {
				holder, renewed := "old-process", clock.Now()
				if tt.local {
					holder = r.instanceID
				}
				if tt.lease == "expired" {
					renewed = renewed.Add(-4 * time.Minute)
				}
				lease := createTestBuildLease(t, r, version, holder, renewed)
				if tt.local {
					r.workers = map[types.UID]*buildWorker{version.UID: {key: client.ObjectKeyFromObject(version), lease: lease.DeepCopy()}}
				}
				if tt.lease == "renewed" {
					clock.Step(time.Minute)
					_, err := r.renewBuildLease(t.Context(), version, lease)
					require.NoError(t, err)
					r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if current, ok := obj.(*coordinationv1.Lease); ok {
							*current = *lease.DeepCopy()
							return nil
						}
						return c.Get(ctx, key, obj, opts...)
					}})
					clock.Step(2 * time.Minute)
				}
			}
			if tt.lease == "read error" {
				r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*coordinationv1.Lease); ok {
						return errors.New("lease read unavailable")
					}
					return c.Get(ctx, key, obj, opts...)
				}})
			}
			result, err := r.ReconcileVersion(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(version)})
			if tt.wantError {
				require.ErrorContains(t, err, "lease read unavailable")
			} else {
				require.NoError(t, err)
			}
			if tt.nearDeadline {
				require.Greater(t, result.RequeueAfter, 30*time.Second)
				require.LessOrEqual(t, result.RequeueAfter, time.Minute)
			} else {
				require.Equal(t, tt.wait, result.RequeueAfter)
			}
			current := &api.SandboxTemplate{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
			require.Equal(t, tt.phase, current.Status.Phase)
			require.Equal(t, tt.reason, meta.FindStatusCondition(current.Status.Conditions, buildReadyCondition).Reason)
			require.Equal(t, tt.local, r.hasWorker(version.UID))
		})
	}
}
