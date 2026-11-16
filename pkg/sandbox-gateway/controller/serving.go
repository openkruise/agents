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

package controller

import (
	"context"
	"fmt"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	agents "github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
	"github.com/openkruise/agents/pkg/sessionservice/kubernetes"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

// SessionService synchronization is independent: missing permissions or a missing CRD
// must not stop the existing Sandbox informer, route feed or wake controllers.
func addSessionServiceRuntime(ctx context.Context, cfg *rest.Config, mgr manager.Manager, provider infracache.Provider, opts ManagerOptions) error {
	if opts.SessionServiceService == nil {
		return nil
	}
	if _, err := mgr.GetRESTMapper().RESTMapping(agents.GroupVersion.WithKind("SessionService").GroupKind(), agents.GroupVersion.Version); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("discover SessionService: %w", err)
	}
	sandboxOptions, err := servingObjectOptions(opts.Namespace, opts.LabelSelector)
	if err != nil {
		return err
	}
	watchHealth := infracache.NewInformerHealth()
	servingCache, err := ctrlcache.New(cfg, ctrlcache.Options{
		Scheme:                      mgr.GetScheme(),
		Mapper:                      mgr.GetRESTMapper(),
		ReaderFailOnMissingInformer: true,
		DefaultWatchErrorHandler:    watchHealth.RecordWatchError,
		ByObject:                    map[client.Object]ctrlcache.ByObject{&agents.SessionService{}: sandboxOptions},
	})
	if err != nil {
		return fmt.Errorf("create SessionService cache: %w", err)
	}
	if err := servingCache.IndexField(ctx, &agents.SessionService{}, infracache.IndexSessionServiceID, func(obj client.Object) []string {
		id := obj.GetLabels()[sessionbinding.SessionServiceIDLabel]
		if id == "" {
			return nil
		}
		return []string{id}
	}); err != nil {
		return err
	}
	informer, err := servingCache.GetInformer(ctx, &agents.SessionService{})
	if err != nil {
		return err
	}
	var active atomic.Bool
	cli := servingClient{Client: provider.GetClient(), serving: servingCache}
	store := kubernetes.NewStore(cli, func() bool {
		return active.Load() && ctx.Err() == nil && provider.SandboxInformerHealthy() && informer.HasSynced() && watchHealth.Healthy()
	}, kubernetes.Options{LabelSelector: opts.LabelSelector, RequireMatchingTemplateScope: true})
	if err := opts.SessionServiceService.Configure(store); err != nil {
		return err
	}
	return mgr.Add(manager.RunnableFunc(func(runCtx context.Context) error {
		child, cancel := context.WithCancel(runCtx)
		defer cancel()
		defer opts.SessionServiceService.Stop()
		defer active.Store(false)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if err := servingCache.Start(child); err != nil {
				log.FromContext(child).Error(err, "SessionService informer stopped")
			}
			cancel()
		}()
		if servingCache.WaitForCacheSync(child) {
			watchHealth.MarkSynced()
			active.Store(true)
			<-child.Done()
		}
		<-done
		return nil
	}))
}

// Only SessionService reads go to the isolated cache; Sandbox reads and all writes use
// the existing process client. No APIReader or second Sandbox informer is used.
type servingClient struct {
	client.Client
	serving client.Reader
}

func (c servingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*agents.SessionService); ok {
		return c.serving.Get(ctx, key, obj, opts...)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c servingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*agents.SessionServiceList); ok {
		return c.serving.List(ctx, list, opts...)
	}
	return c.Client.List(ctx, list, opts...)
}

// Observe complete Sandbox events, not route refreshes, for binding write ACKs.
func observeBindingObject(obj any) {
	if box, ok := obj.(*agents.Sandbox); ok {
		expectations.ResourceVersionExpectationObserve(box)
	}
}

func forgetBindingObject(obj any) {
	if tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	if box, ok := obj.(*agents.Sandbox); ok {
		expectations.ResourceVersionExpectationDelete(box)
	}
}

func servingObjectOptions(namespace, selector string) (ctrlcache.ByObject, error) {
	options, err := buildGatewayCacheOptions(namespace, selector)
	if err != nil {
		return ctrlcache.ByObject{}, err
	}
	for obj, config := range options.ByObject {
		if _, ok := obj.(*agents.Sandbox); ok {
			config.Transform = nil
			return config, nil
		}
	}
	return ctrlcache.ByObject{}, fmt.Errorf("Sandbox cache scope is missing")
}
