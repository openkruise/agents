package sandboxset

import (
	"context"
	"testing"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
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
							APIVersion: sandboxSetControllerKind.GroupVersion().String(),
							Kind:       sandboxSetControllerKind.Kind,
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
							APIVersion: sandboxSetControllerKind.GroupVersion().String(),
							Kind:       sandboxSetControllerKind.Kind,
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
			scaleExpectation.DeleteExpectations(controllerKey)
			if tt.hasExpectation {
				scaleExpectation.ExpectScale(controllerKey, expectations.Create, tt.sandbox.Name)
			}
			handler.Create(context.TODO(), createEvent, queue)
			satisfied, _, _ := scaleExpectation.SatisfiedExpectations(controllerKey)
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
	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandboxset",
			Namespace: "default",
			UID:       "123456789",
		},
	}
	ownerReferences := []metav1.OwnerReference{
		{
			APIVersion: sandboxSetControllerKind.GroupVersion().String(),
			Kind:       sandboxSetControllerKind.Kind,
			Name:       sbs.Name,
			UID:        sbs.UID,
			Controller: ptr.To(true),
		},
	}
	testCases := []struct {
		name             string
		oldSandbox       *agentsv1alpha1.Sandbox
		newSandbox       *agentsv1alpha1.Sandbox
		shouldAddToQueue bool
	}{
		{
			name: "reason changed",
			oldSandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxState: agentsv1alpha1.SandboxStateAvailable,
					},
					OwnerReferences: ownerReferences,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			newSandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxState: agentsv1alpha1.SandboxStateRunning,
					},
					OwnerReferences: ownerReferences,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			shouldAddToQueue: true,
		},
		{
			name: "creating and ready",
			oldSandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-sandbox",
					Namespace:       "default",
					OwnerReferences: ownerReferences,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(agentsv1alpha1.SandboxConditionReady),
							Status: metav1.ConditionFalse,
						},
					},
				},
			},
			newSandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-sandbox",
					Namespace:       "default",
					OwnerReferences: ownerReferences,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(agentsv1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
				},
			},
			shouldAddToQueue: true,
		},
		{
			name: "reason not changed",
			oldSandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxState: agentsv1alpha1.SandboxStateAvailable,
					},
					OwnerReferences: ownerReferences,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			newSandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxState: agentsv1alpha1.SandboxStateAvailable,
					},
					OwnerReferences: ownerReferences,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			shouldAddToQueue: false,
		},
		{
			name: "not controlled by sandboxset",
			oldSandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxState: agentsv1alpha1.SandboxStateAvailable,
					},
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
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			newSandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxState: agentsv1alpha1.SandboxStateRunning,
					},
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
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			shouldAddToQueue: false,
		},
	}

	handler := &SandboxEventHandler{}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			queue := &fakePriorityQueue{}

			updateEvent := event.TypedUpdateEvent[client.Object]{
				ObjectOld: tt.oldSandbox,
				ObjectNew: tt.newSandbox,
			}

			handler.Update(context.TODO(), updateEvent, queue)
			controllerKey := GetControllerKey(sbs)
			if tt.shouldAddToQueue {
				assert.Equal(t, controllerKey, queue.request.String())
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
			APIVersion: sandboxSetControllerKind.GroupVersion().String(),
			Kind:       sandboxSetControllerKind.Kind,
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
			scaleExpectation.DeleteExpectations(controllerKey)
			if tt.hasExpectation {
				scaleExpectation.ExpectScale(controllerKey, expectations.Delete, tt.sandbox.Name)
			}
			handler.Delete(context.TODO(), evt, queue)
			satisfied, _, _ := scaleExpectation.SatisfiedExpectations(controllerKey)
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
