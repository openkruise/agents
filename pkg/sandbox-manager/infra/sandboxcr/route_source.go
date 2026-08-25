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

package sandboxcr

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandboxroute"
)

type sandboxRouteSource struct {
	cache cache.Provider
}

var _ infra.SandboxRouteSource = (*sandboxRouteSource)(nil)

func (i *Infra) GetSandboxRouteSource() infra.SandboxRouteSource {
	return &sandboxRouteSource{cache: i.Cache}
}

func (s *sandboxRouteSource) Subscribe(
	ctx context.Context,
	handler infra.SandboxRouteEventHandler,
) error {
	if handler == nil {
		return fmt.Errorf("sandbox route event handler must not be nil")
	}
	if s == nil || s.cache == nil {
		return fmt.Errorf("sandbox route cache is not configured")
	}

	_, err := s.cache.AddSandboxEventHandler(ctx, toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			s.handleObjectEvent(ctx, handler, obj)
		},
		UpdateFunc: func(_, newObj any) {
			s.handleObjectEvent(ctx, handler, newObj)
		},
		DeleteFunc: func(obj any) {
			s.handleDeleteEvent(ctx, handler, obj)
		},
	})
	if err != nil {
		return fmt.Errorf("register sandbox route informer handler: %w", err)
	}
	return nil
}

func (s *sandboxRouteSource) handleObjectEvent(
	ctx context.Context,
	handler infra.SandboxRouteEventHandler,
	obj any,
) {
	sandbox, ok := obj.(*agentsv1alpha1.Sandbox)
	if !ok {
		klog.FromContext(ctx).Error(
			nil,
			"discarding unexpected sandbox route informer object",
			"type", fmt.Sprintf("%T", obj),
		)
		return
	}
	handler(ctx, infra.SandboxRouteEvent{Sandbox: AsSandbox(sandbox, s.cache)})
}

func (s *sandboxRouteSource) handleDeleteEvent(
	ctx context.Context,
	handler infra.SandboxRouteEventHandler,
	obj any,
) {
	switch value := obj.(type) {
	case *agentsv1alpha1.Sandbox:
		handler(ctx, infra.SandboxRouteEvent{Delete: &sandboxroute.Route{
			Namespace:       value.Namespace,
			Name:            value.Name,
			ResourceVersion: value.ResourceVersion,
		}})
	case toolscache.DeletedFinalStateUnknown:
		key, err := routeObjectKeyFromTombstone(value)
		if err != nil {
			klog.FromContext(ctx).Error(err, "discarding invalid sandbox route tombstone")
			return
		}
		handler(ctx, infra.SandboxRouteEvent{Delete: &sandboxroute.Route{
			Namespace: key.Namespace,
			Name:      key.Name,
		}})
	default:
		klog.FromContext(ctx).Error(
			nil,
			"discarding unexpected sandbox route delete object",
			"type", fmt.Sprintf("%T", obj),
		)
	}
}

func routeObjectKeyFromTombstone(tombstone toolscache.DeletedFinalStateUnknown) (types.NamespacedName, error) {
	namespace, name, err := toolscache.SplitMetaNamespaceKey(tombstone.Key)
	if err != nil {
		return types.NamespacedName{}, fmt.Errorf("parse tombstone key %q: %w", tombstone.Key, err)
	}
	if namespace == "" || name == "" {
		return types.NamespacedName{}, fmt.Errorf("tombstone key %q must contain namespace and name", tombstone.Key)
	}
	return types.NamespacedName{Namespace: namespace, Name: name}, nil
}
