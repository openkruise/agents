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

package utils

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func newTaskTestSandbox(name string) *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
	}
}

func TestWaitTask_AlreadySatisfied_ReturnsImmediately(t *testing.T) {
	hooks := &sync.Map{}
	task := NewWaitTask[*agentsv1alpha1.Sandbox](
		context.Background(), hooks, WaitActionPause, newTaskTestSandbox("sbx-1"),
		func(s *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) { return s, nil },
		func(s *agentsv1alpha1.Sandbox) (bool, error) { return true, nil },
	)
	err := task.Wait(5 * time.Second)
	assert.NoError(t, err)
	// hooks should be empty — fast path never registers an entry
	count := 0
	hooks.Range(func(_, _ any) bool { count++; return true })
	assert.Equal(t, 0, count, "satisfied fast path must not create a wait hook")
}

func TestWaitTask_ZeroTimeout_ReturnsError(t *testing.T) {
	hooks := &sync.Map{}
	task := NewWaitTask[*agentsv1alpha1.Sandbox](
		context.Background(), hooks, WaitActionPause, newTaskTestSandbox("sbx-2"),
		func(s *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) { return s, nil },
		func(s *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
	)
	err := task.Wait(0)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "object is not satisfied")
}

func TestWaitTask_Accessors(t *testing.T) {
	hooks := &sync.Map{}
	sbx := newTaskTestSandbox("sbx-3")
	task := NewWaitTask[*agentsv1alpha1.Sandbox](
		context.Background(), hooks, WaitActionResume, sbx,
		func(s *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) { return s, nil },
		func(s *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
	)
	assert.Equal(t, WaitActionResume, task.Action())
	assert.Same(t, sbx, task.Object())
}

func TestWaitTask_CapturedCtxCancel_TriggersDoubleCheck(t *testing.T) {
	hooks := &sync.Map{}
	ctx, cancel := context.WithCancel(context.Background())
	updateCalled := 0
	task := NewWaitTask[*agentsv1alpha1.Sandbox](
		ctx, hooks, WaitActionPause, newTaskTestSandbox("sbx-4"),
		func(s *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
			updateCalled++
			return s, nil
		},
		func(s *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
	)
	// Cancel after 20ms, well before timeout. Wait must return via double-check branch.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := task.Wait(2 * time.Second)
	assert.Error(t, err)
	assert.GreaterOrEqual(t, updateCalled, 1, "double-check must call update at least once")
}

func TestNewAcquiredWaitTask(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "acquires immediately and release is idempotent",
			run: func(t *testing.T) {
				hooks := &sync.Map{}
				sbx := newTaskTestSandbox("sbx-acquired-release")

				task, err := NewAcquiredWaitTask[*agentsv1alpha1.Sandbox](
					context.Background(), hooks, WaitActionPause, sbx,
					func(s *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) { return s, nil },
					func(s *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
				)
				require.NoError(t, err)
				assert.Equal(t, WaitActionPause, task.Action())
				assert.Same(t, sbx, task.Object())

				key := WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
				stored, exists := hooks.Load(key)
				require.True(t, exists)
				assert.Same(t, task.entry, stored)

				task.Release()
				task.Release()

				_, exists = hooks.Load(key)
				assert.False(t, exists)
			},
		},
		{
			name: "different action conflicts before wait",
			run: func(t *testing.T) {
				hooks := &sync.Map{}
				sbx := newTaskTestSandbox("sbx-acquired-conflict")
				update := func(s *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) { return s, nil }
				check := func(s *agentsv1alpha1.Sandbox) (bool, error) { return false, nil }

				pauseTask, err := NewAcquiredWaitTask[*agentsv1alpha1.Sandbox](
					context.Background(), hooks, WaitActionPause, sbx, update, check,
				)
				require.NoError(t, err)
				defer pauseTask.Release()

				resumeTask, err := NewAcquiredWaitTask[*agentsv1alpha1.Sandbox](
					context.Background(), hooks, WaitActionResume, sbx, update, check,
				)
				require.Error(t, err)
				assert.Nil(t, resumeTask)
				assert.Contains(t, err.Error(), "another action(Pause)'s wait task already exists")
			},
		},
		{
			name: "wait uses existing entry and releases it",
			run: func(t *testing.T) {
				hooks := &sync.Map{}
				sbx := newTaskTestSandbox("sbx-acquired-wait")
				updated := sbx.DeepCopy()
				updated.Labels = map[string]string{"ready": "true"}

				task, err := NewAcquiredWaitTask[*agentsv1alpha1.Sandbox](
					context.Background(), hooks, WaitActionResume, sbx,
					func(s *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
						return updated, nil
					},
					func(s *agentsv1alpha1.Sandbox) (bool, error) {
						return s.Labels["ready"] == "true", nil
					},
				)
				require.NoError(t, err)

				err = task.Wait(time.Second)
				require.NoError(t, err)

				key := WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
				_, exists := hooks.Load(key)
				assert.False(t, exists, "acquired task wait must release its pre-acquired entry")
			},
		},
		{
			name: "wait after release returns single-use error",
			run: func(t *testing.T) {
				hooks := &sync.Map{}
				sbx := newTaskTestSandbox("sbx-acquired-release-before-wait")

				task, err := NewAcquiredWaitTask[*agentsv1alpha1.Sandbox](
					context.Background(), hooks, WaitActionPause, sbx,
					func(s *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) { return s, nil },
					func(s *agentsv1alpha1.Sandbox) (bool, error) { return true, nil },
				)
				require.NoError(t, err)

				task.Release()

				err = task.Wait(time.Second)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "pre-acquired wait task already used or released")

				key := WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
				_, exists := hooks.Load(key)
				assert.False(t, exists)
			},
		},
		{
			name: "wait twice returns single-use error on second wait",
			run: func(t *testing.T) {
				hooks := &sync.Map{}
				sbx := newTaskTestSandbox("sbx-acquired-wait-twice")
				updated := sbx.DeepCopy()
				updated.Labels = map[string]string{"ready": "true"}

				task, err := NewAcquiredWaitTask[*agentsv1alpha1.Sandbox](
					context.Background(), hooks, WaitActionResume, sbx,
					func(s *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
						return updated, nil
					},
					func(s *agentsv1alpha1.Sandbox) (bool, error) {
						return s.Labels["ready"] == "true", nil
					},
				)
				require.NoError(t, err)

				err = task.Wait(time.Second)
				require.NoError(t, err)

				err = task.Wait(time.Second)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "pre-acquired wait task already used or released")
			},
		},
		{
			name: "concurrent wait allows only one waiter",
			run: func(t *testing.T) {
				hooks := &sync.Map{}
				sbx := newTaskTestSandbox("sbx-acquired-concurrent-wait")
				updated := sbx.DeepCopy()
				updated.Labels = map[string]string{"ready": "true"}
				updateStarted := make(chan struct{})
				allowUpdate := make(chan struct{})
				var updateCalls int32

				task, err := NewAcquiredWaitTask[*agentsv1alpha1.Sandbox](
					context.Background(), hooks, WaitActionResume, sbx,
					func(s *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
						if atomic.AddInt32(&updateCalls, 1) == 1 {
							close(updateStarted)
							<-allowUpdate
						}
						return updated, nil
					},
					func(s *agentsv1alpha1.Sandbox) (bool, error) {
						return s.Labels["ready"] == "true", nil
					},
				)
				require.NoError(t, err)

				firstErrCh := make(chan error, 1)
				go func() {
					firstErrCh <- task.Wait(time.Second)
				}()

				select {
				case <-updateStarted:
				case <-time.After(time.Second):
					t.Fatal("first wait did not start")
				}

				err = task.Wait(time.Second)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "pre-acquired wait task already used or released")

				close(allowUpdate)
				require.NoError(t, <-firstErrCh)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
