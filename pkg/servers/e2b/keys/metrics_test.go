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
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecordCacheHit_IncrementsCounter(t *testing.T) {
	cacheHits.DeleteLabelValues(storageLabelMySQL, keyTypeByKey)

	recordCacheHit(storageLabelMySQL, keyTypeByKey)
	recordCacheHit(storageLabelMySQL, keyTypeByKey)

	got := testutil.ToFloat64(cacheHits.WithLabelValues(storageLabelMySQL, keyTypeByKey))
	if got != 2 {
		t.Errorf("cache hits counter = %v, want 2", got)
	}
}

func TestRecordCacheMiss_IncrementsCounter(t *testing.T) {
	cacheMisses.DeleteLabelValues(storageLabelSecret, keyTypeByID)

	recordCacheMiss(storageLabelSecret, keyTypeByID)

	got := testutil.ToFloat64(cacheMisses.WithLabelValues(storageLabelSecret, keyTypeByID))
	if got != 1 {
		t.Errorf("cache misses counter = %v, want 1", got)
	}
}

func TestCacheCounters_LabelIsolation(t *testing.T) {
	// Ensure incrementing one (storage,key_type) partition does not leak into another.
	cacheHits.DeleteLabelValues(storageLabelMySQL, keyTypeByKey)
	cacheHits.DeleteLabelValues(storageLabelMySQL, keyTypeByID)
	cacheHits.DeleteLabelValues(storageLabelSecret, keyTypeByKey)

	recordCacheHit(storageLabelMySQL, keyTypeByKey)

	if got := testutil.ToFloat64(cacheHits.WithLabelValues(storageLabelMySQL, keyTypeByID)); got != 0 {
		t.Errorf("by_id counter unexpectedly incremented: got %v, want 0", got)
	}
	if got := testutil.ToFloat64(cacheHits.WithLabelValues(storageLabelSecret, keyTypeByKey)); got != 0 {
		t.Errorf("secret backend counter unexpectedly incremented: got %v, want 0", got)
	}
}
