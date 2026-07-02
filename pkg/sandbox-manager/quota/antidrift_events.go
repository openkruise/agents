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

package quota

import (
	"context"

	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

func (d *AntiDriftDriver) enqueueQuotaEvent(event infra.QuotaSandboxEvent) {
	if d == nil || d.backend == nil || d.eventQueue == nil {
		return
	}
	user := event.Snapshot.Owner
	lockString := event.Snapshot.LockString
	if user == "" || lockString == "" {
		return
	}

	key := eventKey(user, lockString)
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	d.pendingEvents[key] = event
	d.mu.Unlock()

	d.eventQueue.Add(key)
}

func (d *AntiDriftDriver) processNextQuotaEvent(ctx context.Context) bool {
	if d == nil || d.eventQueue == nil {
		return false
	}
	key, shutdown := d.eventQueue.Get()
	if shutdown {
		return false
	}
	defer d.eventQueue.Done(key)
	d.mu.Lock()
	stopped := d.stopped
	d.mu.Unlock()
	if stopped {
		return false
	}

	event, ok := d.popPendingQuotaEvent(key)
	if !ok {
		return true
	}
	d.reconcileQuotaEvent(ctx, event)
	return true
}

func (d *AntiDriftDriver) popPendingQuotaEvent(key string) (infra.QuotaSandboxEvent, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	event, ok := d.pendingEvents[key]
	if ok {
		delete(d.pendingEvents, key)
	}
	return event, ok
}

func (d *AntiDriftDriver) runQuotaEventWorker(ctx context.Context) {
	for d.processNextQuotaEvent(ctx) {
	}
}
