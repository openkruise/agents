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

// Package routestore provides a concurrent, resourceVersion-aware route table
// shared by the proxy (sandbox-manager) and the registry (sandbox-gateway).
//
// Route changes reach a replica out of order: peer /refresh pushes can be
// reordered or retried, and the informer-driven controller can reconcile from a
// cache that lags the API server. The store preserves a single invariant against
// that disorder — a route is only mutated by an event whose resourceVersion is at
// least as new as the one currently recorded.
//
// Crucially, this invariant is enforced on deletes as well as writes. A plain map
// delete would erase the recorded resourceVersion, so the very next write — no
// matter how stale — would land as a fresh first write and win unconditionally,
// resurrecting a route a newer event already removed. To prevent that, a delete
// leaves a short-lived tombstone carrying the resourceVersion observed at deletion
// time; a later write must beat that resourceVersion to take effect. Tombstones
// expire after a TTL (by which point the cluster has converged) and are reclaimed
// lazily on access plus an inline sweep, so the store needs no background goroutine.
package routestore

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/openkruise/agents/pkg/utils/expectations"
	"github.com/openkruise/agents/pkg/utils/proxyutils"
)

const (
	// DefaultTombstoneTTL is how long a delete tombstone is retained so that a
	// stale or reordered write cannot resurrect a route a newer event removed.
	// After this window the cluster is expected to have converged on the deletion.
	DefaultTombstoneTTL = 10 * time.Minute

	// gcThreshold is the tombstone count above which Set/Delete sweep expired
	// tombstones inline, bounding memory without a background goroutine.
	gcThreshold = 256
)

// entry is a single slot in the store: either a live route or a tombstone.
type entry struct {
	route     proxyutils.Route
	tombstone bool
	// rv is the resourceVersion governing this slot: the route's resourceVersion
	// for a live entry, or the resourceVersion observed at deletion for a tombstone.
	rv       string
	expireAt time.Time // tombstone expiry; zero for live entries
}

// Store is a concurrent, resourceVersion-aware route table. The zero value is not
// usable; construct one with New.
type Store struct {
	entries      sync.Map // id(string) -> *entry
	tombstoneTTL time.Duration
	now          func() time.Time
	// tombstones is an approximate count of tombstone slots, used only to decide
	// when to run the inline sweep. It is allowed to drift; correctness never
	// depends on it because expiry is also checked lazily on access.
	tombstones atomic.Int64
}

// Option configures a Store.
type Option func(*Store)

// WithTombstoneTTL overrides the tombstone retention window.
func WithTombstoneTTL(ttl time.Duration) Option {
	return func(s *Store) {
		if ttl > 0 {
			s.tombstoneTTL = ttl
		}
	}
}

// WithClock overrides the time source. Intended for tests.
func WithClock(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// New returns a ready-to-use Store.
func New(opts ...Option) *Store {
	s := &Store{
		tombstoneTTL: DefaultTombstoneTTL,
		now:          time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// expired reports whether e is a tombstone that has outlived its TTL.
func (s *Store) expired(e *entry, now time.Time) bool {
	return e.tombstone && !e.expireAt.IsZero() && now.After(e.expireAt)
}

// Get returns the live route for id. A tombstone (expired or not) is reported as absent.
func (s *Store) Get(id string) (proxyutils.Route, bool) {
	raw, ok := s.entries.Load(id)
	if !ok {
		return proxyutils.Route{}, false
	}
	e := raw.(*entry)
	if e.tombstone {
		return proxyutils.Route{}, false
	}
	return e.route, true
}

// Set records route under id if no newer version is already present.
//
// applied reports whether the write took effect; becameLive additionally reports
// whether it turned an absent/tombstoned slot into a live route (so callers can
// keep an O(1) live-route gauge).
func (s *Store) Set(id string, route proxyutils.Route) (applied bool, becameLive bool) {
	now := s.now()
	newE := &entry{route: route, rv: route.ResourceVersion}
	for {
		raw, loaded := s.entries.LoadOrStore(id, newE)
		if !loaded {
			// Slot was genuinely absent: first write.
			return true, true
		}
		old := raw.(*entry)

		if old.tombstone {
			// An active tombstone may only be overwritten by a STRICTLY newer
			// event — an equal resourceVersion is the very write the deletion
			// superseded, so accepting it would resurrect the route. An expired
			// tombstone is treated as absent.
			if !s.expired(old, now) && !expectations.IsResourceVersionReallyNewer(old.rv, route.ResourceVersion) {
				return false, false
			}
			if s.entries.CompareAndSwap(id, raw, newE) {
				s.tombstones.Add(-1)
				return true, true
			}
			continue
		}

		// Live entry: refuse an older-or-equal-but-older resourceVersion.
		if !expectations.IsResourceVersionNewer(old.rv, route.ResourceVersion) {
			return false, false
		}
		if s.entries.CompareAndSwap(id, raw, newE) {
			return true, false
		}
	}
}

// Delete removes the route for id, leaving a tombstone stamped with the most
// recent resourceVersion known for the slot (the larger of resourceVersion and
// the resourceVersion currently recorded). An empty resourceVersion — e.g. an
// informer not-found delete — falls back to the recorded one. removed reports
// whether a live route was actually turned into a tombstone.
func (s *Store) Delete(id string, resourceVersion string) (removed bool) {
	now := s.now()
	for {
		raw, ok := s.entries.Load(id)
		if !ok {
			// Nothing recorded yet; still drop a tombstone so an in-flight
			// stale write for this id cannot land as a fresh first write.
			tomb := &entry{tombstone: true, rv: resourceVersion, expireAt: now.Add(s.tombstoneTTL)}
			if _, loaded := s.entries.LoadOrStore(id, tomb); !loaded {
				s.tombstones.Add(1)
				s.maybeGC(now)
				return false
			}
			continue // raced with an insert; retry and handle as existing
		}

		old := raw.(*entry)
		tombRV := resourceVersion
		if expectations.IsResourceVersionNewer(resourceVersion, old.rv) {
			// old.rv is the same-or-newer of the two; keep it as the tombstone rv.
			tombRV = old.rv
		}
		tomb := &entry{tombstone: true, rv: tombRV, expireAt: now.Add(s.tombstoneTTL)}
		if !s.entries.CompareAndSwap(id, raw, tomb) {
			continue
		}
		if old.tombstone {
			// Refreshed an existing tombstone; live count unchanged.
			s.maybeGC(now)
			return false
		}
		s.tombstones.Add(1)
		s.maybeGC(now)
		return true
	}
}

// List returns a snapshot of all live routes keyed by id.
func (s *Store) List() map[string]proxyutils.Route {
	result := make(map[string]proxyutils.Route)
	s.entries.Range(func(k, v any) bool {
		if e := v.(*entry); !e.tombstone {
			result[k.(string)] = e.route
		}
		return true
	})
	return result
}

// Len returns the number of live routes.
func (s *Store) Len() int {
	n := 0
	s.entries.Range(func(_, v any) bool {
		if !v.(*entry).tombstone {
			n++
		}
		return true
	})
	return n
}

// GC reclaims expired tombstones. It is safe to call concurrently and is cheap
// when there is nothing to reclaim.
func (s *Store) GC() {
	s.gc(s.now())
}

// maybeGC sweeps only once tombstones have accumulated past the threshold, so the
// common path stays allocation- and scan-free.
func (s *Store) maybeGC(now time.Time) {
	if s.tombstones.Load() > gcThreshold {
		s.gc(now)
	}
}

func (s *Store) gc(now time.Time) {
	s.entries.Range(func(k, v any) bool {
		if e := v.(*entry); s.expired(e, now) {
			if s.entries.CompareAndDelete(k, v) {
				s.tombstones.Add(-1)
			}
		}
		return true
	})
}

// Clear drops all entries. It is not safe to call concurrently with other
// operations and is intended for tests.
func (s *Store) Clear() {
	s.entries = sync.Map{}
	s.tombstones.Store(0)
}
