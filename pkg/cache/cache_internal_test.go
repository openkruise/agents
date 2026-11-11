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

package cache

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newSandboxSetLookupTestCache(t *testing.T, list interceptor.Funcs) (*Cache, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	store := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&agentsv1alpha1.SandboxSet{}, IndexTemplateID, func(obj client.Object) []string {
			return []string{obj.GetName()}
		}).Build()
	apiCalls := 0
	apiReader := interceptor.NewClient(store, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			apiCalls++
			return errors.New("unexpected APIReader Get")
		},
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			apiCalls++
			return errors.New("unexpected APIReader List")
		},
	})
	t.Cleanup(func() { require.Zero(t, apiCalls, "lookup must only read the informer client") })
	return &Cache{client: interceptor.NewClient(store, list), reader: apiReader}, store
}

func TestPickSandboxSetWait(t *testing.T) {
	readFailure := errors.New("informer read failed")
	for _, tt := range []struct {
		name      string
		visibleAt time.Duration
		namespace string
		deadline  time.Duration
		readErr   error
		errorAt   time.Duration
		wantErr   error
		wantMiss  bool
		duration  time.Duration
		finish    func(context.CancelFunc) error
	}{
		{name: "already visible", duration: 0},
		{name: "delayed visibility", visibleAt: 1050 * time.Millisecond, duration: 1100 * time.Millisecond},
		{name: "last part of window", visibleAt: 4950 * time.Millisecond, duration: 5 * time.Second, finish: func(context.CancelFunc) error { return nil }},
		{name: "already expired", deadline: -1, wantErr: context.DeadlineExceeded},
		{name: "final read error", visibleAt: -1, wantErr: readFailure, duration: 5 * time.Second, finish: func(context.CancelFunc) error { return readFailure }},
		{name: "cancel during final read", visibleAt: -1, wantErr: context.Canceled, duration: 5 * time.Second, finish: func(cancel context.CancelFunc) error { cancel(); return nil }},
		{name: "other namespace stays invisible", namespace: "team-b", wantMiss: true, duration: 5 * time.Second},
		{name: "parent deadline", visibleAt: -1, deadline: 350 * time.Millisecond, wantErr: context.DeadlineExceeded, duration: 350 * time.Millisecond},
		{name: "read error during wait", visibleAt: -1, errorAt: 250 * time.Millisecond, readErr: readFailure, wantErr: readFailure, duration: 300 * time.Millisecond},
		{name: "read deadline is not a cache miss", readErr: context.DeadlineExceeded, wantErr: context.DeadlineExceeded},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				start := time.Now()
				parentCtx, cancel := context.WithCancel(t.Context())
				defer cancel()
				sbs := &agentsv1alpha1.SandboxSet{ObjectMeta: metav1.ObjectMeta{Name: "template", Namespace: "team-a"}}
				if tt.namespace != "" {
					sbs.Namespace = tt.namespace
				}
				c, store := newSandboxSetLookupTestCache(t, interceptor.Funcs{
					List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						calls++
						// The final check uses the caller context rather than the poll context.
						if tt.finish != nil {
							if ctx != parentCtx {
								return inner.List(ctx, list, opts...)
							}
							if err := tt.finish(cancel); err != nil {
								return err
							}
						}
						if tt.deadline > 0 {
							<-ctx.Done()
							return ctx.Err()
						}
						if tt.readErr != nil && time.Since(start) >= tt.errorAt {
							return tt.readErr
						}
						if tt.visibleAt > 0 && time.Since(start) >= tt.visibleAt {
							require.NoError(t, inner.Create(ctx, sbs))
						}
						return inner.List(ctx, list, opts...)
					},
				})
				if tt.visibleAt == 0 {
					require.NoError(t, store.Create(t.Context(), sbs))
				}
				ctx := parentCtx
				if tt.deadline != 0 {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(ctx, tt.deadline)
					defer stop()
				}
				got, err := c.PickSandboxSet(ctx, PickSandboxSetOptions{Namespace: "team-a", Name: "template"})
				if tt.wantErr != nil {
					require.ErrorIs(t, err, tt.wantErr)
					require.Nil(t, got)
				} else if tt.wantMiss {
					require.EqualError(t, err, "sandboxset template not found in cache")
					require.Nil(t, got)
				} else {
					require.NoError(t, err)
					require.Equal(t, "team-a", got.Namespace)
					require.Equal(t, "template", got.Name)
				}
				require.Equal(t, tt.duration, time.Since(start))
				if tt.deadline < 0 {
					require.Zero(t, calls)
				} else if tt.duration == 0 {
					require.Equal(t, 1, calls)
				}
			})
		})
	}
}

func TestPickSandboxSetIndependentCalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, store := newSandboxSetLookupTestCache(t, interceptor.Funcs{})
		opts := PickSandboxSetOptions{Name: "template"}
		firstCtx, cancel := context.WithCancel(t.Context())
		defer cancel()
		first := make(chan error, 1)
		second := make(chan error, 1)
		go func() { _, err := c.PickSandboxSet(firstCtx, opts); first <- err }()
		synctest.Wait()
		time.Sleep(2 * time.Second)
		var secondGot *agentsv1alpha1.SandboxSet
		go func() {
			var err error
			secondGot, err = c.PickSandboxSet(t.Context(), opts)
			second <- err
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		require.ErrorIs(t, <-first, context.Canceled)
		select {
		case err := <-second:
			t.Fatalf("second caller stopped with the first caller: %v", err)
		default:
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		select {
		case err := <-second:
			t.Fatalf("second caller inherited the first caller's window: %v", err)
		default:
		}
		time.Sleep(time.Second)
		require.NoError(t, store.Create(t.Context(), &agentsv1alpha1.SandboxSet{
			ObjectMeta: metav1.ObjectMeta{Name: "template", Namespace: "team-b"},
		}))
		require.NoError(t, <-second)
		require.NotNil(t, secondGot)
		require.Equal(t, "team-b", secondGot.Namespace)
	})
}
