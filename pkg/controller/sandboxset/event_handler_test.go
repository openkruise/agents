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

package sandboxset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

type fakePriorityQueue struct {
	workqueue.TypedRateLimitingInterface[reconcile.Request]
	request reconcile.Request
}

func (f *fakePriorityQueue) Add(item reconcile.Request) {
	f.request = item
}

func TestSandboxEventHandler_Create(t *testing.T) {
	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandboxset",
			Namespace: "default",
			UID:       "123456789",
		},
	}
	testCases := []struct {
		name             string
		sandbox          *agentsv1alpha1.Sandbox
		hasExpectation   bool
		shouldAddToQueue bool
	}{
		{
			name: "owned by sandboxset, has expectation",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: agentsv1alpha1.SandboxSetControllerKind.GroupVersion().String(),
							Kind:       agentsv1alpha1.SandboxSetControllerKind.Kind,
							Name:       sbs.Name,
							UID:        sbs.UID,
							Controller: ptr.To(true),
						},
					},
				},
			},
			hasExpectation:   true,
			shouldAddToQueue: true,
		},
		{
			name: "owned by sandboxset, no expectation",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: agentsv1alpha1.SandboxSetControllerKind.GroupVersion().String(),
							Kind:       agentsv1alpha1.SandboxSetControllerKind.Kind,
							Name:       sbs.Name,
							UID:        sbs.UID,
							Controller: ptr.To(true),
						},
					},
				},
			},
			shouldAddToQueue: true,
		},
		{
			name: "not owned by sandboxset, has expectation",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: appsv1.SchemeGroupVersion.String(),
							Kind:       "Deployment",
							Name:       sbs.Name,
							UID:        sbs.UID,
							Controller: ptr.To(true),
						},
					},
				},
			},
			hasExpectation:   true,
			shouldAddToQueue: false,
		},
		{
			name: "not owned by sandboxset, no expectation",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: appsv1.SchemeGroupVersion.String(),
							Kind:       "Deployment",
							Name:       sbs.Name,
							UID:        sbs.UID,
							Controller: ptr.To(true),
						},
					},
				},
			},
			shouldAddToQueue: false,
		},
	}

	handler := &SandboxEventHandler{}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			queue := &fakePriorityQueue{}
			createEvent := event.TypedCreateEvent[client.Object]{
				Object: tt.sandbox,
			}
			controllerKey := GetControllerKey(sbs)
			scaleUpExpectation.DeleteExpectations(controllerKey)
			if tt.hasExpectation {
				scaleUpExpectation.ExpectScale(controllerKey, expectations.Create, tt.sandbox.Name)
			}
			handler.Create(context.TODO(), createEvent, queue)
			satisfied, _, _ := scaleUpExpectation.SatisfiedExpectations(controllerKey)
			if tt.shouldAddToQueue {
				assert.Equal(t, controllerKey, queue.request.String())
				assert.True(t, satisfied)
			} else {
				assert.Equal(t, "/", queue.request.String())
				assert.NotEqual(t, tt.hasExpectation, satisfied)
			}
		})
	}
}

func TestSandboxEventHandler_Update(t *testing.T) {
	sbs := &agentsv1alpha1.SandboxSet{ObjectMeta: metav1.ObjectMeta{
		Name: "test-sandboxset", Namespace: "default", UID: "123456789",
	}}
	newSandbox := func(phase agentsv1alpha1.SandboxPhase, conditions []metav1.Condition) *agentsv1alpha1.Sandbox {
		return &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-sandbox",
				Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: agentsv1alpha1.SandboxSetControllerKind.GroupVersion().String(),
					Kind:       agentsv1alpha1.SandboxSetControllerKind.Kind,
					Name:       sbs.Name,
					UID:        sbs.UID,
					Controller: ptr.To(true),
				}},
			},
			Status: agentsv1alpha1.SandboxStatus{Phase: phase, Conditions: conditions},
		}
	}
	readyFailure := []metav1.Condition{{
		Type:   string(agentsv1alpha1.SandboxConditionReady),
		Status: metav1.ConditionFalse,
		Reason: agentsv1alpha1.SandboxReadyReasonStartContainerFailed,
	}}
	readyPending := []metav1.Condition{{
		Type:   string(agentsv1alpha1.SandboxConditionReady),
		Status: metav1.ConditionFalse,
		Reason: agentsv1alpha1.SandboxReadyReasonPodReady,
	}}
	readyPodCreateFailed := []metav1.Condition{{
		Type:   string(agentsv1alpha1.SandboxConditionReady),
		Status: metav1.ConditionFalse,
		Reason: agentsv1alpha1.SandboxReadyReasonPodCreateFailed,
	}}
	tests := []struct {
		name          string
		oldSandbox    *agentsv1alpha1.Sandbox
		newSandbox    *agentsv1alpha1.Sandbox
		shouldEnqueue bool
	}{
		{
			name:          "startup failure enters while sandbox remains creating",
			oldSandbox:    newSandbox(agentsv1alpha1.SandboxPending, readyPending),
			newSandbox:    newSandbox(agentsv1alpha1.SandboxPending, readyFailure),
			shouldEnqueue: true,
		},
		{
			name:          "pod create failure enters while sandbox remains creating",
			oldSandbox:    newSandbox(agentsv1alpha1.SandboxPending, readyPending),
			newSandbox:    newSandbox(agentsv1alpha1.SandboxPending, readyPodCreateFailed),
			shouldEnqueue: true,
		},
		{
			name:          "startup failure clears while sandbox remains creating",
			oldSandbox:    newSandbox(agentsv1alpha1.SandboxPending, readyFailure),
			newSandbox:    newSandbox(agentsv1alpha1.SandboxPending, readyPending),
			shouldEnqueue: true,
		},
		{
			name:       "unrelated pending update is ignored",
			oldSandbox: newSandbox(agentsv1alpha1.SandboxPending, readyPending),
			newSandbox: newSandbox(agentsv1alpha1.SandboxPending, readyPending),
		},
		{
			name:          "sandbox state change remains enqueued",
			oldSandbox:    newSandbox(agentsv1alpha1.SandboxPending, nil),
			newSandbox:    newSandbox(agentsv1alpha1.SandboxFailed, nil),
			shouldEnqueue: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queue := &fakePriorityQueue{}
			(&SandboxEventHandler{}).Update(t.Context(), event.TypedUpdateEvent[client.Object]{
				ObjectOld: tt.oldSandbox,
				ObjectNew: tt.newSandbox,
			}, queue)
			if tt.shouldEnqueue {
				assert.Equal(t, GetControllerKey(sbs), queue.request.String())
			} else {
				assert.Equal(t, "/", queue.request.String())
			}
		})
	}
}

func TestSandboxEventHandler_Delete(t *testing.T) {
	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandboxset",
			Namespace: "default",
			UID:       "123456789",
		},
	}
	ownerReferences := []metav1.OwnerReference{
		{
			APIVersion: agentsv1alpha1.SandboxSetControllerKind.GroupVersion().String(),
			Kind:       agentsv1alpha1.SandboxSetControllerKind.Kind,
			Name:       sbs.Name,
			UID:        sbs.UID,
			Controller: ptr.To(true),
		},
	}
	testCases := []struct {
		name             string
		sandbox          *agentsv1alpha1.Sandbox
		hasExpectation   bool
		shouldAddToQueue bool
	}{
		{
			name: "owned by sandboxset, has expectation",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-sandbox",
					Namespace:       "default",
					OwnerReferences: ownerReferences,
				},
			},
			hasExpectation:   true,
			shouldAddToQueue: true,
		},
		{
			name: "owned by sandboxset, no expectation",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-sandbox",
					Namespace:       "default",
					OwnerReferences: ownerReferences,
				},
			},
			hasExpectation:   false,
			shouldAddToQueue: true,
		},
		{
			name: "not owned by sandboxset, has expectation",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: appsv1.SchemeGroupVersion.String(),
							Kind:       "Deployment",
							Name:       sbs.Name,
							UID:        sbs.UID,
							Controller: ptr.To(true),
						},
					},
				},
			},
			hasExpectation:   true,
			shouldAddToQueue: false,
		},
		{
			name: "not owned by sandboxset, no expectation",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: appsv1.SchemeGroupVersion.String(),
							Kind:       "Deployment",
							Name:       sbs.Name,
							UID:        sbs.UID,
							Controller: ptr.To(true),
						},
					},
				},
			},
			hasExpectation:   false,
			shouldAddToQueue: false,
		},
	}
	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			queue := &fakePriorityQueue{}

			evt := event.TypedDeleteEvent[client.Object]{
				Object: tt.sandbox,
			}
			handler := &SandboxEventHandler{}
			controllerKey := GetControllerKey(sbs)
			scaleDownExpectation.DeleteExpectations(controllerKey)
			if tt.hasExpectation {
				scaleDownExpectation.ExpectScale(controllerKey, expectations.Delete, tt.sandbox.Name)
			}
			handler.Delete(context.TODO(), evt, queue)
			satisfied, _, _ := scaleDownExpectation.SatisfiedExpectations(controllerKey)
			if tt.shouldAddToQueue {
				assert.Equal(t, controllerKey, queue.request.String())
				assert.True(t, satisfied)
			} else {
				assert.Equal(t, "/", queue.request.String())
				assert.NotEqual(t, tt.hasExpectation, satisfied)
			}
		})
	}
}
