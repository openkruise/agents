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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/sandboxid"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/utils"
)

func newManagerRouteProjector() *sandboxroute.Projector {
	return sandboxroute.NewProjector(func(object metav1.Object) string {
		return sandboxid.Resolve(object)
	})
}

func (m *SandboxManager) registerRouteFeeder() {
	managerCache, ok := m.infra.GetCache().(*infracache.Cache)
	if !ok || managerCache.GetSandboxController() == nil {
		return
	}
	managerCache.GetSandboxController().AddReconcileHandlers(m.reconcileSandboxRoute)
}

func (m *SandboxManager) reconcileSandboxRoute(ctx context.Context, sandbox *agentsv1alpha1.Sandbox, notFound bool) (ctrl.Result, error) {
	key := client.ObjectKeyFromObject(sandbox)
	if notFound || !m.routeIncludes(sandbox) {
		result := m.proxy.DeleteAuthoritativeByObjectKey(key, sandboxid.Legacy(sandbox.GetNamespace(), sandbox.GetName()))
		m.logRouteMutation(ctx, "delete", key, result)
		return ctrl.Result{}, nil
	}

	route, err := m.projectSandboxObject(sandbox)
	if err != nil {
		return ctrl.Result{}, err
	}
	result := m.proxy.SetRoute(ctx, route)
	m.logRouteMutation(ctx, "upsert", key, result)
	return ctrl.Result{}, nil
}

func (m *SandboxManager) observeRoute(reader client.Reader) sandboxroute.ObserveFunc {
	return func(ctx context.Context, key types.NamespacedName) (sandboxroute.AuthoritativeObservation, error) {
		sandbox := &agentsv1alpha1.Sandbox{}
		if err := reader.Get(ctx, key, sandbox); err != nil {
			if apierrors.IsNotFound(err) {
				return sandboxroute.AuthoritativeObservation{}, nil
			}
			return sandboxroute.AuthoritativeObservation{}, sandboxroute.NewGetObservationError(err)
		}
		if !m.routeIncludes(sandbox) {
			return sandboxroute.AuthoritativeObservation{}, nil
		}
		route, err := m.projectSandboxObject(sandbox)
		if err != nil {
			return sandboxroute.AuthoritativeObservation{}, sandboxroute.NewProjectionObservationError(err)
		}
		return sandboxroute.AuthoritativeObservation{Present: true, Route: route}, nil
	}
}

func (m *SandboxManager) routeIncludes(sandbox *agentsv1alpha1.Sandbox) bool {
	if sandbox == nil || sandbox.GetDeletionTimestamp() != nil {
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

func (m *SandboxManager) projectSandboxObject(sandbox *agentsv1alpha1.Sandbox) (sandboxroute.Route, error) {
	if sandbox == nil {
		return sandboxroute.Route{}, fmt.Errorf("project manager route: sandbox is nil")
	}
	state, _ := utils.GetSandboxState(sandbox)
	if sandbox.Status.PodInfo.PodIP == "" {
		state = agentsv1alpha1.SandboxStateCreating
	}
	return m.projectRoute(sandbox, sandbox.Status.PodInfo.PodIP, state)
}

func (m *SandboxManager) projectInfraSandbox(sandbox infra.Sandbox) (sandboxroute.Route, error) {
	if sandbox == nil {
		return sandboxroute.Route{}, fmt.Errorf("project manager route: sandbox is nil")
	}
	state, _ := sandbox.GetState()
	if sandbox.GetPodIP() == "" {
		state = agentsv1alpha1.SandboxStateCreating
	}
	return m.projectRoute(sandbox, sandbox.GetPodIP(), state)
}

func (m *SandboxManager) projectRoute(object metav1.Object, ip, state string) (sandboxroute.Route, error) {
	if m.routeProjector == nil {
		return sandboxroute.Route{}, fmt.Errorf("project manager route: projector is not configured")
	}
	annotations := object.GetAnnotations()
	return m.routeProjector.Project(sandboxroute.ProjectionInput{
		Object:             object,
		IP:                 ip,
		State:              state,
		Owner:              annotations[agentsv1alpha1.AnnotationOwner],
		AccessToken:        annotations[agentsv1alpha1.AnnotationRuntimeAccessToken],
		RequireTrafficAuth: annotations[identity.AnnotationEnableJwtAuth] == agentsv1alpha1.True,
	})
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
