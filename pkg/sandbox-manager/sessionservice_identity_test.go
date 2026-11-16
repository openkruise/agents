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
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agents "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	session "github.com/openkruise/agents/pkg/sessionservice"
	binding "github.com/openkruise/agents/pkg/sessionservice/binding"
	"github.com/openkruise/agents/pkg/sessionservice/kubernetes"
)

func TestSessionServiceIdentityEvents(t *testing.T) {
	for _, mode := range []string{"bootstrap", "add", "update", "conflict", "committed timeout", "committed timeout with cache lag", "uncommitted timeout", "existing ID", "recreated UID"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			parent := &agents.SessionService{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "new", UID: "old-uid"}}
			if mode == "existing ID" {
				parent.Labels = map[string]string{binding.SessionServiceIDLabel: "abcde234abcde"}
			}
			_, writer, err := cachetest.NewTestSessionServiceCache(t, parent)
			require.NoError(t, err)
			var initial agents.SessionService
			require.NoError(t, writer.Get(ctx, client.ObjectKeyFromObject(parent), &initial))
			var staleReads atomic.Int32
			var patches, reads, generated, removed atomic.Int32
			var handler toolscache.ResourceEventHandler
			observed := interceptor.NewClient(writer.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if target, ok := obj.(*agents.SessionService); ok {
						reads.Add(1)
						if staleReads.Load() > 0 {
							staleReads.Add(-1)
							*target = *initial.DeepCopy()
							return nil
						}
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					n := patches.Add(1)
					if mode == "conflict" && n == 1 {
						return fmt.Errorf("%w: injected conflict", session.ErrSessionServiceConflict)
					}
					if mode == "uncommitted timeout" && n == 1 {
						return context.DeadlineExceeded
					}
					err := c.Patch(ctx, obj, patch, opts...)
					if (mode == "committed timeout" || mode == "committed timeout with cache lag") && n == 1 && err == nil {
						if mode == "committed timeout with cache lag" {
							staleReads.Store(2)
						}
						return context.DeadlineExceeded
					}
					return err
				},
			})
			backend := kubernetes.NewStore(observed, func() bool { return true }, kubernetes.Options{RegisterSessionServiceHandler: func(_ context.Context, h toolscache.ResourceEventHandler) (func() error, error) {
				handler = h
				if mode == "bootstrap" || mode == "recreated UID" {
					h.OnAdd(parent, true)
					if mode == "recreated UID" {
						require.NoError(t, writer.Delete(ctx, parent))
						parent = parent.DeepCopy()
						parent.UID = "new-uid"
						parent.ResourceVersion = ""
						require.NoError(t, writer.Create(ctx, parent))
						h.OnAdd(parent, false)
					}
				}
				return func() error { removed.Add(1); return nil }, nil
			}})
			m := &SandboxManager{servingBackend: backend, generateSessionServiceID: func() (string, error) {
				n := generated.Add(1)
				if n > 1 {
					return "abcde234abcdf", nil
				}
				return "abcde234abcde", nil
			}}
			require.NoError(t, m.startSessionServiceIdentityWorker(ctx, backend))
			if mode != "bootstrap" && mode != "recreated UID" {
				if mode == "update" {
					handler.OnUpdate(parent, parent)
				} else {
					handler.OnAdd(parent, false)
					handler.OnAdd(parent, false)
				}
			}
			require.Eventually(t, func() bool {
				var got agents.SessionService
				if writer.Get(ctx, client.ObjectKeyFromObject(parent), &got) != nil {
					return false
				}
				if got.Labels[binding.SessionServiceIDLabel] == "" {
					return false
				}
				if mode == "committed timeout with cache lag" && reads.Load() < 5 {
					return false
				}
				if mode == "committed timeout" && reads.Load() < 3 {
					return false
				}
				return true
			}, time.Second, 5*time.Millisecond, "identity must not wait for the 5s maintenance timer")
			cancel()
			select {
			case <-m.sessionServiceIdentityDone:
			case <-time.After(time.Second):
				t.Fatal("identity worker did not shut down")
			}
			require.EqualValues(t, 1, removed.Load())
			if mode == "existing ID" {
				require.Zero(t, patches.Load())
				require.Zero(t, generated.Load())
			} else if mode == "conflict" || mode == "uncommitted timeout" || mode == "committed timeout with cache lag" {
				require.EqualValues(t, 2, patches.Load())
			} else {
				require.EqualValues(t, 1, patches.Load())
			}
			var stored agents.SessionService
			require.NoError(t, writer.Get(t.Context(), client.ObjectKeyFromObject(parent), &stored))
			require.Equal(t, parent.UID, stored.UID)
			if mode == "committed timeout with cache lag" {
				require.Equal(t, "abcde234abcde", stored.Labels[binding.SessionServiceIDLabel])
				require.EqualValues(t, 2, generated.Load())
			}
		})
	}
}

func TestSessionServiceIdentityRegistrationFailureAndCancellation(t *testing.T) {
	_, writer, err := cachetest.NewTestSessionServiceCache(t)
	require.NoError(t, err)
	for _, mode := range []string{"registration failure", "already canceled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "already canceled" {
				cancel()
			}
			boom := errors.New("registration failed")
			backend := kubernetes.NewStore(writer, func() bool { return true }, kubernetes.Options{RegisterSessionServiceHandler: func(context.Context, toolscache.ResourceEventHandler) (func() error, error) { return nil, boom }})
			m := &SandboxManager{}
			err := m.startSessionServiceIdentityWorker(ctx, backend)
			if mode == "already canceled" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, boom)
			}
		})
	}
}

func TestSessionServiceIdentityEventManagersRace(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	parent := &agents.SessionService{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "race", UID: "race-uid", Labels: map[string]string{"keep": "yes"}}}
	_, writer, err := cachetest.NewTestSessionServiceCache(t, parent)
	require.NoError(t, err)
	var reads, patches, conflicts, removed atomic.Int32
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	observed := interceptor.NewClient(writer.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*agents.SessionService); ok {
				reads.Add(1)
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			patches.Add(1)
			select {
			case entered <- struct{}{}:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			err := c.Patch(ctx, obj, p, opts...)
			if apierrors.IsConflict(err) {
				conflicts.Add(1)
			}
			return err
		},
	})
	backend := kubernetes.NewStore(observed, func() bool { return true }, kubernetes.Options{RegisterSessionServiceHandler: func(_ context.Context, h toolscache.ResourceEventHandler) (func() error, error) {
		h.OnAdd(parent, true)
		return func() error { removed.Add(1); return nil }, nil
	}})
	first := &SandboxManager{generateSessionServiceID: func() (string, error) { return "abcde234abcde", nil }}
	second := &SandboxManager{generateSessionServiceID: func() (string, error) { return "abcde234abcdf", nil }}
	require.NoError(t, first.startSessionServiceIdentityWorker(ctx, backend))
	require.NoError(t, second.startSessionServiceIdentityWorker(ctx, backend))
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("both workers did not enter conditional write", ctx.Err())
		}
	}
	close(release)
	require.Eventually(t, func() bool { return conflicts.Load() == 1 && reads.Load() >= 5 }, time.Second, 5*time.Millisecond)
	cancel()
	for _, m := range []*SandboxManager{first, second} {
		select {
		case <-m.sessionServiceIdentityDone:
		case <-time.After(time.Second):
			t.Fatal("worker shutdown")
		}
	}
	require.EqualValues(t, 2, patches.Load())
	require.EqualValues(t, 2, removed.Load())
	var got agents.SessionService
	require.NoError(t, writer.Get(t.Context(), client.ObjectKeyFromObject(parent), &got))
	require.Contains(t, []string{"abcde234abcde", "abcde234abcdf"}, got.Labels[binding.SessionServiceIDLabel])
	require.Equal(t, "yes", got.Labels["keep"])
}

func TestSessionServiceIdentityInformerReplayScopeAndRemoval(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	parent := &agents.SessionService{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "bootstrap", UID: "bootstrap-uid", Labels: map[string]string{"scope": "yes"}}}
	outside := parent.DeepCopy()
	outside.Name = "excluded-label"
	outside.UID = "excluded-label"
	outside.Labels = map[string]string{"scope": "no"}
	otherNamespace := parent.DeepCopy()
	otherNamespace.Namespace = "other"
	otherNamespace.UID = "other-uid"
	_, writer, err := cachetest.NewTestSessionServiceCache(t, parent, outside, otherNamespace)
	require.NoError(t, err)
	watchReady := make(chan struct{})
	var watchStarted atomic.Bool
	informer := toolscache.NewSharedIndexInformer(&toolscache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, _ metav1.ListOptions) (runtime.Object, error) {
			list := &agents.SessionServiceList{}
			err := writer.List(ctx, list, client.InNamespace("team"), client.MatchingLabels{"scope": "yes"})
			return list, err
		},
		WatchFuncWithContext: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
			// The existing fake supports List+Watch, not streaming initial events.
			if opts.SendInitialEvents != nil && *opts.SendInitialEvents {
				return nil, apierrors.NewBadRequest("fake does not support WatchList")
			}
			w, err := writer.(client.WithWatch).Watch(ctx, &agents.SessionServiceList{}, client.InNamespace("team"))
			if err == nil && watchStarted.CompareAndSwap(false, true) {
				close(watchReady)
			}
			return w, err
		},
	}, &agents.SessionService{}, 0, toolscache.Indexers{})
	go informer.RunWithContext(ctx)
	require.True(t, toolscache.WaitForCacheSync(ctx.Done(), informer.HasSynced))
	select {
	case <-watchReady:
	case <-ctx.Done():
		t.Fatal("test informer watch did not start")
	}
	var removed atomic.Int32
	backend := kubernetes.NewStore(writer, informer.HasSynced, kubernetes.Options{RegisterSessionServiceHandler: func(_ context.Context, h toolscache.ResourceEventHandler) (func() error, error) {
		reg, err := informer.AddEventHandler(h)
		if err != nil {
			return nil, err
		}
		return func() error { removed.Add(1); return informer.RemoveEventHandler(reg) }, nil
	}})
	workerCtx, cancelWorker := context.WithCancel(ctx)
	defer cancelWorker()
	m := &SandboxManager{generateSessionServiceID: func() (string, error) { return "abcde234abcde", nil }}
	require.NoError(t, m.startSessionServiceIdentityWorker(workerCtx, backend))
	require.Eventually(t, func() bool {
		var got agents.SessionService
		return writer.Get(ctx, client.ObjectKeyFromObject(parent), &got) == nil && got.Labels[binding.SessionServiceIDLabel] != ""
	}, time.Second, 5*time.Millisecond)
	for _, excluded := range []*agents.SessionService{outside, otherNamespace} {
		var got agents.SessionService
		require.NoError(t, writer.Get(ctx, client.ObjectKeyFromObject(excluded), &got))
		require.Empty(t, got.Labels[binding.SessionServiceIDLabel])
	}
	added := parent.DeepCopy()
	added.Name = "after-start"
	added.UID = "after-start-uid"
	added.ResourceVersion = ""
	require.NoError(t, writer.Create(ctx, added))
	require.Eventually(t, func() bool {
		var got agents.SessionService
		return writer.Get(ctx, client.ObjectKeyFromObject(added), &got) == nil && got.Labels[binding.SessionServiceIDLabel] != ""
	}, time.Second, 5*time.Millisecond)
	cancelWorker()
	select {
	case <-m.sessionServiceIdentityDone:
	case <-ctx.Done():
		t.Fatal("worker did not remove registration")
	}
	require.EqualValues(t, 1, removed.Load())
	stopped := parent.DeepCopy()
	stopped.Name = "after-stop"
	stopped.UID = "after-stop-uid"
	stopped.ResourceVersion = ""
	require.NoError(t, writer.Create(ctx, stopped))
	require.Eventually(t, func() bool { _, exists, _ := informer.GetStore().GetByKey("team/after-stop"); return exists }, time.Second, 5*time.Millisecond)
	var unchanged agents.SessionService
	require.NoError(t, writer.Get(ctx, client.ObjectKeyFromObject(stopped), &unchanged))
	require.Empty(t, unchanged.Labels[binding.SessionServiceIDLabel])
}

func TestSessionServiceIdentityShutdownCancelsActiveWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	parent := &agents.SessionService{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "shutdown", UID: "shutdown-uid"}}
	_, writer, err := cachetest.NewTestSessionServiceCache(t, parent)
	require.NoError(t, err)
	entered := make(chan struct{})
	var removed atomic.Int32
	observed := interceptor.NewClient(writer.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}})
	backend := kubernetes.NewStore(observed, func() bool { return true }, kubernetes.Options{RegisterSessionServiceHandler: func(_ context.Context, h toolscache.ResourceEventHandler) (func() error, error) {
		h.OnAdd(parent, true)
		return func() error { removed.Add(1); return nil }, nil
	}})
	m := &SandboxManager{generateSessionServiceID: func() (string, error) { return "abcde234abcde", nil }}
	require.NoError(t, m.startSessionServiceIdentityWorker(ctx, backend))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not enter assignment")
	}
	cancel()
	select {
	case <-m.sessionServiceIdentityDone:
	case <-time.After(time.Second):
		t.Fatal("active write did not cancel")
	}
	require.EqualValues(t, 1, removed.Load())
	var unchanged agents.SessionService
	require.NoError(t, writer.Get(t.Context(), client.ObjectKeyFromObject(parent), &unchanged))
	require.Empty(t, unchanged.Labels[binding.SessionServiceIDLabel])
}
