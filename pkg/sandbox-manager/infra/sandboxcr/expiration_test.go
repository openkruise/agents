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
	"encoding/json"
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
	"github.com/openkruise/agents/pkg/utils/timeout"
)

func TestSandbox_UpdateShutdownTime(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tt := range []struct {
		name, mutation string
		shutdownOnly   bool
		want           managererrors.ErrorCode
	}{
		{name: "persist and preserve pause"},
		{name: "finite shutdown extension", shutdownOnly: true},
		{name: "finite shutdown shortening", shutdownOnly: true, mutation: "shorter"},
		{name: "finite shutdown same value is no-op", shutdownOnly: true, mutation: "equal"},
		{name: "finite shutdown subsecond same value is no-op", shutdownOnly: true, mutation: "precision"},
		{name: "never timeout still accepts finite deadline", shutdownOnly: true, mutation: "never"},
		{name: "concurrent pause deadline", mutation: "pause"},
		{name: "new concurrent pause rejects shortening", shutdownOnly: true, mutation: "new pause shorter", want: managererrors.ErrorBadRequest},
		{name: "new concurrent pause allows strict extension", shutdownOnly: true, mutation: "new pause"},
		{name: "concurrent longer renewal wins", mutation: "longer", want: managererrors.ErrorBadRequest},
		{name: "pause deadline rejects equal shutdown", mutation: "equal", want: managererrors.ErrorBadRequest},
		{name: "pause deadline rejects shorter shutdown", mutation: "shorter", want: managererrors.ErrorBadRequest},
		{name: "past deadline", mutation: "past", want: managererrors.ErrorBadRequest},
		{name: "terminating", mutation: "terminating", want: managererrors.ErrorConflict},
		{name: "subsecond extension is rejected", mutation: "precision", want: managererrors.ErrorBadRequest},
		{name: "patch failure", mutation: "fail", want: managererrors.ErrorUnknown},
		{name: "conflicts exhausted", mutation: "conflict", want: managererrors.ErrorConflict},
		{name: "canceled", mutation: "cancel", want: managererrors.ErrorUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			initial := submissionTestSandbox(false)
			initial.Spec.ShutdownTime = &metav1.Time{Time: now.Add(time.Hour)}
			initial.Spec.PauseTime = &metav1.Time{Time: now.Add(30 * time.Minute)}
			if tt.shutdownOnly {
				initial.Spec.PauseTime = nil
			}
			if tt.mutation == "never" {
				initial.Spec.ShutdownTime = nil
			}
			if tt.mutation == "terminating" {
				initial.Finalizers = []string{"keep-for-test"}
				initial.DeletionTimestamp = &metav1.Time{Time: now}
			}
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
			if tt.mutation == "shorter" || tt.mutation == "new pause shorter" {
				deadline = now.Add(45 * time.Minute)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			writeErr := fmt.Errorf("write rejected")
			intercepted := interceptor.NewClient(fc.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
					calls++
					data, err := p.Data(obj)
					require.NoError(t, err)
					var patch map[string]map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(data, &patch))
					assert.Len(t, patch, 2)
					assert.Len(t, patch["spec"], 1)
					assert.Contains(t, patch["spec"], "shutdownTime")
					assert.Len(t, patch["metadata"], 1)
					assert.Contains(t, patch["metadata"], "resourceVersion")
					switch tt.mutation {
					case "fail":
						return writeErr
					case "conflict":
						return apierrors.NewConflict(schema.GroupResource{Resource: "sandboxes"}, initial.Name, writeErr)
					case "cancel":
						cancel()
						return context.Canceled
					}
					if calls == 1 && (tt.mutation == "pause" || tt.mutation == "new pause" || tt.mutation == "new pause shorter" || tt.mutation == "longer") {
						var winner agentsv1alpha1.Sandbox
						if err := c.Get(ctx, client.ObjectKeyFromObject(initial), &winner); err != nil {
							return err
						}
						if tt.mutation != "longer" {
							winner.Spec.PauseTime = &metav1.Time{Time: now.Add(45 * time.Minute)}
							winner.Spec.Paused = true
							winner.Annotations = map[string]string{"concurrent": "keep"}
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
			err = sbx.UpdateShutdownTime(ctx, deadline)
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
			want := timeout.GetTimeoutFromSandbox(initial)
			if tt.want == "" {
				want.ShutdownTime = timeout.NormalizeTime(deadline)
			}
			if tt.mutation == "longer" {
				want.ShutdownTime = now.Add(3 * time.Hour)
			}
			if tt.mutation == "pause" || tt.mutation == "new pause" || tt.mutation == "new pause shorter" {
				want.PauseTime = now.Add(45 * time.Minute)
				assert.True(t, persisted.Spec.Paused)
				assert.Equal(t, "keep", persisted.Annotations["concurrent"])
				if tt.want == "" {
					assert.GreaterOrEqual(t, calls, 2)
				} else {
					assert.Equal(t, 1, calls)
				}
			}
			if (tt.shutdownOnly && (tt.mutation == "equal" || tt.mutation == "precision")) || tt.mutation == "past" || tt.mutation == "terminating" {
				assert.Zero(t, calls)
			}
			assert.True(t, timeout.Equal(want, timeout.GetTimeoutFromSandbox(&persisted)))
		})
	}
}
