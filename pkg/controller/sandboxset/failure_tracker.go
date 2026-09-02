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

package sandboxset

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/pkg/utils"
)

// failureTracker records terminally failed Sandboxes per SandboxSet so that
// scale-up into a template that cannot start can be stopped instead of
// repeating at maxUnavailable width.
//
// Two properties of the surrounding code decide its shape:
//
//   - The same Dead Sandbox is regrouped on every reconcile until it actually
//     disappears. deleteDeadSandboxes skips objects that are already
//     terminating, while groupAllSandboxes keeps grouping them, and a failed
//     delete requeues with the Sandbox still present. Counting sightings would
//     therefore count one failure many times, so records are keyed on Sandbox
//     UID and the first sighting is the one that counts.
//   - GetSandboxState reports DeletionTimestamp before Phase, so a Sandbox that
//     begins terminating between two reconciles is reported as ResourceDeleted
//     on the later ones even though it failed. The reason is captured with the
//     first sighting of a UID and is never overwritten.
type failureTracker struct {
	sync.Mutex
	// key: controller key, namespace/name of the SandboxSet
	controllers map[string]map[types.UID]failureRecord
}

type failureRecord struct {
	reason   string
	observed time.Time
}

func newFailureTracker() *failureTracker {
	return &failureTracker{controllers: make(map[string]map[types.UID]failureRecord)}
}

// observe records uid against controllerKey the first time it is seen. Later
// calls for the same UID keep the original reason and timestamp, which is what
// makes the count independent of how many reconciles the Sandbox survives.
func (f *failureTracker) observe(controllerKey string, uid types.UID, reason string, now time.Time) {
	if uid == "" {
		return
	}
	f.Lock()
	defer f.Unlock()

	records := f.controllers[controllerKey]
	if records == nil {
		records = make(map[types.UID]failureRecord)
		f.controllers[controllerKey] = records
	}
	if _, seen := records[uid]; seen {
		return
	}
	records[uid] = failureRecord{reason: reason, observed: now}
}

// terminalFailures returns how many distinct Sandboxes failed for controllerKey
// within window, and drops every record that has aged out of it. Only
// ResourceFailed counts: a Sandbox stuck in Pending is reported as Creating, so
// it consumes the maxUnavailable budget on its own and needs no counter.
func (f *failureTracker) terminalFailures(controllerKey string, window time.Duration, now time.Time) int {
	f.Lock()
	defer f.Unlock()

	records := f.controllers[controllerKey]
	if records == nil {
		return 0
	}
	cutoff := now.Add(-window)
	count := 0
	for uid, record := range records {
		if record.observed.Before(cutoff) {
			delete(records, uid)
			continue
		}
		if record.reason == utils.ReasonResourceFailed {
			count++
		}
	}
	if len(records) == 0 {
		delete(f.controllers, controllerKey)
	}
	return count
}

// forget drops every record for controllerKey. Called when the SandboxSet is
// gone, so a deleted and recreated pool does not inherit the old counts.
func (f *failureTracker) forget(controllerKey string) {
	f.Lock()
	defer f.Unlock()
	delete(f.controllers, controllerKey)
}
