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

package commit

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	jobutil "github.com/openkruise/agents/pkg/job"
)

func newTestQueue() workqueue.TypedRateLimitingInterface[reconcile.Request] {
	return workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
}

func TestEnqueueRequestForJob_CompletedJob(t *testing.T) {
	handler := &enqueueRequestForJob{}
	q := newTestQueue()
	defer q.ShutDown()

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-job-abc",
			Namespace: "default",
			Labels: map[string]string{
				jobutil.LabelCommitName: "my-commit",
			},
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}

	handler.Update(context.TODO(), event.TypedUpdateEvent[client.Object]{
		ObjectNew: job,
	}, q)

	if q.Len() != 1 {
		t.Fatalf("expected 1 item in queue, got %d", q.Len())
	}
	item, _ := q.Get()
	if item.NamespacedName != (types.NamespacedName{Namespace: "default", Name: "my-commit"}) {
		t.Errorf("unexpected queue item: %v", item)
	}
}

func TestEnqueueRequestForJob_IncompleteJob(t *testing.T) {
	handler := &enqueueRequestForJob{}
	q := newTestQueue()
	defer q.ShutDown()

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-job-abc",
			Namespace: "default",
			Labels: map[string]string{
				jobutil.LabelCommitName: "my-commit",
			},
		},
		Status: batchv1.JobStatus{
			Active: 1,
		},
	}

	handler.Update(context.TODO(), event.TypedUpdateEvent[client.Object]{
		ObjectNew: job,
	}, q)

	if q.Len() != 0 {
		t.Errorf("expected 0 items for incomplete job, got %d", q.Len())
	}
}

func TestEnqueueRequestForJob_NoCommitLabel(t *testing.T) {
	handler := &enqueueRequestForJob{}
	q := newTestQueue()
	defer q.ShutDown()

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "some-other-job",
			Namespace: "default",
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}

	handler.Create(context.TODO(), event.TypedCreateEvent[client.Object]{
		Object: job,
	}, q)

	if q.Len() != 0 {
		t.Errorf("expected 0 items for job without commit label, got %d", q.Len())
	}
}

func TestEnqueueRequestForJob_NotAJob(t *testing.T) {
	handler := &enqueueRequestForJob{}
	q := newTestQueue()
	defer q.ShutDown()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "not-a-job", Namespace: "default"},
	}

	handler.Create(context.TODO(), event.TypedCreateEvent[client.Object]{
		Object: pod,
	}, q)

	if q.Len() != 0 {
		t.Errorf("expected 0 items for non-job object, got %d", q.Len())
	}
}

func TestEnqueueRequestForJob_FailedJob(t *testing.T) {
	handler := &enqueueRequestForJob{}
	q := newTestQueue()
	defer q.ShutDown()

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-job-failed",
			Namespace: "ns1",
			Labels: map[string]string{
				jobutil.LabelCommitName: "failed-commit",
			},
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			},
		},
	}

	handler.Create(context.TODO(), event.TypedCreateEvent[client.Object]{
		Object: job,
	}, q)

	if q.Len() != 1 {
		t.Fatalf("expected 1 item for failed job, got %d", q.Len())
	}
	item, _ := q.Get()
	if item.NamespacedName != (types.NamespacedName{Namespace: "ns1", Name: "failed-commit"}) {
		t.Errorf("unexpected queue item: %v", item)
	}
}

func TestEnqueueRequestForJob_DeleteAndGenericNoOp(t *testing.T) {
	handler := &enqueueRequestForJob{}
	q := newTestQueue()
	defer q.ShutDown()

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-job-del",
			Namespace: "default",
			Labels: map[string]string{
				jobutil.LabelCommitName: "commit-x",
			},
		},
	}

	handler.Delete(context.TODO(), event.TypedDeleteEvent[client.Object]{
		Object: job,
	}, q)
	if q.Len() != 0 {
		t.Errorf("Delete should not enqueue, got %d", q.Len())
	}

	handler.Generic(context.TODO(), event.TypedGenericEvent[client.Object]{
		Object: job,
	}, q)
	if q.Len() != 0 {
		t.Errorf("Generic should not enqueue, got %d", q.Len())
	}
}
