/*
Copyright 2025 The Kruise Authors.

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
	"context"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/fieldindex"
)

// SandboxTemplateEventHandler fans SandboxTemplate Update events out to every
// SandboxSet that references the template, so that template spec changes drive
// a rolling update on the managed Sandboxes.
//
// Only Update is meaningful: Create / Delete / Generic are left as no-ops
// because the SandboxSet controller already re-reconciles on its own watches
// and error backoff, and Generic events are never produced for informer-backed
// watches (Generic is only emitted by source.Channel).
type SandboxTemplateEventHandler struct {
	client client.Reader
}

var _ handler.EventHandler = &SandboxTemplateEventHandler{}

func NewSandboxTemplateEventHandler(c client.Reader) *SandboxTemplateEventHandler {
	return &SandboxTemplateEventHandler{client: c}
}

func (h *SandboxTemplateEventHandler) Create(context.Context, event.TypedCreateEvent[client.Object], workqueue.TypedRateLimitingInterface[reconcile.Request]) {
}

func (h *SandboxTemplateEventHandler) Update(ctx context.Context, evt event.TypedUpdateEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if evt.ObjectOld == nil || evt.ObjectNew == nil {
		return
	}
	// Skip status-only updates: only spec mutations bump Generation.
	if evt.ObjectOld.GetGeneration() == evt.ObjectNew.GetGeneration() {
		return
	}
	h.enqueueReferencingSandboxSets(ctx, evt.ObjectNew, w)
}

func (h *SandboxTemplateEventHandler) Delete(context.Context, event.TypedDeleteEvent[client.Object], workqueue.TypedRateLimitingInterface[reconcile.Request]) {
}

func (h *SandboxTemplateEventHandler) Generic(context.Context, event.TypedGenericEvent[client.Object], workqueue.TypedRateLimitingInterface[reconcile.Request]) {
}

func (h *SandboxTemplateEventHandler) enqueueReferencingSandboxSets(
	ctx context.Context,
	obj client.Object,
	w workqueue.TypedRateLimitingInterface[reconcile.Request],
) {
	log := logf.FromContext(ctx).WithValues("sandboxtemplate", client.ObjectKeyFromObject(obj))
	sbsList := &agentsv1alpha1.SandboxSetList{}
	if err := h.client.List(ctx, sbsList,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{fieldindex.IndexNameForSandboxSetTemplateRef: obj.GetName()},
	); err != nil {
		log.Error(err, "failed to list sandboxsets referencing sandbox template")
		return
	}
	if len(sbsList.Items) == 0 {
		return
	}
	log.V(1).Info("enqueueing sandboxsets for sandbox template change", "count", len(sbsList.Items))
	for i := range sbsList.Items {
		sbs := &sbsList.Items[i]
		w.Add(reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: sbs.Namespace,
			Name:      sbs.Name,
		}})
	}
}
