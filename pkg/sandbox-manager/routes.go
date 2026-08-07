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

package sandbox_manager

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandboxroute"
)

func (m *SandboxManager) handleRouteSandboxEvent(ctx context.Context, event infra.RouteSandboxEvent) {
	log := klog.FromContext(ctx)
	if event.Delete != nil {
		result := m.proxy.Delete(*event.Delete)
		sandboxroute.LogMutation(log, "delete", *event.Delete, result)
		return
	}
	if event.Sandbox == nil {
		log.Error(nil, "discarding empty manager route event")
		return
	}

	key := types.NamespacedName{
		Namespace: event.Sandbox.GetNamespace(),
		Name:      event.Sandbox.GetName(),
	}
	deletion := sandboxroute.Route{
		Namespace:       key.Namespace,
		Name:            key.Name,
		ResourceVersion: event.Sandbox.GetResourceVersion(),
	}
	if event.Sandbox.GetDeletionTimestamp() != nil {
		result := m.proxy.Delete(deletion)
		sandboxroute.LogMutation(log, "delete", deletion, result)
		return
	}
	route, err := event.Sandbox.GetRoute()
	if err != nil {
		log.Error(err, "failed to project manager route", "namespace", key.Namespace, "name", key.Name)
		return
	}
	result := m.proxy.SetRoute(route)
	sandboxroute.LogMutation(log, "upsert", route, result)
}
