package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var (
	podEventHandlerScheme *runtime.Scheme
)

func init() {
	podEventHandlerScheme = runtime.NewScheme()
	_ = corev1.AddToScheme(podEventHandlerScheme)
}

// fakeWorkQueue 是一个模拟的 workqueue 实现，用于测试
type fakeWorkQueue struct {
	addedItems []reconcile.Request
}

func (f *fakeWorkQueue) Add(item reconcile.Request) {
	f.addedItems = append(f.addedItems, item)
}

func (f *fakeWorkQueue) AddAfter(item reconcile.Request, _ time.Duration) {
	f.Add(item)
}

func (f *fakeWorkQueue) AddRateLimited(item reconcile.Request) {
	f.Add(item)
}

func (f *fakeWorkQueue) Forget(reconcile.Request) {}

func (f *fakeWorkQueue) NumRequeues(reconcile.Request) int {
	return 0
}

func (f *fakeWorkQueue) When() time.Duration {
	return 0
}

func (f *fakeWorkQueue) Done(reconcile.Request) {}

func (f *fakeWorkQueue) ShutDown() {}

func (f *fakeWorkQueue) ShutDownWithDrain() {}

func (f *fakeWorkQueue) ShuttingDown() bool {
	return false
}

func (f *fakeWorkQueue) Get() (item reconcile.Request, shutdown bool) {
	return reconcile.Request{}, false
}

func (f *fakeWorkQueue) Len() int {
	return len(f.addedItems)
}

func (f *fakeWorkQueue) GetAddedItems() []reconcile.Request {
	return f.addedItems
}

func TestSandboxPodEventHandler_Create(t *testing.T) {
	testCases := []struct {
		name             string
		pod              *corev1.Pod
		shouldAddToQueue bool
	}{
		{
			name: "有enable-paused注解",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-pod",
					Namespace:   "default",
					Annotations: map[string]string{},
				},
			},
			shouldAddToQueue: true,
		},
		{
			name: "无enable-paused注解",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
			},
			shouldAddToQueue: false,
		},
		{
			name: "enable-paused注解为空",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-pod",
					Namespace:   "default",
					Annotations: map[string]string{},
				},
			},
			shouldAddToQueue: false,
		},
	}

	handler := &SandboxPodEventHandler{}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			queue := &fakeWorkQueue{}

			createEvent := event.TypedCreateEvent[client.Object]{
				Object: tc.pod,
			}

			handler.Create(context.TODO(), createEvent, queue)

			if tc.shouldAddToQueue && len(queue.GetAddedItems()) == 0 {
				t.Errorf("期望添加到队列，但队列为空")
			} else if !tc.shouldAddToQueue && len(queue.GetAddedItems()) > 0 {
				t.Errorf("不应添加到队列，但队列中有 %d 项", len(queue.GetAddedItems()))
			}

			if tc.shouldAddToQueue && len(queue.GetAddedItems()) > 0 {
				expectedReq := reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      tc.pod.Name,
						Namespace: tc.pod.Namespace,
					},
				}
				actualReq := queue.GetAddedItems()[0]

				if actualReq != expectedReq {
					t.Errorf("添加到队列的请求不匹配，期望: %v, 实际: %v", expectedReq, actualReq)
				}
			}
		})
	}
}

func TestSandboxPodEventHandler_Update(t *testing.T) {
	testCases := []struct {
		name             string
		oldPod           *corev1.Pod
		newPod           *corev1.Pod
		shouldAddToQueue bool
	}{
		{
			name: "新Pod有created-by注解",
			oldPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
			},
			newPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						utils.PodAnnotationCreatedBy: utils.CreatedBySandbox,
					},
				},
			},
			shouldAddToQueue: true,
		},
		{
			name: "新Pod无created-by注解",
			oldPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						utils.PodAnnotationCreatedBy: utils.CreatedBySandbox,
					},
				},
			},
			newPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
			},
			shouldAddToQueue: false,
		},
		{
			name: "新Pod的created-by注解为空",
			oldPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
			},
			newPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						utils.PodAnnotationCreatedBy: "",
					},
				},
			},
			shouldAddToQueue: false,
		},
	}

	handler := &SandboxPodEventHandler{}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			queue := &fakeWorkQueue{}

			updateEvent := event.TypedUpdateEvent[client.Object]{
				ObjectOld: tc.oldPod,
				ObjectNew: tc.newPod,
			}

			handler.Update(context.TODO(), updateEvent, queue)

			if tc.shouldAddToQueue && len(queue.GetAddedItems()) == 0 {
				t.Errorf("期望添加到队列，但队列为空")
			} else if !tc.shouldAddToQueue && len(queue.GetAddedItems()) > 0 {
				t.Errorf("不应添加到队列，但队列中有 %d 项", len(queue.GetAddedItems()))
			}

			if tc.shouldAddToQueue && len(queue.GetAddedItems()) > 0 {
				expectedReq := reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      tc.newPod.Name,
						Namespace: tc.newPod.Namespace,
					},
				}
				actualReq := queue.GetAddedItems()[0]

				if actualReq != expectedReq {
					t.Errorf("添加到队列的请求不匹配，期望: %v, 实际: %v", expectedReq, actualReq)
				}
			}
		})
	}
}

func TestSandboxPodEventHandler_Delete(t *testing.T) {
	testCases := []struct {
		name             string
		pod              *corev1.Pod
		shouldAddToQueue bool
	}{
		{
			name: "有created-by注解",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						utils.PodAnnotationCreatedBy: utils.CreatedBySandbox,
					},
				},
			},
			shouldAddToQueue: true,
		},
		{
			name: "无created-by注解",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
			},
			shouldAddToQueue: false,
		},
		{
			name: "created-by注解为空",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						utils.PodAnnotationCreatedBy: "",
					},
				},
			},
			shouldAddToQueue: false,
		},
	}

	handler := &SandboxPodEventHandler{}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			queue := &fakeWorkQueue{}

			deleteEvent := event.TypedDeleteEvent[client.Object]{
				Object: tc.pod,
			}

			handler.Delete(context.TODO(), deleteEvent, queue)

			if tc.shouldAddToQueue && len(queue.GetAddedItems()) == 0 {
				t.Errorf("期望添加到队列，但队列为空")
			} else if !tc.shouldAddToQueue && len(queue.GetAddedItems()) > 0 {
				t.Errorf("不应添加到队列，但队列中有 %d 项", len(queue.GetAddedItems()))
			}

			if tc.shouldAddToQueue && len(queue.GetAddedItems()) > 0 {
				expectedReq := reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      tc.pod.Name,
						Namespace: tc.pod.Namespace,
					},
				}
				actualReq := queue.GetAddedItems()[0]

				if actualReq != expectedReq {
					t.Errorf("添加到队列的请求不匹配，期望: %v, 实际: %v", expectedReq, actualReq)
				}
			}
		})
	}
}

func TestSandboxPodEventHandler_Generic(t *testing.T) {
	handler := &SandboxPodEventHandler{}
	queue := &fakeWorkQueue{}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
	}

	genericEvent := event.TypedGenericEvent[client.Object]{
		Object: pod,
	}

	// Generic 方法应该不添加任何内容到队列
	handler.Generic(context.TODO(), genericEvent, queue)

	if len(queue.GetAddedItems()) != 0 {
		t.Errorf("Generic 方法应该不添加任何内容到队列，但添加了 %d 项", len(queue.GetAddedItems()))
	}
}
