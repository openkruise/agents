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
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/openkruise/agents/pkg/cache/controllers"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

type checkpointUpdateFuncTestCase struct {
	name               string
	cacheObjects       []ctrlclient.Object
	apiObjects         []ctrlclient.Object
	expectCheckpointID string
	expectError        string
}

func TestCheckpointUpdateFunc_FallsBackToAPIReader(t *testing.T) {
	tests := []checkpointUpdateFuncTestCase{
		{
			name: "cache hit returns cached checkpoint",
			cacheObjects: []ctrlclient.Object{
				newCheckpointUpdateFuncTestCheckpoint("cp-cache-hit", "cache-id"),
			},
			apiObjects: []ctrlclient.Object{
				newCheckpointUpdateFuncTestCheckpoint("cp-cache-hit", "api-id"),
			},
			expectCheckpointID: "cache-id",
		},
		{
			name: "cache miss falls back to api reader",
			apiObjects: []ctrlclient.Object{
				newCheckpointUpdateFuncTestCheckpoint("cp-api-hit", "api-id"),
			},
			expectCheckpointID: "api-id",
		},
		{
			name:        "cache and api reader miss returns not found",
			expectError: "not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cacheClient := newCheckpointUpdateFuncTestClient(t, tt.cacheObjects...)
			apiReader := newCheckpointUpdateFuncTestClient(t, tt.apiObjects...)
			c := &Cache{client: cacheClient, reader: apiReader}

			cp := &agentsv1alpha1.Checkpoint{
				ObjectMeta: metav1.ObjectMeta{Name: checkpointUpdateFuncTestCheckpointName(tt), Namespace: "default"},
			}
			got, err := c.CheckpointUpdateFunc(context.Background(), cp)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.expectCheckpointID, got.Status.CheckpointId)
		})
	}
}

func checkpointUpdateFuncTestCheckpointName(tt checkpointUpdateFuncTestCase) string {
	if len(tt.cacheObjects) > 0 {
		return tt.cacheObjects[0].GetName()
	}
	if len(tt.apiObjects) > 0 {
		return tt.apiObjects[0].GetName()
	}
	return "cp-missing"
}

func newCheckpointUpdateFuncTestCheckpoint(name, checkpointID string) *agentsv1alpha1.Checkpoint {
	return &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status:     agentsv1alpha1.CheckpointStatus{CheckpointId: checkpointID},
	}
}

func newCheckpointUpdateFuncTestClient(t *testing.T, objects ...ctrlclient.Object) ctrlclient.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

// The waiter blocks in the APIReader fallback while the reconciler observes
// the same object through its own informer-backed client.
func TestSandboxPauseTask_BlockedRead(t *testing.T) {
	for _, outcome := range []string{"completion", "timeout", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "ns"}, Spec: agentsv1alpha1.SandboxSpec{Paused: true}, Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxPaused}}
				builder, err := controllers.NewMockManagerBuilder(t)
				require.NoError(t, err)
				source := fake.NewClientBuilder().WithScheme(builder.Build().GetScheme()).WithStatusSubresource(sbx).WithObjects(sbx).Build()
				hooks := &sync.Map{}
				mgr := builder.WithClient(source).WithWaitSimulation(sbx).Build()
				mgr.SetWaitHooks(hooks)
				require.NoError(t, mgr.Start(ctx))
				blocked := make(chan struct{})
				api := interceptor.NewClient(source, interceptor.Funcs{Get: func(readCtx context.Context, _ ctrlclient.WithWatch, _ ctrlclient.ObjectKey, _ ctrlclient.Object, _ ...ctrlclient.GetOption) error {
					close(blocked)
					<-readCtx.Done()
					if outcome == "completion" {
						cancel()
					} // The event has already committed its result.
					return readCtx.Err()
				}})
				c := &Cache{client: newCheckpointUpdateFuncTestClient(t), reader: api, waitHooks: hooks}
				task, err := c.NewSandboxPauseTask(ctx, sbx)
				require.NoError(t, err)
				finished := make(chan error, 1)
				go func() {
					got, err := task.WaitForObject(sbx, 6*time.Second)
					if err == nil {
						assert.Equal(t, metav1.ConditionTrue, got.Status.Conditions[0].Status)
					}
					finished <- err
				}()
				<-blocked
				time.Sleep(4 * time.Second)
				synctest.Wait()
				select {
				case err := <-finished:
					t.Fatalf("unfinished Pause returned early: %v", err)
				default:
				}
				switch outcome {
				case "completion":
					current := sbx.DeepCopy()
					require.NoError(t, source.Get(ctx, ctrlclient.ObjectKeyFromObject(sbx), current))
					current.Status.Conditions = []metav1.Condition{{Type: string(agentsv1alpha1.SandboxConditionPaused), Status: metav1.ConditionTrue}}
					require.NoError(t, source.Status().Update(ctx, current))
					require.NoError(t, <-finished)
				case "timeout":
					err := <-finished
					require.ErrorIs(t, err, cacheutils.ErrWaitNotSatisfied)
					require.ErrorContains(t, err, "6s")
				case "cancel":
					cancel()
					require.ErrorIs(t, <-finished, context.Canceled)
				}
				_, exists := hooks.Load(cacheutils.WaitHookKey(sbx))
				require.False(t, exists)
			})
		})
	}
}
