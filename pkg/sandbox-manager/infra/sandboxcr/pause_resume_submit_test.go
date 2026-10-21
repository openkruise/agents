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
	stderrors "errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

func submissionTestSandbox(paused bool) *v1alpha1.Sandbox {
	sbx := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "submit", Namespace: "default"},
		Spec: v1alpha1.SandboxSpec{Paused: paused, EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
			TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "keep-template"},
		}},
		Status: v1alpha1.SandboxStatus{Phase: v1alpha1.SandboxRunning},
	}
	condition := v1alpha1.SandboxConditionReady
	if paused {
		sbx.Status.Phase = v1alpha1.SandboxPaused
		condition = v1alpha1.SandboxConditionPaused
	}
	SetSandboxCondition(sbx, string(condition), metav1.ConditionTrue, "Test", "")
	return sbx
}

func submissionTestCall(ctx context.Context, sbx *Sandbox, paused bool, opts *timeout.Options, annotations map[string]string) error {
	if paused {
		return sbx.SubmitPause(ctx, infra.PauseOptions{Timeout: opts, ExtraAnnotations: annotations})
	}
	return sbx.SubmitResume(ctx, infra.ResumeOptions{Timeout: opts})
}

func TestSandbox_SubmissionAcceptance(t *testing.T) {
	for _, paused := range []bool{true, false} {
		t.Run(fmt.Sprintf("paused=%t", paused), func(t *testing.T) {
			initial := submissionTestSandbox(!paused)
			cache, fc, err := cachetest.NewTestCache(t, initial)
			require.NoError(t, err)
			require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(initial), initial))
			var reads atomic.Int32
			reader := interceptor.NewClient(fc.(client.WithWatch), interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					reads.Add(1)
					return fmt.Errorf("unexpected APIReader read")
				},
			})
			provider := &apiReaderOverrideCache{Provider: cache, apiReader: reader}
			first := timeout.Options{PauseTime: time.Now().Add(time.Hour), ShutdownTime: time.Now().Add(2 * time.Hour)}
			loser := timeout.Options{PauseTime: time.Now().Add(3 * time.Hour), ShutdownTime: time.Now().Add(4 * time.Hour)}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			sbx := AsSandbox(initial, provider)
			require.NoError(t, submissionTestCall(ctx, sbx, paused, &first, map[string]string{"winner": "first"}))
			cancel()
			// Neither a disconnected caller nor a new wrapper owns completion.
			restarted := AsSandbox(initial.DeepCopy(), provider)
			require.NoError(t, submissionTestCall(t.Context(), restarted, paused, &loser, map[string]string{"winner": "second"}))
			var persisted v1alpha1.Sandbox
			require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(initial), &persisted))
			assert.Equal(t, paused, persisted.Spec.Paused)
			assert.Equal(t, initial.Status, persisted.Status, "controller has not completed the operation")
			assertTimePtrEqual(t, timePtrOrNil(first.PauseTime), persisted.Spec.PauseTime, "first pause deadline")
			assertTimePtrEqual(t, timePtrOrNil(first.ShutdownTime), persisted.Spec.ShutdownTime, "first shutdown deadline")
			assert.Equal(t, initial.Spec.EmbeddedSandboxTemplate, persisted.Spec.EmbeddedSandboxTemplate)
			if paused {
				assert.Equal(t, "first", persisted.Annotations["winner"])
			}
			assert.Zero(t, reads.Load())
			cache.GetWaitHooks().Range(func(key, value any) bool {
				t.Errorf("submission leaked wait hook %v", key)
				return true
			})
			// A different Manager has no process-local hook, so persisted intent
			// must protect against reversing a still-pending operation.
			otherCache, _, err := cachetest.NewTestCache(t, &persisted)
			require.NoError(t, err)
			err = submissionTestCall(t.Context(), AsSandbox(&persisted, otherCache), !paused, nil, nil)
			assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err))
		})
	}
}

func TestSandbox_SubmissionGuardsAndFailures(t *testing.T) {
	writeErr := stderrors.New("write unavailable")
	tests := []struct {
		name    string
		mutate  func(*v1alpha1.Sandbox, bool)
		patch   string
		missing bool
		cancel  bool
		want    managererrors.ErrorCode
	}{
		{name: "opposite waiter", patch: "waiter", want: managererrors.ErrorUnknown},
		{name: "deleting", mutate: func(s *v1alpha1.Sandbox, _ bool) {
			now := metav1.Now()
			s.DeletionTimestamp = &now
			s.Finalizers = []string{"keep"}
		}, want: managererrors.ErrorConflict},
		{name: "expired", mutate: func(s *v1alpha1.Sandbox, _ bool) { s.Spec.ShutdownTime = timePtrOrNil(time.Now().Add(-time.Hour)) }, want: managererrors.ErrorConflict},
		{name: "pending", mutate: func(s *v1alpha1.Sandbox, _ bool) { s.Status.Phase = v1alpha1.SandboxPending }, want: managererrors.ErrorConflict},
		{name: "opposite transition", mutate: func(s *v1alpha1.Sandbox, paused bool) {
			if paused {
				s.Spec.Paused = false
			} else {
				SetSandboxCondition(s, string(v1alpha1.SandboxConditionPaused), metav1.ConditionFalse, "Test", "")
			}
		}, want: managererrors.ErrorConflict},
		{name: "write failed", patch: "fail", want: managererrors.ErrorUnknown},
		{name: "conflicts exhausted", patch: "conflict", want: managererrors.ErrorConflict},
		{name: "canceled", cancel: true, want: managererrors.ErrorUnknown},
		{name: "canceled during patch", patch: "cancel", want: managererrors.ErrorUnknown},
		{name: "missing", missing: true, want: managererrors.ErrorUnknown},
	}
	for _, paused := range []bool{true, false} {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("paused=%t/%s", paused, tt.name), func(t *testing.T) {
				initial := submissionTestSandbox(!paused)
				if tt.patch == "waiter" {
					// Use native admissible states so only the opposite hook rejects.
					initial = submissionTestSandbox(false)
				}
				if tt.name == "opposite transition" && paused {
					initial = submissionTestSandbox(true)
				}
				if tt.mutate != nil {
					tt.mutate(initial, paused)
				}
				cache, fc, err := cachetest.NewTestCache(t, initial)
				require.NoError(t, err)
				require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(initial), initial))
				if tt.missing {
					require.NoError(t, fc.Delete(t.Context(), initial))
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tt.cancel {
					cancel()
				}
				if tt.patch == "waiter" {
					if paused {
						task, err := cache.NewSandboxResumeTask(ctx, initial)
						require.NoError(t, err)
						defer task.Release()
					} else {
						task, err := cache.NewSandboxPauseTask(ctx, initial)
						require.NoError(t, err)
						defer task.Release()
					}
				}
				intercepted := interceptor.NewClient(fc.(client.WithWatch), interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
						switch tt.patch {
						case "fail":
							return writeErr
						case "conflict":
							return apierrors.NewConflict(schema.GroupResource{Resource: "sandboxes"}, initial.Name, writeErr)
						case "cancel":
							cancel()
							return context.Canceled
						default:
							return c.Patch(ctx, obj, p, opts...)
						}
					},
				})
				sbx := AsSandbox(initial, &apiReaderOverrideCache{Provider: cache, client: intercepted})
				err = submissionTestCall(ctx, sbx, paused, nil, nil)
				require.Error(t, err)
				assert.Equal(t, tt.want, managererrors.GetErrCode(err))
				switch {
				case tt.cancel || tt.patch == "cancel":
					assert.ErrorIs(t, err, context.Canceled)
				case tt.patch == "fail":
					assert.ErrorIs(t, err, writeErr)
				case tt.patch == "waiter":
					assert.ErrorIs(t, err, cacheutils.ErrWaitTaskConflict)
				case tt.missing:
					assert.True(t, apierrors.IsNotFound(err))
				}
				if !tt.missing {
					var actual v1alpha1.Sandbox
					require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(initial), &actual))
					assert.Equal(t, initial.Spec, actual.Spec, "failed submissions must not mutate intent")
				}
			})
		}
	}
}

func TestSandbox_SubmissionStaleObservation(t *testing.T) {
	for _, paused := range []bool{true, false} {
		for _, observation := range []string{"stale rejection", "stale no-op", "opposite won"} {
			t.Run(fmt.Sprintf("paused=%t/%s", paused, observation), func(t *testing.T) {
				initial := submissionTestSandbox(!paused)
				cache, fc, err := cachetest.NewTestCache(t, initial)
				require.NoError(t, err)
				var current v1alpha1.Sandbox
				require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(initial), &current))
				stale := current.DeepCopy()
				if observation == "stale rejection" {
					stale.Status.Phase = v1alpha1.SandboxPending
				} else if observation == "stale no-op" {
					// The cache incorrectly says the requested intent already exists.
					stale.Spec.Paused = paused
					stale.Status.Phase = v1alpha1.SandboxRunning
				}
				updated := current.DeepCopy()
				updated.Annotations = map[string]string{"external": "keep"}
				require.NoError(t, fc.Patch(t.Context(), updated, client.MergeFrom(&current)))
				if observation == "opposite won" {
					previous := updated.DeepCopy()
					condition := v1alpha1.SandboxConditionReady
					updated.Status.Phase = v1alpha1.SandboxRunning
					if paused {
						updated.Status.Phase = v1alpha1.SandboxPaused
						condition = v1alpha1.SandboxConditionPaused
					}
					updated.Status.Conditions = []metav1.Condition{{Type: string(condition), Status: metav1.ConditionTrue}}
					require.NoError(t, fc.Status().Patch(t.Context(), updated, client.MergeFrom(previous)))
				}
				// Production informer transformations may omit large template fields.
				stale.Spec.EmbeddedSandboxTemplate = v1alpha1.EmbeddedSandboxTemplate{}
				var reads, patches atomic.Int32
				intercepted := interceptor.NewClient(fc.(client.WithWatch), interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if reads.Add(1) == 1 {
							stale.DeepCopyInto(obj.(*v1alpha1.Sandbox))
							return nil
						}
						if err := c.Get(ctx, key, obj, opts...); err != nil {
							return err
						}
						obj.(*v1alpha1.Sandbox).Spec.EmbeddedSandboxTemplate = v1alpha1.EmbeddedSandboxTemplate{}
						return nil
					},
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
						patches.Add(1)
						return c.Patch(ctx, obj, p, opts...)
					},
				})
				sbx := AsSandbox(stale.DeepCopy(), &apiReaderOverrideCache{Provider: cache, client: intercepted})
				err = submissionTestCall(t.Context(), sbx, paused, nil, nil)
				if observation == "opposite won" {
					assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err))
				} else {
					require.NoError(t, err)
				}
				assert.GreaterOrEqual(t, patches.Load(), int32(2), "stale no-op/rejection must be validated and retried")
				require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(initial), &current))
				wantPaused := paused
				if observation == "opposite won" {
					wantPaused = !paused
				}
				assert.Equal(t, wantPaused, current.Spec.Paused)
				assert.Equal(t, initial.Spec.EmbeddedSandboxTemplate, current.Spec.EmbeddedSandboxTemplate)
				assert.Equal(t, "keep", current.Annotations["external"])
			})
		}
	}
}

func TestSandbox_SubmissionConcurrent(t *testing.T) {
	for _, paused := range []bool{true, false} {
		t.Run(fmt.Sprintf("paused=%t", paused), func(t *testing.T) {
			initial := submissionTestSandbox(!paused)
			cache, fc, err := cachetest.NewTestCache(t, initial)
			require.NoError(t, err)
			start := make(chan struct{})
			results := make(chan error, 4)
			for i := 0; i < cap(results); i++ {
				go func() {
					<-start
					results <- submissionTestCall(t.Context(), AsSandbox(initial.DeepCopy(), cache), paused, nil, nil)
				}()
			}
			close(start)
			for i := 0; i < cap(results); i++ {
				require.NoError(t, <-results)
			}
			var persisted v1alpha1.Sandbox
			require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(initial), &persisted))
			assert.Equal(t, paused, persisted.Spec.Paused)
			cache.GetWaitHooks().Range(func(key, value any) bool { t.Errorf("submission leaked wait hook %v", key); return true })
		})
	}
}
