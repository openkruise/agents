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

// Package tokencache provides a thread-safe LRU cache for tokens obtained
// from the credential provider. Tokens are cached by credentialProviderName +
// resourceId and expire after a configurable TTL. The LRU backing uses
// github.com/hashicorp/golang-lru/v2.
package tokencache

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

const (
	// Environment variables for cache configuration.
	ttlEnvVar      = "TOKEN_CACHE_TTL"
	maxSizeEnvVar  = "TOKEN_CACHE_MAX_SIZE"
	defaultTTL     = 3 * time.Hour
	defaultMaxSize = 10000
)

// cacheKey is the composite key for a cached token.
type cacheKey struct {
	credentialProviderName string
	resourceID             string
}

// cacheEntry holds a cached token with metadata.
type cacheEntry struct {
	token     string
	expiresAt time.Time
}

// isExpired returns true if the entry has passed its expiration time.
func (e cacheEntry) isExpired() bool {
	return time.Now().After(e.expiresAt)
}

// Cache is a thread-safe LRU cache for tokens with configurable TTL and capacity.
//
// A plain Mutex (not RWMutex) is intentional: every cache access — including
// Get — mutates the LRU recency order, and the underlying lru.Cache already
// serializes operations on its own internal mutex. RWMutex would only add
// per-op overhead without unlocking real read parallelism.
type Cache struct {
	mu  sync.Mutex
	lru *lru.Cache[cacheKey, cacheEntry]
	ttl time.Duration
}

// NewCache creates a new token cache with the given TTL and max size.
// If ttl or maxSize is zero or negative, defaults are used.
func NewCache(ttl time.Duration, maxSize int) *Cache {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	if maxSize <= 0 {
		maxSize = defaultMaxSize
	}
	l, err := lru.New[cacheKey, cacheEntry](maxSize)
	if err != nil {
		// maxSize <= 0 is the only error case, already guarded above.
		panic(fmt.Sprintf("failed to create LRU cache: %v", err))
	}
	return &Cache{
		lru: l,
		ttl: ttl,
	}
}

// NewCacheFromEnv creates a token cache configured from environment variables.
// TOKEN_CACHE_TTL overrides the default TTL (parsed via time.ParseDuration).
// TOKEN_CACHE_MAX_SIZE overrides the default max size.
func NewCacheFromEnv() *Cache {
	ttl := defaultTTL
	if v := os.Getenv(ttlEnvVar); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
			ttl = parsed
		}
	}

	maxSize := defaultMaxSize
	if v := os.Getenv(maxSizeEnvVar); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			maxSize = parsed
		}
	}

	return NewCache(ttl, maxSize)
}

// Get retrieves a token from the cache by credentialProviderName and resourceID.
// Returns the token and true if found and not expired, or ("", false) otherwise.
// On a successful get, the entry is moved to the front of the LRU list.
//
// The whole get / expired-check / remove sequence runs under a single write
// lock so a concurrent Set cannot have its fresh entry deleted by a stale
// expired-check from this goroutine.
func (c *Cache) Get(credentialProviderName, resourceID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := cacheKey{credentialProviderName: credentialProviderName, resourceID: resourceID}
	entry, ok := c.lru.Get(key)
	if !ok {
		return "", false
	}
	if entry.isExpired() {
		c.lru.Remove(key)
		return "", false
	}
	return entry.token, true
}

// Set adds or updates a token in the cache.
// If the cache is at capacity, the least recently used entry is evicted.
func (c *Cache) Set(credentialProviderName, resourceID, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := cacheKey{credentialProviderName: credentialProviderName, resourceID: resourceID}
	entry := cacheEntry{token: token, expiresAt: time.Now().Add(c.ttl)}
	c.lru.Add(key, entry)
}

// Delete removes an entry from the cache.
func (c *Cache) Delete(credentialProviderName, resourceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := cacheKey{credentialProviderName: credentialProviderName, resourceID: resourceID}
	c.lru.Remove(key)
}

// Len returns the current number of entries in the cache.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

// ConfigInfo returns a human-readable string of cache configuration for logging.
func ConfigInfo() string {
	ttl := defaultTTL
	if v := os.Getenv(ttlEnvVar); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
			ttl = parsed
		}
	}
	maxSize := defaultMaxSize
	if v := os.Getenv(maxSizeEnvVar); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			maxSize = parsed
		}
	}
	return fmt.Sprintf("TTL=%s, maxSize=%d", ttl, maxSize)
}
