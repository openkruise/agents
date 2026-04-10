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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
