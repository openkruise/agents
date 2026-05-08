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

package keys

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Storage backend label values.
const (
	storageLabelMySQL  = "mysql"
	storageLabelSecret = "secret"
)

// Key-type label values, matching the two distinct lookup paths exposed by
// every KeyStorage implementation.
const (
	keyTypeByKey = "by_key"
	keyTypeByID  = "by_id"
)

var (
	// e2b_key_cache_hits_total counts API key lookups that were answered from
	// the in-memory cache, without falling through to the storage backend.
	cacheHits = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "e2b_key_cache_hits_total",
			Help: "Total number of API key cache hits, partitioned by storage backend and key-type lookup path.",
		},
		[]string{"storage", "key_type"},
	)

	// e2b_key_cache_misses_total counts API key lookups that missed the
	// in-memory cache. For mysql this also implies a DB round-trip; for the
	// secret backend it implies the key wasn't present at all.
	cacheMisses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "e2b_key_cache_misses_total",
			Help: "Total number of API key cache misses, partitioned by storage backend and key-type lookup path.",
		},
		[]string{"storage", "key_type"},
	)
)

func init() {
	metrics.Registry.MustRegister(cacheHits, cacheMisses)
}

// recordCacheHit increments the cache-hit counter for the given backend and lookup path.
func recordCacheHit(storage, keyType string) {
	cacheHits.WithLabelValues(storage, keyType).Inc()
}

// recordCacheMiss increments the cache-miss counter for the given backend and lookup path.
func recordCacheMiss(storage, keyType string) {
	cacheMisses.WithLabelValues(storage, keyType).Inc()
}
