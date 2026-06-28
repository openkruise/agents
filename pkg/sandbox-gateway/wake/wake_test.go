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

package wake

import (
	"sync/atomic"
	"testing"
)

func TestInitWakerAndGetWaker(t *testing.T) {
	// Reset before test
	var zero atomic.Pointer[Waker]
	defaultWaker = zero

	// Before init, GetWaker returns nil
	if w := GetWaker(); w != nil {
		t.Error("GetWaker() should return nil before InitWaker is called")
	}

	// After init with nil cache, GetWaker returns non-nil (but Wake would fail)
	InitWaker(nil)
	w := GetWaker()
	if w == nil {
		t.Error("GetWaker() should return non-nil after InitWaker is called")
	}

	// The waker's cache field should be nil
	if w.cache != nil {
		t.Error("Waker.cache should be nil when initialized with nil cache")
	}

	// Reset for other tests
	defaultWaker = zero
}
