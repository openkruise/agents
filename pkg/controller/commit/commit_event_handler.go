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

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	jobutil "github.com/openkruise/agents/pkg/controller/commit/job"
)

var _ handler.TypedEventHandler[client.Object, reconcile.Request] = &enqueueRequestForJob{}

// enqueueRequestForJob enqueues a Commit reconcile request only when the underlying
// commit Job reaches a terminal state (Complete or Failed). Intermediate Job/Pod
// status changes are filtered out to avoid unnecessary reconciles.
type enqueueRequestForJob struct{}

func (h *enqueueRequestForJob) addEvent(q workqueue.TypedRateLimitingInterface[reconcile.Request], obj runtime.Object) {
	job, ok := obj.(*batchv1.Job)
	if !ok {
		return
	}

	commitName, ok := commitOwnerName(job)
	if !ok {
		return
	}

	complete, _ := jobutil.IsJobCompleted(job)
	if !complete {
		return
	}
	q.Add(reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: job.Namespace,
			Name:      commitName,
		},
	})
}

func (h *enqueueRequestForJob) Create(_ context.Context, evt event.TypedCreateEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.addEvent(q, evt.Object)
}

func (h *enqueueRequestForJob) Delete(_ context.Context, evt event.TypedDeleteEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	job, ok := evt.Object.(*batchv1.Job)
	if !ok {
		return
	}
	commitName, ok := commitOwnerName(job)
	if !ok {
		return
	}
	q.Add(reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: job.Namespace,
			Name:      commitName,
		},
	})
}

func (h *enqueueRequestForJob) Generic(_ context.Context, evt event.TypedGenericEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
}

func (h *enqueueRequestForJob) Update(_ context.Context, evt event.TypedUpdateEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.addEvent(q, evt.ObjectNew)
}

// commitOwnerName extracts the Commit name from the Job's controller OwnerReference.
func commitOwnerName(job *batchv1.Job) (string, bool) {
	for _, ref := range job.OwnerReferences {
		if ref.Kind == "Commit" && ref.APIVersion == agentsv1alpha1.SchemeGroupVersion.String() &&
			ref.Controller != nil && *ref.Controller {
			return ref.Name, true
		}
	}
	return "", false
}
