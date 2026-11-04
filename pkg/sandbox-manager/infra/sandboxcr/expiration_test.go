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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
)

func TestSandbox_ExtendShutdownTime(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tt := range []struct {
		name, mutation string
		want           managererrors.ErrorCode
	}{
		{name: "persist and preserve pause"},
		{name: "concurrent pause deadline", mutation: "pause"},
		{name: "concurrent longer renewal wins", mutation: "longer", want: managererrors.ErrorBadRequest},
		{name: "non advancing", mutation: "equal", want: managererrors.ErrorBadRequest},
		{name: "past deadline", mutation: "past", want: managererrors.ErrorBadRequest},
		{name: "subsecond extension is rejected", mutation: "precision", want: managererrors.ErrorBadRequest},
		{name: "patch failure", mutation: "fail", want: managererrors.ErrorUnknown},
		{name: "conflicts exhausted", mutation: "conflict", want: managererrors.ErrorConflict},
		{name: "canceled", mutation: "cancel", want: managererrors.ErrorUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			initial := submissionTestSandbox(false)
			initial.Spec.ShutdownTime = &metav1.Time{Time: now.Add(time.Hour)}
			initial.Spec.PauseTime = &metav1.Time{Time: now.Add(30 * time.Minute)}
			cache, fc, err := cachetest.NewTestCache(t, initial)
			require.NoError(t, err)
			deadline := now.Add(2 * time.Hour)
			if tt.mutation == "equal" {
				deadline = initial.Spec.ShutdownTime.Time
			}
			if tt.mutation == "past" {
				deadline = now.Add(-time.Hour)
			}
			if tt.mutation == "precision" {
				deadline = initial.Spec.ShutdownTime.Time.Add(time.Millisecond)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			writeErr := fmt.Errorf("write rejected")
			intercepted := interceptor.NewClient(fc.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
					calls++
					switch tt.mutation {
					case "fail":
						return writeErr
					case "conflict":
						return apierrors.NewConflict(schema.GroupResource{Resource: "sandboxes"}, initial.Name, writeErr)
					case "cancel":
						cancel()
						return context.Canceled
					}
					if calls == 1 && (tt.mutation == "pause" || tt.mutation == "longer") {
						var winner agentsv1alpha1.Sandbox
						if err := c.Get(ctx, client.ObjectKeyFromObject(initial), &winner); err != nil {
							return err
						}
						if tt.mutation == "pause" {
							winner.Spec.PauseTime = &metav1.Time{Time: now.Add(45 * time.Minute)}
						} else {
							winner.Spec.ShutdownTime = &metav1.Time{Time: now.Add(3 * time.Hour)}
						}
						if err := c.Update(ctx, &winner); err != nil {
							return err
						}
					}
					return c.Patch(ctx, obj, p, opts...)
				},
			})
			reader := interceptor.NewClient(fc.(client.WithWatch), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				t.Error("unexpected APIReader read")
				return writeErr
			}})
			sbx := AsSandbox(initial, &apiReaderOverrideCache{Provider: cache, client: intercepted, apiReader: reader})
			err = sbx.ExtendShutdownTime(ctx, deadline)
			if tt.want == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Equal(t, tt.want, managererrors.GetErrCode(err))
			}
			if tt.mutation == "cancel" {
				assert.ErrorIs(t, err, context.Canceled)
			}
			if tt.mutation == "fail" {
				assert.ErrorIs(t, err, writeErr)
			}
			var persisted agentsv1alpha1.Sandbox
			require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(initial), &persisted))
			wantShutdown, wantPause := initial.Spec.ShutdownTime.Time, initial.Spec.PauseTime.Time
			if tt.want == "" {
				wantShutdown = deadline
			}
			if tt.mutation == "longer" {
				wantShutdown = now.Add(3 * time.Hour)
			}
			if tt.mutation == "pause" {
				wantPause = now.Add(45 * time.Minute)
				assert.GreaterOrEqual(t, calls, 2)
			}
			assert.True(t, wantShutdown.Equal(persisted.Spec.ShutdownTime.Time))
			assert.True(t, wantPause.Equal(persisted.Spec.PauseTime.Time))
		})
	}
}
