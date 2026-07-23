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

package sandboxroute

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
)

func TestObservationErrors(t *testing.T) {
	cause := errors.New("boom")
	tests := []struct {
		name        string
		build       func(error) error
		expectError string
	}{
		{name: "get", build: NewGetObservationError, expectError: "observation get failed"},
		{name: "projection", build: NewProjectionObservationError, expectError: "observation projection failed"},
		{name: "nil get", build: NewGetObservationError},
		{name: "nil projection", build: NewProjectionObservationError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := cause
			if tt.expectError == "" {
				input = nil
			}
			err := tt.build(input)
			if tt.expectError == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
			assert.ErrorIs(t, err, cause)
		})
	}
}

func TestNewRepairer(t *testing.T) {
	store := newTestStore(t, nil, time.Second)
	observer := func(context.Context, types.NamespacedName) (AuthoritativeObservation, error) {
		return AuthoritativeObservation{}, nil
	}
	tests := []struct {
		name        string
		store       *Store
		observer    ObserveFunc
		options     RepairerOptions
		expectError string
	}{
		{name: "defaults", store: store, observer: observer},
		{name: "custom limits", store: store, observer: observer, options: RepairerOptions{Workers: 2, QPS: 3, Burst: 2, BaseDelay: time.Millisecond, MaxDelay: time.Second, MaintenanceInterval: time.Second}},
		{name: "nil Store", observer: observer, expectError: "Store must not be nil"},
		{name: "nil observer", store: store, expectError: "observer must not be nil"},
		{name: "negative workers", store: store, observer: observer, options: RepairerOptions{Workers: -1}, expectError: "must not be negative"},
		{name: "negative QPS", store: store, observer: observer, options: RepairerOptions{QPS: -1}, expectError: "must not be negative"},
		{name: "max below base", store: store, observer: observer, options: RepairerOptions{BaseDelay: time.Second, MaxDelay: time.Millisecond}, expectError: "max delay"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repairer, err := NewRepairer(tt.store, tt.observer, tt.options)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, repairer)
			repairer.queue.ShutDown()
		})
	}
}

func TestRepairerEnqueueDeduplicatesNewestGeneration(t *testing.T) {
	key := types.NamespacedName{Namespace: "ns", Name: "one"}
	tests := []struct {
		name             string
		requests         []RepairRequest
		expectPending    int
		expectGeneration uint64
	}{
		{name: "invalid requests ignored", requests: []RepairRequest{{}, {ObjectKey: key}}, expectPending: 0},
		{name: "one request", requests: []RepairRequest{{ObjectKey: key, Generation: 1}}, expectPending: 1, expectGeneration: 1},
		{name: "duplicate older ignored", requests: []RepairRequest{{ObjectKey: key, Generation: 2}, {ObjectKey: key, Generation: 1}}, expectPending: 1, expectGeneration: 2},
		{name: "newest replaces queued generation", requests: []RepairRequest{{ObjectKey: key, Generation: 1}, {ObjectKey: key, Generation: 3}, {ObjectKey: key, Generation: 2}}, expectPending: 1, expectGeneration: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repairer := newTestRepairer(t, newTestStore(t, nil, time.Second), func(context.Context, types.NamespacedName) (AuthoritativeObservation, error) {
				return AuthoritativeObservation{}, nil
			})
			defer repairer.queue.ShutDown()
			repairer.Enqueue(MutationResult{RepairRequests: tt.requests[:len(tt.requests)/2]})
			repairer.EnqueueRequests(tt.requests[len(tt.requests)/2:])
			assert.Equal(t, tt.expectPending, repairer.Pending())
			if tt.expectPending > 0 {
				generation, exists := repairer.pendingGeneration(key)
				assert.True(t, exists)
				assert.Equal(t, tt.expectGeneration, generation)
			}
		})
	}
	assert.Equal(t, 0, (*Repairer)(nil).Pending())
}

func TestRepairerProcessOutcomes(t *testing.T) {
	tests := []struct {
		name             string
		observer         func(Route) ObserveFunc
		mutateBeforeRead func(*Store, RepairRequest)
		expectPending    int
		expectRequeues   int
		expectIDs        []string
	}{
		{
			name: "authoritative presence",
			observer: func(authoritative Route) ObserveFunc {
				return func(context.Context, types.NamespacedName) (AuthoritativeObservation, error) {
					return AuthoritativeObservation{Present: true, Route: authoritative}, nil
				}
			},
			expectIDs: []string{"new"},
		},
		{
			name: "authoritative absence",
			observer: func(Route) ObserveFunc {
				return func(context.Context, types.NamespacedName) (AuthoritativeObservation, error) {
					return AuthoritativeObservation{}, nil
				}
			},
		},
		{
			name: "get error retries",
			observer: func(Route) ObserveFunc {
				return func(context.Context, types.NamespacedName) (AuthoritativeObservation, error) {
					return AuthoritativeObservation{}, NewGetObservationError(errors.New("temporary"))
				}
			},
			expectPending: 1, expectRequeues: 1,
		},
		{
			name: "projection error retries",
			observer: func(Route) ObserveFunc {
				return func(context.Context, types.NamespacedName) (AuthoritativeObservation, error) {
					return AuthoritativeObservation{}, NewProjectionObservationError(errors.New("project"))
				}
			},
			expectPending: 1, expectRequeues: 1,
		},
		{
			name: "malformed present observation retries",
			observer: func(Route) ObserveFunc {
				return func(context.Context, types.NamespacedName) (AuthoritativeObservation, error) {
					return AuthoritativeObservation{Present: true, Route: Route{}}, nil
				}
			},
			expectPending: 1, expectRequeues: 1,
		},
		{
			name: "same key event makes result stale",
			mutateBeforeRead: func(store *Store, _ RepairRequest) {
				store.Upsert(fullRoute("old", "ns", "one", "uid-a", "2"))
			},
			observer: func(authoritative Route) ObserveFunc {
				return func(context.Context, types.NamespacedName) (AuthoritativeObservation, error) {
					return AuthoritativeObservation{Present: true, Route: authoritative}, nil
				}
			},
			expectIDs: []string{"old"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t, nil, time.Second)
			request := requireDeletionFenceRepairRequest(
				t,
				store,
				fullRoute("old", "ns", "one", "uid-a", "1"),
			)
			if tt.mutateBeforeRead != nil {
				tt.mutateBeforeRead(store, request)
			}
			repairer := newTestRepairer(t, store, tt.observer(fullRoute("new", "ns", "one", "uid-b", "2")))
			defer repairer.queue.ShutDown()
			repairer.EnqueueRequest(request)
			assert.True(t, repairer.processNext(context.Background()))
			assert.Equal(t, tt.expectPending, repairer.Pending())
			assert.Equal(t, tt.expectRequeues, repairer.queue.NumRequeues(request.ObjectKey))
			assert.Equal(t, tt.expectIDs, routeIDs(store.List()))
		})
	}
}

func TestRepairerRetainsNewestGenerationDuringRead(t *testing.T) {
	store := newTestStore(t, nil, time.Second)
	route := fullRoute("old", "ns", "one", "uid-a", "1")
	first := requireDeletionFenceRepairRequest(t, store, route)

	calls := 0
	var repairer *Repairer
	repairer = newTestRepairer(t, store, func(context.Context, types.NamespacedName) (AuthoritativeObservation, error) {
		calls++
		if calls == 1 {
			newer := store.Upsert(route)
			require.Len(t, newer.RepairRequests, 1)
			repairer.Enqueue(newer)
		}
		return AuthoritativeObservation{Present: true, Route: route}, nil
	})
	defer repairer.queue.ShutDown()
	repairer.EnqueueRequest(first)
	require.True(t, repairer.processNext(context.Background()))
	require.True(t, repairer.processNext(context.Background()))
	assert.Equal(t, 2, calls)
	assert.Equal(t, 0, repairer.Pending())
	assert.Equal(t, []string{"old"}, routeIDs(store.List()))
}

func TestRepairPendingTracksDeduplicatedRequests(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{
			name: "queued generations are deduplicated by ObjectKey",
			run: func(t *testing.T) {
				repairer := newTestRepairer(t, newTestStore(t, nil, time.Second), func(context.Context, types.NamespacedName) (AuthoritativeObservation, error) {
					return AuthoritativeObservation{}, nil
				})
				defer repairer.queue.ShutDown()
				first := types.NamespacedName{Namespace: "ns", Name: "one"}
				second := types.NamespacedName{Namespace: "ns", Name: "two"}
				repairer.EnqueueRequests([]RepairRequest{
					{ObjectKey: first, Generation: 1},
					{ObjectKey: first, Generation: 2},
					{ObjectKey: second, Generation: 1},
				})
				assert.Equal(t, 2, repairer.Pending())
			},
		},
		{
			name: "concurrent enqueue cannot overwrite depth with an older snapshot",
			run: func(t *testing.T) {
				store := NewStore(StoreOptions{})
				queue := &blockingFirstAddQueue{
					TypedRateLimitingInterface: workqueue.NewTypedRateLimitingQueue(newImmediateRateLimiter()),
					entered:                    make(chan struct{}),
					release:                    make(chan struct{}),
				}
				repairer, err := NewRepairer(store, func(context.Context, types.NamespacedName) (AuthoritativeObservation, error) {
					return AuthoritativeObservation{}, nil
				}, RepairerOptions{Queue: queue})
				require.NoError(t, err)
				defer repairer.queue.ShutDown()

				firstDone := make(chan struct{})
				go func() {
					defer close(firstDone)
					repairer.EnqueueRequest(RepairRequest{
						ObjectKey:  types.NamespacedName{Namespace: "ns", Name: "one"},
						Generation: 1,
					})
				}()
				<-queue.entered
				repairer.EnqueueRequest(RepairRequest{
					ObjectKey:  types.NamespacedName{Namespace: "ns", Name: "two"},
					Generation: 1,
				})
				close(queue.release)
				<-firstDone

				assert.Equal(t, 2, repairer.Pending())
			},
		},
		{
			name: "concurrent completion and replacement generations keep deterministic depth",
			run: func(t *testing.T) {
				repairer := newTestRepairer(t, newTestStore(t, nil, time.Second), func(context.Context, types.NamespacedName) (AuthoritativeObservation, error) {
					return AuthoritativeObservation{}, nil
				})
				defer repairer.queue.ShutDown()
				const count = 64
				keys := make([]types.NamespacedName, 0, count)
				for index := 0; index < count; index++ {
					key := types.NamespacedName{Namespace: "ns", Name: strconv.Itoa(index)}
					keys = append(keys, key)
					repairer.EnqueueRequest(RepairRequest{ObjectKey: key, Generation: 1})
				}

				start := make(chan struct{})
				var workers sync.WaitGroup
				for _, key := range keys {
					key := key
					workers.Add(2)
					go func() {
						defer workers.Done()
						<-start
						repairer.complete(key, 1)
					}()
					go func() {
						defer workers.Done()
						<-start
						repairer.EnqueueRequest(RepairRequest{ObjectKey: key, Generation: 2})
					}()
				}
				close(start)
				workers.Wait()
				assert.Equal(t, count, repairer.Pending())

				for _, key := range keys {
					key := key
					workers.Add(1)
					go func() {
						defer workers.Done()
						repairer.complete(key, 2)
					}()
				}
				workers.Wait()
				assert.Equal(t, 0, repairer.Pending())
			},
		},
		{
			name: "processing item remains in depth until completion",
			run: func(t *testing.T) {
				entered := make(chan struct{})
				release := make(chan struct{})
				repairer := newTestRepairer(t, newTestStore(t, nil, time.Second), func(context.Context, types.NamespacedName) (AuthoritativeObservation, error) {
					close(entered)
					<-release
					return AuthoritativeObservation{}, nil
				})
				defer repairer.queue.ShutDown()
				repairer.EnqueueRequest(RepairRequest{ObjectKey: types.NamespacedName{Namespace: "ns", Name: "one"}, Generation: 1})
				done := make(chan bool, 1)
				go func() { done <- repairer.processNext(context.Background()) }()
				<-entered
				assert.Equal(t, 0, repairer.queue.Len())
				assert.Equal(t, 1, repairer.Pending())
				close(release)
				assert.True(t, <-done)
				assert.Equal(t, 0, repairer.Pending())
			},
		},
		{
			name: "rate-limited retry remains in depth while delayed",
			run: func(t *testing.T) {
				store := newTestStore(t, nil, time.Second)
				repairer, err := NewRepairer(store, func(context.Context, types.NamespacedName) (AuthoritativeObservation, error) {
					return AuthoritativeObservation{}, errors.New("temporary")
				}, RepairerOptions{
					QPS:         1_000_000,
					Burst:       1,
					RateLimiter: newDelayedRateLimiter(time.Hour),
				})
				require.NoError(t, err)
				defer repairer.queue.ShutDown()
				key := types.NamespacedName{Namespace: "ns", Name: "one"}
				repairer.EnqueueRequest(RepairRequest{ObjectKey: key, Generation: 1})
				require.True(t, repairer.processNext(context.Background()))
				assert.Equal(t, 0, repairer.queue.Len())
				assert.Equal(t, 1, repairer.Pending())
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t)
		})
	}
}

func TestRepairerStartRunsMaintenanceAndStops(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "deletion fence confirmation"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t, nil, 10*time.Millisecond)
			route := fullRoute("id", "ns", "one", "uid-a", "1")
			store.Upsert(route)
			store.Delete(route)
			repairer, err := NewRepairer(store, func(context.Context, types.NamespacedName) (AuthoritativeObservation, error) {
				return AuthoritativeObservation{}, nil
			}, RepairerOptions{
				MaintenanceInterval: 2 * time.Millisecond,
				RateLimiter:         newImmediateRateLimiter(),
			})
			require.NoError(t, err)

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- repairer.Start(ctx) }()
			require.Eventually(t, func() bool { return storeIsEmpty(store) }, time.Second, time.Millisecond)
			cancel()
			require.NoError(t, <-done)
			assert.False(t, repairer.processNext(context.Background()))
		})
	}
}

func TestValidateObservation(t *testing.T) {
	key := types.NamespacedName{Namespace: "ns", Name: "one"}
	tests := []struct {
		name        string
		observation AuthoritativeObservation
		expectError string
	}{
		{name: "absence"},
		{name: "full", observation: AuthoritativeObservation{Present: true, Route: fullRoute("id", "ns", "one", "uid", "1")}},
		{name: "invalid", observation: AuthoritativeObservation{Present: true}, expectError: "invalid legacy sandbox ID"},
		{name: "opaque ID-only", observation: AuthoritativeObservation{Present: true, Route: idOnlyRoute("id", "uid", "1")}, expectError: "invalid legacy sandbox ID"},
		{name: "legacy ID-only", observation: AuthoritativeObservation{Present: true, Route: idOnlyRoute("ns--one", "uid", "1")}},
		{name: "wrong key", observation: AuthoritativeObservation{Present: true, Route: fullRoute("id", "ns", "two", "uid", "1")}, expectError: "does not match"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateObservation(key, tt.observation)
			if tt.expectError == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
		})
	}
}

func requireDeletionFenceRepairRequest(t *testing.T, store *Store, route Route) RepairRequest {
	t.Helper()
	require.Equal(t, EventResultApplied, store.Upsert(route).Result)
	require.Equal(t, EventResultApplied, store.Delete(route).Result)
	result := store.Upsert(route)
	require.Equal(t, EventResultRepairRequired, result.Result)
	require.Len(t, result.RepairRequests, 1)
	return result.RepairRequests[0]
}

func newTestRepairer(t *testing.T, store *Store, observe ObserveFunc) *Repairer {
	t.Helper()
	repairer, err := NewRepairer(store, observe, RepairerOptions{
		QPS:         1_000_000,
		Burst:       1,
		RateLimiter: newImmediateRateLimiter(),
	})
	require.NoError(t, err)
	return repairer
}

type immediateRateLimiter struct {
	mu       sync.Mutex
	requeues map[types.NamespacedName]int
	delay    time.Duration
}

type blockingFirstAddQueue struct {
	workqueue.TypedRateLimitingInterface[types.NamespacedName]
	addCount atomic.Int32
	entered  chan struct{}
	release  chan struct{}
}

func (q *blockingFirstAddQueue) Add(item types.NamespacedName) {
	if q.addCount.Add(1) == 1 {
		close(q.entered)
		<-q.release
	}
	q.TypedRateLimitingInterface.Add(item)
}

func newImmediateRateLimiter() workqueue.TypedRateLimiter[types.NamespacedName] {
	return &immediateRateLimiter{requeues: make(map[types.NamespacedName]int)}
}

func newDelayedRateLimiter(delay time.Duration) workqueue.TypedRateLimiter[types.NamespacedName] {
	return &immediateRateLimiter{requeues: make(map[types.NamespacedName]int), delay: delay}
}

func (r *immediateRateLimiter) When(item types.NamespacedName) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requeues[item]++
	return r.delay
}

func (r *immediateRateLimiter) NumRequeues(item types.NamespacedName) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requeues[item]
}

func (r *immediateRateLimiter) Forget(item types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.requeues, item)
}
