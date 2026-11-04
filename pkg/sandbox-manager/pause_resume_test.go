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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/sandboxid"
)

// The existing test cache has no informer. This adapter supplies a real shared
// informer driven by the existing fake client's List/Watch implementation.
type lifecycleInformerCache struct {
	cache.Provider
	informer toolscache.SharedIndexInformer
}

func (*lifecycleInformerCache) IsWatchListSemanticsUnSupported() bool {
	// The fake client supports ordinary List/Watch, but not streaming initial events.
	return true
}

func (c *lifecycleInformerCache) AddSandboxEventHandler(_ context.Context, handler toolscache.ResourceEventHandler) (cache.SandboxEventHandlerRegistration, error) {
	registration, err := c.informer.AddEventHandler(handler)
	if err != nil {
		return nil, err
	}
	return &lifecycleInformerRegistration{ResourceEventHandlerRegistration: registration, informer: c.informer}, nil
}

type lifecycleInformerRegistration struct {
	toolscache.ResourceEventHandlerRegistration
	informer toolscache.SharedIndexInformer
}

func (r *lifecycleInformerRegistration) Remove() error {
	return r.informer.RemoveEventHandler(r.ResourceEventHandlerRegistration)
}

func TestSandboxManager_SubmissionRouteSubscription(t *testing.T) {
	for _, paused := range []bool{true, false} {
		t.Run(fmt.Sprintf("paused=%t", paused), func(t *testing.T) {
			initial := getSandboxForApiTest("async-route")
			initial.Spec.Paused = !paused
			condition := agentsv1alpha1.SandboxConditionReady
			if !paused {
				initial.Status.Phase = agentsv1alpha1.SandboxPaused
				condition = agentsv1alpha1.SandboxConditionPaused
			}
			initial.Status.Conditions = []metav1.Condition{{Type: string(condition), Status: metav1.ConditionTrue}}
			baseCache, fc, err := cachetest.NewTestCache(t)
			require.NoError(t, err)
			CreateSandboxWithStatus(t, fc, initial)
			provider := &lifecycleInformerCache{Provider: baseCache}
			listWatch := &toolscache.ListWatch{
				ListWithContextFunc: func(ctx context.Context, _ metav1.ListOptions) (runtime.Object, error) {
					list := &agentsv1alpha1.SandboxList{}
					return list, fc.List(ctx, list)
				},
				WatchFuncWithContext: func(ctx context.Context, _ metav1.ListOptions) (watch.Interface, error) {
					return fc.(client.WithWatch).Watch(ctx, &agentsv1alpha1.SandboxList{})
				},
			}
			informer := toolscache.NewSharedIndexInformer(toolscache.ToListWatcherWithWatchListSemantics(listWatch, provider), &agentsv1alpha1.Sandbox{}, 0, toolscache.Indexers{})
			provider.informer = informer
			opts := config.InitOptions(config.SandboxManagerOptions{DisableEnvoyExtProc: true})
			gateway := proxy.NewServer(opts)
			backend := sandboxcr.NewInfraBuilder(opts).WithCache(provider).WithRouteReader(gateway).Build()
			manager := &SandboxManager{infra: backend, proxy: gateway}
			sbx := sandboxcr.AsSandbox(initial, provider)
			requestCtx, disconnect := context.WithTimeout(t.Context(), time.Second)
			if paused {
				err = manager.SubmitSandboxPause(requestCtx, sbx.GetAnnotations()[agentsv1alpha1.AnnotationOwner], sbx, infra.PauseOptions{})
			} else {
				err = manager.SubmitSandboxResume(requestCtx, sbx.GetAnnotations()[agentsv1alpha1.AnnotationOwner], sbx, infra.ResumeOptions{})
			}
			disconnect()
			require.NoError(t, err)
			id := sandboxid.Resolve(initial)
			_, exists := gateway.LoadRoute(id)
			assert.False(t, exists, "submission must skip completion-phase route sync")
			var accepted agentsv1alpha1.Sandbox
			require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(initial), &accepted))
			assert.Equal(t, paused, accepted.Spec.Paused)
			assert.Equal(t, initial.Status.Phase, accepted.Status.Phase)

			// Subscribe after acceptance: the initial informer list recovers the
			// persisted intent independently of the disconnected request.
			ctx, stop := context.WithTimeout(t.Context(), 3*time.Second)
			defer stop()
			require.NoError(t, backend.GetSandboxRouteSource().Subscribe(ctx, manager.handleSandboxRouteEvent))
			go informer.Run(ctx.Done())
			require.True(t, toolscache.WaitForCacheSync(ctx.Done(), informer.HasSynced))
			require.Eventually(t, func() bool { _, ok := gateway.LoadRoute(id); return ok }, time.Second, time.Millisecond)
			completed := accepted.DeepCopy()
			completed.Status.PodInfo.PodIP = "10.0.0.9"
			if paused {
				completed.Status.Phase = agentsv1alpha1.SandboxPaused
				completed.Status.Conditions = []metav1.Condition{{Type: string(agentsv1alpha1.SandboxConditionPaused), Status: metav1.ConditionTrue}}
			} else {
				completed.Status.Phase = agentsv1alpha1.SandboxRunning
				completed.Status.Conditions = []metav1.Condition{{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}}
			}
			require.NoError(t, fc.Status().Patch(t.Context(), completed, client.MergeFrom(&accepted)))
			wantState := agentsv1alpha1.SandboxStateRunning
			if paused {
				wantState = agentsv1alpha1.SandboxStatePaused
			}
			require.Eventually(t, func() bool {
				route, ok := gateway.LoadRoute(id)
				return ok && route.ResourceVersion == completed.ResourceVersion && route.IP == "10.0.0.9" && route.State == wantState
			}, time.Second, time.Millisecond)
			// A fresh Manager recovers the same route through its subscription.
			restarted := &SandboxManager{infra: backend, proxy: proxy.NewServer(opts)}
			require.NoError(t, backend.GetSandboxRouteSource().Subscribe(ctx, restarted.handleSandboxRouteEvent))
			require.Eventually(t, func() bool {
				route, ok := restarted.proxy.LoadRoute(id)
				return ok && route.State == wantState && route.IP == "10.0.0.9"
			}, time.Second, time.Millisecond)
		})
	}
}

func TestSandboxManager_SubmissionOwnership(t *testing.T) {
	for _, user := range []string{"", "other-owner"} {
		for _, paused := range []bool{true, false} {
			t.Run(fmt.Sprintf("user=%q/paused=%t", user, paused), func(t *testing.T) {
				initial := getSandboxForApiTest("owned")
				initial.Annotations[agentsv1alpha1.AnnotationOwner] = "owner"
				// No cache is supplied: rejected calls must never reach backend submission.
				sbx := sandboxcr.AsSandbox(initial, nil)
				manager := &SandboxManager{}
				var err error
				if paused {
					err = manager.SubmitSandboxPause(t.Context(), user, sbx, infra.PauseOptions{})
				} else {
					err = manager.SubmitSandboxResume(t.Context(), user, sbx, infra.ResumeOptions{})
				}
				require.Error(t, err)
				assert.Equal(t, managererrors.ErrorNotAllowed, managererrors.GetErrCode(err))
			})
		}
	}
}
