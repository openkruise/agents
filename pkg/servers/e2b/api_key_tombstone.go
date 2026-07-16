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

package e2b

import (
	"sync"
	"time"
)

const defaultAPIKeyDeletionTombstoneTTL = 10 * time.Minute

type apiKeyDeletionTombstones struct {
	ttl     time.Duration
	entries sync.Map
}

func newAPIKeyDeletionTombstones(ttl time.Duration) *apiKeyDeletionTombstones {
	return &apiKeyDeletionTombstones{ttl: ttl}
}

func (t *apiKeyDeletionTombstones) Add(id string) {
	if t == nil || id == "" {
		return
	}
	t.entries.Store(id, time.Now().Add(t.ttl))
}

func (t *apiKeyDeletionTombstones) Contains(id string) bool {
	if t == nil || id == "" {
		return false
	}
	value, ok := t.entries.Load(id)
	if !ok {
		return false
	}
	expiresAt, ok := value.(time.Time)
	if !ok || time.Now().After(expiresAt) {
		t.entries.Delete(id)
		return false
	}
	return true
}
