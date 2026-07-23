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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandboxid"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/utils"
)

type managerProjectionSource struct {
	infra.Sandbox
}

var _ sandboxroute.ProjectionSource = (*managerProjectionSource)(nil)

func newManagerProjectionSource(sandbox infra.Sandbox) *managerProjectionSource {
	if sandbox == nil {
		return nil
	}
	return &managerProjectionSource{Sandbox: sandbox}
}

func (s *managerProjectionSource) GetID() string {
	return sandboxid.Resolve(s.Sandbox)
}

func (s *managerProjectionSource) GetAccessToken() string {
	return s.GetAnnotations()[agentsv1alpha1.AnnotationRuntimeAccessToken]
}

func (s *managerProjectionSource) RequiresTrafficAuth() bool {
	return s.GetAnnotations()[identity.AnnotationEnableJwtAuth] == agentsv1alpha1.True
}

func (m *SandboxManager) handleRouteSandboxEvent(ctx context.Context, event infra.RouteSandboxEvent) {
	if event.Delete != nil {
		result := m.proxy.Delete(*event.Delete)
		m.logRouteMutation(ctx, "delete", event.Delete.ObjectKey, result)
		return
	}
	if event.Sandbox == nil {
		klog.FromContext(ctx).Error(nil, "discarding empty manager route event")
		return
	}

	key := types.NamespacedName{
		Namespace: event.Sandbox.GetNamespace(),
		Name:      event.Sandbox.GetName(),
	}
	deletion := sandboxroute.Delete{
		ObjectKey:       key,
		ResourceVersion: event.Sandbox.GetResourceVersion(),
	}
	if event.Sandbox.GetDeletionTimestamp() != nil {
		result := m.proxy.Delete(deletion)
		m.logRouteMutation(ctx, "delete", key, result)
		return
	}
	if !m.routeIncludes(event.Sandbox) {
		result := m.proxy.DeleteIfTracked(deletion)
		m.logRouteMutation(ctx, "delete_if_tracked", key, result)
		return
	}

	route, err := m.projectInfraSandbox(event.Sandbox)
	if err != nil {
		klog.FromContext(ctx).Error(err, "failed to project manager route", "namespace", key.Namespace, "name", key.Name)
		return
	}
	result := m.proxy.SetRoute(ctx, route)
	m.logRouteMutation(ctx, "upsert", key, result)
}

func (m *SandboxManager) routeIncludes(sandbox metav1.Object) bool {
	if sandbox == nil {
		return false
	}
	if m.routeNamespace != "" && sandbox.GetNamespace() != m.routeNamespace {
		return false
	}
	selector := m.routeSelector
	if selector == nil {
		selector = labels.Everything()
	}
	return selector.Matches(labels.Set(sandbox.GetLabels()))
}

func (m *SandboxManager) projectInfraSandbox(sandbox infra.Sandbox) (sandboxroute.Route, error) {
	return sandboxroute.ProjectRoute(newManagerProjectionSource(sandbox))
}

func (m *SandboxManager) logRouteMutation(ctx context.Context, operation string, key types.NamespacedName, result sandboxroute.MutationResult) {
	klog.FromContext(ctx).V(utils.DebugLogLevel).Info(
		"manager route mutation completed",
		"operation", operation,
		"reason", result.Reason,
		"result", result.Result,
		"namespace", key.Namespace,
		"name", key.Name,
	)
}
