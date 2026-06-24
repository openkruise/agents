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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAPIKeyDeletionTombstones(t *testing.T) {
	t.Run("nil and empty id are ignored", func(t *testing.T) {
		var tombstones *apiKeyDeletionTombstones

		tombstones.Add("deleted-key")
		assert.False(t, tombstones.Contains("deleted-key"))

		tombstones = newAPIKeyDeletionTombstones(time.Minute)
		tombstones.Add("")
		assert.False(t, tombstones.Contains(""))
	})

	t.Run("active tombstone is visible", func(t *testing.T) {
		tombstones := newAPIKeyDeletionTombstones(time.Minute)

		tombstones.Add("deleted-key")

		assert.True(t, tombstones.Contains("deleted-key"))
		assert.False(t, tombstones.Contains("other-key"))
	})

	t.Run("expired tombstone is removed", func(t *testing.T) {
		tombstones := newAPIKeyDeletionTombstones(-time.Second)
		tombstones.Add("deleted-key")

		assert.False(t, tombstones.Contains("deleted-key"))
		assert.False(t, tombstones.Contains("deleted-key"))
	})
}
