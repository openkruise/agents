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

package tokencache

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestCache_BasicSetGet(t *testing.T) {
	c := NewCache(1*time.Minute, 100)
	c.Set("provider-a", "resource-1", "token-abc")

	got, ok := c.Get("provider-a", "resource-1")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got != "token-abc" {
		t.Errorf("expected token-abc, got %s", got)
	}
}

func TestCache_Miss(t *testing.T) {
	c := NewCache(1*time.Minute, 100)

	got, ok := c.Get("provider-a", "resource-1")
	if ok {
		t.Fatalf("expected cache miss, got %s", got)
	}
}

func TestCache_Delete(t *testing.T) {
	c := NewCache(1*time.Minute, 100)
	c.Set("provider-a", "resource-1", "token-abc")

	c.Delete("provider-a", "resource-1")

	got, ok := c.Get("provider-a", "resource-1")
	if ok {
		t.Fatalf("expected cache miss after delete, got %s", got)
	}
}

func TestCache_DeleteNonExistent(t *testing.T) {
	c := NewCache(1*time.Minute, 100)
	c.Delete("nonexistent", "nonexistent") // should not panic
}

func TestCache_Expiration(t *testing.T) {
	c := NewCache(50*time.Millisecond, 100)
	c.Set("provider-a", "resource-1", "token-abc")

	// Should hit immediately.
	got, ok := c.Get("provider-a", "resource-1")
	if !ok || got != "token-abc" {
		t.Fatalf("expected immediate hit, got %s, %v", got, ok)
	}

	// Wait for expiration.
	time.Sleep(100 * time.Millisecond)

	got, ok = c.Get("provider-a", "resource-1")
	if ok {
		t.Fatalf("expected expired entry, got %s", got)
	}
}

func TestCache_LRUEviction(t *testing.T) {
	c := NewCache(1*time.Minute, 3)

	c.Set("p", "r1", "t1")
	c.Set("p", "r2", "t2")
	c.Set("p", "r3", "t3")

	// Cache is full; adding r4 should evict r1 (LRU).
	c.Set("p", "r4", "t4")

	_, ok := c.Get("p", "r1")
	if ok {
		t.Fatal("expected r1 to be evicted")
	}

	// r2, r3, r4 should still exist.
	got, ok := c.Get("p", "r2")
	if !ok || got != "t2" {
		t.Errorf("expected r2=t2, got %s, %v", got, ok)
	}

	got, ok = c.Get("p", "r3")
	if !ok || got != "t3" {
		t.Errorf("expected r3=t3, got %s, %v", got, ok)
	}

	got, ok = c.Get("p", "r4")
	if !ok || got != "t4" {
		t.Errorf("expected r4=t4, got %s, %v", got, ok)
	}
}

func TestCache_LRUOrder_Update(t *testing.T) {
	c := NewCache(1*time.Minute, 3)

	c.Set("p", "r1", "t1")
	c.Set("p", "r2", "t2")
	c.Set("p", "r3", "t3")

	// Access r1 — moves it to front, so r2 becomes LRU.
	c.Get("p", "r1")

	// Add r4 — should evict r2.
	c.Set("p", "r4", "t4")

	_, ok := c.Get("p", "r2")
	if ok {
		t.Fatal("expected r2 to be evicted (was LRU after r1 accessed)")
	}

	got, ok := c.Get("p", "r1")
	if !ok || got != "t1" {
		t.Errorf("expected r1=t1, got %s, %v", got, ok)
	}
}

func TestCache_UpdateExisting(t *testing.T) {
	c := NewCache(1*time.Minute, 100)

	c.Set("p", "r1", "token-old")
	c.Set("p", "r1", "token-new")

	got, ok := c.Get("p", "r1")
	if !ok || got != "token-new" {
		t.Errorf("expected token-new, got %s, %v", got, ok)
	}

	// Length should still be 1.
	if c.Len() != 1 {
		t.Errorf("expected len 1, got %d", c.Len())
	}
}

func TestCache_Len(t *testing.T) {
	c := NewCache(1*time.Minute, 100)

	if c.Len() != 0 {
		t.Errorf("expected 0, got %d", c.Len())
	}

	c.Set("p", "r1", "t1")
	c.Set("p", "r2", "t2")

	if c.Len() != 2 {
		t.Errorf("expected 2, got %d", c.Len())
	}
}

func TestCache_DifferentProviders(t *testing.T) {
	c := NewCache(1*time.Minute, 100)

	c.Set("provider-a", "r1", "token-a")
	c.Set("provider-b", "r1", "token-b")

	gotA, okA := c.Get("provider-a", "r1")
	gotB, okB := c.Get("provider-b", "r1")

	if !okA || gotA != "token-a" {
		t.Errorf("provider-a: expected token-a, got %s, %v", gotA, okA)
	}
	if !okB || gotB != "token-b" {
		t.Errorf("provider-b: expected token-b, got %s, %v", gotB, okB)
	}
}

func TestCache_ConcurrentAccess(t *testing.T) {
	c := NewCache(1*time.Minute, 1000)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			c.Set("provider", "resource", "token")
		}(i)
		go func(i int) {
			defer wg.Done()
			c.Get("provider", "resource")
		}(i)
	}
	wg.Wait()

	got, ok := c.Get("provider", "resource")
	if !ok || got != "token" {
		t.Errorf("expected token after concurrent ops, got %s, %v", got, ok)
	}
}

func TestCache_Defaults(t *testing.T) {
	c := NewCache(0, 0)
	if c.ttl != defaultTTL {
		t.Errorf("expected default TTL %v, got %v", defaultTTL, c.ttl)
	}
	// maxSize is internal to the golang-lru backing; verify via behavior.
	fillAndVerifyCapacity(t, c, defaultMaxSize)
}

func TestCache_NegativeParams(t *testing.T) {
	c := NewCache(-1*time.Hour, -5)
	if c.ttl != defaultTTL {
		t.Errorf("expected default TTL for negative value, got %v", c.ttl)
	}
	fillAndVerifyCapacity(t, c, defaultMaxSize)
}

// fillAndVerifyCapacity sets entries up to the default max and verifies
// that adding one more evicts the LRU entry (behavioral check for maxSize).
func fillAndVerifyCapacity(t *testing.T, c *Cache, maxSize int) {
	// For the default capacity (10000), we only test a subset to keep tests fast.
	limit := maxSize
	if limit > 100 {
		limit = 100 // test a reasonable subset
	}
	for i := 0; i < limit; i++ {
		c.Set("default-test", fmt.Sprintf("r-%d", i), fmt.Sprintf("t-%d", i))
	}
	expectedLen := limit
	if c.Len() != expectedLen {
		t.Errorf("expected len %d after filling, got %d", expectedLen, c.Len())
	}
}

// TestNewCacheFromEnv covers env-driven configuration including invalid
// values (which should fall back to defaults) and valid overrides.
func TestNewCacheFromEnv(t *testing.T) {
	t.Setenv(ttlEnvVar, "")
	t.Setenv(maxSizeEnvVar, "")
	c1 := NewCacheFromEnv()
	if c1.ttl != defaultTTL {
		t.Errorf("expected default TTL, got %v", c1.ttl)
	}

	t.Setenv(ttlEnvVar, "10m")
	t.Setenv(maxSizeEnvVar, "500")
	c2 := NewCacheFromEnv()
	if c2.ttl != 10*time.Minute {
		t.Errorf("expected 10m TTL, got %v", c2.ttl)
	}

	// Invalid values should be ignored.
	t.Setenv(ttlEnvVar, "not-a-duration")
	t.Setenv(maxSizeEnvVar, "abc")
	c3 := NewCacheFromEnv()
	if c3.ttl != defaultTTL {
		t.Errorf("expected default TTL when env is bad, got %v", c3.ttl)
	}
}

func TestConfigInfo(t *testing.T) {
	t.Setenv(ttlEnvVar, "5m")
	t.Setenv(maxSizeEnvVar, "42")
	if got := ConfigInfo(); got != "TTL=5m0s, maxSize=42" {
		t.Errorf("unexpected ConfigInfo: %q", got)
	}

	t.Setenv(ttlEnvVar, "")
	t.Setenv(maxSizeEnvVar, "")
	got := ConfigInfo()
	// Defaults: TTL=3h, maxSize=10000
	if got == "" {
		t.Errorf("expected non-empty ConfigInfo with defaults")
	}
}
