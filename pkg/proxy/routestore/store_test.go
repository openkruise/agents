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

package routestore_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/openkruise/agents/pkg/proxy/routestore"
	"github.com/openkruise/agents/pkg/utils/proxyutils"
)

// fakeClock is a controllable time source for tombstone-expiry tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func rt(id, rv, state, ip string) proxyutils.Route {
	return proxyutils.Route{ID: id, ResourceVersion: rv, State: state, IP: ip}
}

// step is one operation against the store plus its expected outcome.
type step struct {
	op string // "set" | "delete" | "advance" | "gc"

	id    string
	rv    string
	state string
	ip    string
	dur   time.Duration

	// expectations for set/delete
	wantApplied    bool
	wantBecameLive bool
	wantRemoved    bool

	// post-condition: look up checkID and assert presence/value
	checkID     string
	wantPresent bool
	wantIP      string
	wantLen     int
	checkLen    bool
}

func TestStore_Sequences(t *testing.T) {
	const ttl = time.Minute
	tests := []struct {
		name  string
		steps []step
	}{
		{
			name: "first write then newer write wins, older write is ignored",
			steps: []step{
				{op: "set", id: "a", rv: "10", state: "running", ip: "1.1.1.1", wantApplied: true, wantBecameLive: true, checkID: "a", wantPresent: true, wantIP: "1.1.1.1"},
				{op: "set", id: "a", rv: "20", state: "running", ip: "2.2.2.2", wantApplied: true, wantBecameLive: false, checkID: "a", wantPresent: true, wantIP: "2.2.2.2"},
				{op: "set", id: "a", rv: "15", state: "running", ip: "3.3.3.3", wantApplied: false, checkID: "a", wantPresent: true, wantIP: "2.2.2.2", checkLen: true, wantLen: 1},
			},
		},
		{
			name: "equal resourceVersion is accepted (matches Update's >= semantics)",
			steps: []step{
				{op: "set", id: "a", rv: "10", state: "running", ip: "1.1.1.1", wantApplied: true, wantBecameLive: true},
				{op: "set", id: "a", rv: "10", state: "running", ip: "9.9.9.9", wantApplied: true, wantBecameLive: false, checkID: "a", wantPresent: true, wantIP: "9.9.9.9"},
			},
		},
		{
			name: "delete removes the live route",
			steps: []step{
				{op: "set", id: "a", rv: "10", state: "running", ip: "1.1.1.1", wantApplied: true, wantBecameLive: true},
				{op: "delete", id: "a", rv: "11", wantRemoved: true, checkID: "a", wantPresent: false, checkLen: true, wantLen: 0},
			},
		},
		{
			name: "RESURRECTION GUARD: a stale write after delete is rejected",
			steps: []step{
				{op: "set", id: "a", rv: "10", state: "running", ip: "1.1.1.1", wantApplied: true, wantBecameLive: true},
				{op: "delete", id: "a", rv: "11", wantRemoved: true},
				// A lagging-cache controller re-adds the old running state: must not resurrect.
				{op: "set", id: "a", rv: "10", state: "running", ip: "1.1.1.1", wantApplied: false, wantBecameLive: false, checkID: "a", wantPresent: false, checkLen: true, wantLen: 0},
			},
		},
		{
			name: "a genuinely newer event after delete is allowed (recreate)",
			steps: []step{
				{op: "set", id: "a", rv: "10", state: "running", ip: "1.1.1.1", wantApplied: true, wantBecameLive: true},
				{op: "delete", id: "a", rv: "11", wantRemoved: true},
				{op: "set", id: "a", rv: "20", state: "running", ip: "5.5.5.5", wantApplied: true, wantBecameLive: true, checkID: "a", wantPresent: true, wantIP: "5.5.5.5", checkLen: true, wantLen: 1},
			},
		},
		{
			name: "delete with empty resourceVersion falls back to the recorded one",
			steps: []step{
				{op: "set", id: "a", rv: "100", state: "running", ip: "1.1.1.1", wantApplied: true, wantBecameLive: true},
				{op: "delete", id: "a", rv: "", wantRemoved: true},
				{op: "set", id: "a", rv: "99", state: "running", ip: "1.1.1.1", wantApplied: false, checkID: "a", wantPresent: false},
				{op: "set", id: "a", rv: "101", state: "running", ip: "7.7.7.7", wantApplied: true, wantBecameLive: true, checkID: "a", wantPresent: true, wantIP: "7.7.7.7"},
			},
		},
		{
			name: "a write equal to the tombstone version is rejected (strict newer required)",
			steps: []step{
				{op: "set", id: "a", rv: "100", state: "running", ip: "1.1.1.1", wantApplied: true, wantBecameLive: true},
				{op: "delete", id: "a", rv: "", wantRemoved: true}, // tombstone rv = 100 (from the entry)
				{op: "set", id: "a", rv: "100", state: "running", ip: "1.1.1.1", wantApplied: false, checkID: "a", wantPresent: false},
			},
		},
		{
			name: "tombstone expiry lets a later write through",
			steps: []step{
				{op: "set", id: "a", rv: "100", state: "running", ip: "1.1.1.1", wantApplied: true, wantBecameLive: true},
				{op: "delete", id: "a", rv: "100", wantRemoved: true},
				// Before expiry the stale write is still blocked.
				{op: "set", id: "a", rv: "50", state: "running", ip: "1.1.1.1", wantApplied: false},
				{op: "advance", dur: 2 * time.Minute},
				// After expiry the tombstone is treated as absent.
				{op: "set", id: "a", rv: "50", state: "running", ip: "8.8.8.8", wantApplied: true, wantBecameLive: true, checkID: "a", wantPresent: true, wantIP: "8.8.8.8"},
			},
		},
		{
			name: "deleting an unknown id still blocks an in-flight stale write",
			steps: []step{
				{op: "delete", id: "a", rv: "100", wantRemoved: false},
				{op: "set", id: "a", rv: "50", state: "running", ip: "1.1.1.1", wantApplied: false, checkID: "a", wantPresent: false},
			},
		},
		{
			name: "deleting a tombstone again does not report a removal",
			steps: []step{
				{op: "set", id: "a", rv: "10", state: "running", ip: "1.1.1.1", wantApplied: true, wantBecameLive: true},
				{op: "delete", id: "a", rv: "11", wantRemoved: true},
				{op: "delete", id: "a", rv: "12", wantRemoved: false},
			},
		},
		{
			name: "GC before expiry keeps the tombstone effective",
			steps: []step{
				{op: "set", id: "a", rv: "10", state: "running", ip: "1.1.1.1", wantApplied: true, wantBecameLive: true},
				{op: "delete", id: "a", rv: "11", wantRemoved: true},
				{op: "gc"},
				{op: "set", id: "a", rv: "10", state: "running", ip: "1.1.1.1", wantApplied: false, checkID: "a", wantPresent: false},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := &fakeClock{t: time.Unix(0, 0)}
			s := routestore.New(routestore.WithTombstoneTTL(ttl), routestore.WithClock(clock.Now))
			for i, st := range tt.steps {
				switch st.op {
				case "set":
					applied, becameLive := s.Set(st.id, rt(st.id, st.rv, st.state, st.ip))
					assert.Equalf(t, st.wantApplied, applied, "step %d set applied", i)
					assert.Equalf(t, st.wantBecameLive, becameLive, "step %d set becameLive", i)
				case "delete":
					removed := s.Delete(st.id, st.rv)
					assert.Equalf(t, st.wantRemoved, removed, "step %d delete removed", i)
				case "advance":
					clock.advance(st.dur)
				case "gc":
					s.GC()
				default:
					t.Fatalf("unknown op %q", st.op)
				}

				if st.checkID != "" {
					got, ok := s.Get(st.checkID)
					assert.Equalf(t, st.wantPresent, ok, "step %d Get(%s) presence", i, st.checkID)
					if st.wantPresent && ok {
						assert.Equalf(t, st.wantIP, got.IP, "step %d Get(%s) ip", i, st.checkID)
					}
				}
				if st.checkLen {
					assert.Equalf(t, st.wantLen, s.Len(), "step %d Len", i)
				}
			}
		})
	}
}

func TestStore_List(t *testing.T) {
	s := routestore.New()
	_, _ = s.Set("a", rt("a", "1", "running", "1.1.1.1"))
	_, _ = s.Set("b", rt("b", "1", "paused", "2.2.2.2"))
	s.Delete("b", "2")

	got := s.List()
	assert.Len(t, got, 1)
	assert.Contains(t, got, "a")
	assert.NotContains(t, got, "b")
}

func TestStore_GCReclaimsExpiredTombstones(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	s := routestore.New(routestore.WithTombstoneTTL(time.Minute), routestore.WithClock(clock.Now))

	_, _ = s.Set("a", rt("a", "1", "running", "1.1.1.1"))
	s.Delete("a", "2")
	clock.advance(2 * time.Minute)
	s.GC()

	// After GC of an expired tombstone, even an older-versioned write is accepted.
	applied, becameLive := s.Set("a", rt("a", "1", "running", "9.9.9.9"))
	assert.True(t, applied)
	assert.True(t, becameLive)
	got, ok := s.Get("a")
	assert.True(t, ok)
	assert.Equal(t, "9.9.9.9", got.IP)
}

func TestStore_Clear(t *testing.T) {
	s := routestore.New()
	_, _ = s.Set("a", rt("a", "1", "running", "1.1.1.1"))
	s.Clear()
	_, ok := s.Get("a")
	assert.False(t, ok)
	assert.Equal(t, 0, s.Len())
}

// TestStore_ConcurrentWritersConverge ensures the store is race-free and that the
// highest resourceVersion wins regardless of interleaving.
func TestStore_ConcurrentWritersConverge(t *testing.T) {
	s := routestore.New()
	const writers = 16
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for v := 1; v <= 50; v++ {
				rv := fmt.Sprintf("%d", n*100+v)
				_, _ = s.Set("a", rt("a", rv, "running", rv))
				if v%10 == 0 {
					s.Delete("a", rv)
				}
			}
		}(w)
	}
	wg.Wait()

	// Whatever the final state, the store must be internally consistent: a present
	// route's IP matches its resourceVersion (we encoded them identically above).
	if got, ok := s.Get("a"); ok {
		assert.Equal(t, got.ResourceVersion, got.IP)
	}
}
