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

package sandbox

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func newSandboxEventHandlerQueue(t *testing.T) workqueue.TypedRateLimitingInterface[reconcile.Request] {
	t.Helper()
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	t.Cleanup(q.ShutDown)
	return q
}

func TestSandboxEventHandler_Create(t *testing.T) {
	q := newSandboxEventHandlerQueue(t)
	sb := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx-create", Namespace: "ns-create"}}

	(&SandboxEventHandler{}).Create(context.Background(), event.TypedCreateEvent[client.Object]{Object: sb}, q)

	assert.Equal(t, 1, q.Len())
	item, _ := q.Get()
	assert.Equal(t, types.NamespacedName{Namespace: "ns-create", Name: "sbx-create"}, item.NamespacedName)
	q.Done(item)
}

func TestSandboxEventHandler_Update(t *testing.T) {
	q := newSandboxEventHandlerQueue(t)
	sb := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx-update", Namespace: "ns-update"}}

	(&SandboxEventHandler{}).Update(context.Background(), event.TypedUpdateEvent[client.Object]{ObjectNew: sb}, q)

	assert.Equal(t, 1, q.Len())
	item, _ := q.Get()
	assert.Equal(t, types.NamespacedName{Namespace: "ns-update", Name: "sbx-update"}, item.NamespacedName)
	q.Done(item)
}

func TestSandboxEventHandler_Delete(t *testing.T) {
	tests := []struct {
		name          string
		trackStart    bool
		wantDuration  bool
		wantTotalIncr float64
	}{
		{
			name:          "delete event records the deletion exactly once",
			trackStart:    true,
			wantDuration:  true,
			wantTotalIncr: 1,
		},
		{
			name:          "delete without a tracked start counts but observes no duration",
			trackStart:    false,
			wantDuration:  false,
			wantTotalIncr: 1,
		},
	}

	base := time.Date(2026, time.October, 16, 12, 0, 0, 0, time.UTC)

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := "sbx-handler-ns"
			name := fmt.Sprintf("sbx-handler-%d", i)
			key := ns + "/" + name
			t.Cleanup(func() { deletionStartTimes.Delete(key) })

			if tt.trackStart {
				// Simulate a reconcile that stored the deletion start time.
				deletionStartTimes.Store(key, base)
			}

			beforeCount := histogramSampleCount(t, sandboxDeleteDuration, ns)
			beforeTotal := counterValue(t, sandboxDeleteTotal, ns, "success")

			q := newSandboxEventHandlerQueue(t)
			sb := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
			(&SandboxEventHandler{}).Delete(context.Background(), event.TypedDeleteEvent[client.Object]{Object: sb}, q)

			// The handler enqueues the deleted sandbox like the watch it replaced.
			assert.Equal(t, 1, q.Len())
			item, _ := q.Get()
			assert.Equal(t, types.NamespacedName{Namespace: ns, Name: name}, item.NamespacedName)
			q.Done(item)

			// One Delete event increments the counter exactly once.
			assert.Equal(t, tt.wantTotalIncr, counterValue(t, sandboxDeleteTotal, ns, "success")-beforeTotal,
				"sandbox_delete_total delta")

			afterCount := histogramSampleCount(t, sandboxDeleteDuration, ns)
			if tt.wantDuration {
				assert.Equal(t, beforeCount+1, afterCount, "deletion duration sample count")
			} else {
				assert.Equal(t, beforeCount, afterCount, "deletion duration sample count")
			}

			// The tracked start is consumed either way, so a replayed event cannot
			// observe the same deletion twice.
			_, stillExists := deletionStartTimes.Load(key)
			assert.False(t, stillExists, "deletionStartTimes entry should be consumed")
		})
	}
}
