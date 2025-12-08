package sandboxcr

import (
	"context"
	"testing"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned/fake"
	informers "github.com/openkruise/agents/client/informers/externalversions"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	client2 "sigs.k8s.io/controller-runtime/pkg/client"
)

func AsSandbox(sbx *v1alpha1.Sandbox, client *fake.Clientset, cache Cache[*v1alpha1.Sandbox]) *Sandbox {
	s := &Sandbox{
		BaseSandbox: BaseSandbox[*v1alpha1.Sandbox]{
			Sandbox:       sbx,
			Cache:         cache,
			SetCondition:  SetSandboxCondition,
			GetConditions: ListSandboxConditions,
			DeepCopy:      DeepCopy,
		},
		Sandbox: sbx,
	}
	if client != nil {
		s.PatchSandbox = client.ApiV1alpha1().Sandboxes("default").Patch
		s.UpdateStatus = client.ApiV1alpha1().Sandboxes("default").UpdateStatus
		s.DeleteFunc = client.ApiV1alpha1().Sandboxes("default").Delete
	}
	return s
}

func ConvertPodToSandboxCR(pod *corev1.Pod) *v1alpha1.Sandbox {
	return &v1alpha1.Sandbox{
		ObjectMeta: pod.ObjectMeta,
		Spec: v1alpha1.SandboxSpec{
			Template: corev1.PodTemplateSpec{
				Spec: pod.Spec,
			},
		},
		Status: v1alpha1.SandboxStatus{
			Phase: v1alpha1.SandboxPhase(pod.Status.Phase),
			PodInfo: v1alpha1.PodInfo{
				PodIP: pod.Status.PodIP,
			},
		},
	}
}

func TestSandbox_GetState(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "returns sandbox state label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelSandboxState: v1alpha1.SandboxStateRunning,
					},
				},
			},
			want: v1alpha1.SandboxStateRunning,
		},
		{
			name: "empty state",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelSandboxState: "",
					},
				},
			},
			want: "",
		},
		{
			name: "no state label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{},
				},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := AsSandbox(ConvertPodToSandboxCR(tt.pod), nil, nil)
			if got := s.GetState(); got != tt.want {
				t.Errorf("GetState() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSandbox_GetTemplate(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "returns sandbox pool label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelSandboxPool: "test-template",
					},
				},
			},
			want: "test-template",
		},
		{
			name: "empty template",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelSandboxPool: "",
					},
				},
			},
			want: "",
		},
		{
			name: "no template label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{},
				},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := AsSandbox(ConvertPodToSandboxCR(tt.pod), nil, nil)
			if got := s.GetTemplate(); got != tt.want {
				t.Errorf("GetTemplate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSandbox_GetResource(t *testing.T) {
	cpuQuantity1, _ := resource.ParseQuantity("1000m")
	cpuQuantity2, _ := resource.ParseQuantity("500m")
	memoryQuantity1, _ := resource.ParseQuantity("1024Mi")
	memoryQuantity2, _ := resource.ParseQuantity("512Mi")

	tests := []struct {
		name string
		pod  *corev1.Pod
		want infra.SandboxResource
	}{
		{
			name: "single container with resources",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQuantity1,
									corev1.ResourceMemory: memoryQuantity1,
								},
							},
						},
					},
				},
			},
			want: infra.SandboxResource{
				CPUMilli: 1000,
				MemoryMB: 1024,
			},
		},
		{
			name: "multiple containers with resources",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQuantity1,
									corev1.ResourceMemory: memoryQuantity1,
								},
							},
						},
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQuantity2,
									corev1.ResourceMemory: memoryQuantity2,
								},
							},
						},
					},
				},
			},
			want: infra.SandboxResource{
				CPUMilli: 1500,
				MemoryMB: 1536,
			},
		},
		{
			name: "no containers",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{},
				},
			},
			want: infra.SandboxResource{
				CPUMilli: 0,
				MemoryMB: 0,
			},
		},
		{
			name: "containers without resources",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{},
							},
						},
					},
				},
			},
			want: infra.SandboxResource{
				CPUMilli: 0,
				MemoryMB: 0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := AsSandbox(ConvertPodToSandboxCR(tt.pod), nil, nil)
			got := s.GetResource()
			if got.CPUMilli != tt.want.CPUMilli {
				t.Errorf("GetResource().CPUMilli = %v, want %v", got.CPUMilli, tt.want.CPUMilli)
			}
			if got.MemoryMB != tt.want.MemoryMB {
				t.Errorf("GetResource().MemoryMB = %v, want %v", got.MemoryMB, tt.want.MemoryMB)
			}
		})
	}
}

func TestSandbox_SetState(t *testing.T) {
	tests := []struct {
		name          string
		initialLabels map[string]string
		setState      string
		expectedState string
	}{
		{
			name:          "set state on sandbox without labels",
			initialLabels: map[string]string{},
			setState:      v1alpha1.SandboxStateRunning,
			expectedState: v1alpha1.SandboxStateRunning,
		},
		{
			name: "set state on sandbox with existing state",
			initialLabels: map[string]string{
				v1alpha1.LabelSandboxState: v1alpha1.SandboxStatePaused,
			},
			setState:      v1alpha1.SandboxStateKilling,
			expectedState: v1alpha1.SandboxStateKilling,
		},
		{
			name: "set empty state",
			initialLabels: map[string]string{
				v1alpha1.LabelSandboxState: v1alpha1.SandboxStateRunning,
			},
			setState:      "",
			expectedState: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create Pod with initial labels
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels:    tt.initialLabels,
				},
			}
			sandbox := ConvertPodToSandboxCR(pod)

			// Using fake client
			//goland:noinspection GoDeprecation
			client := fake.NewSimpleClientset()
			_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), sandbox, metav1.CreateOptions{})
			assert.NoError(t, err)
			// Create Sandbox instance
			s := AsSandbox(ConvertPodToSandboxCR(pod), client, nil)

			// Call SetState method
			err = s.SetState(context.Background(), tt.setState)
			assert.NoError(t, err)

			// Verify that the state is set correctly
			updatedSbx, err := client.ApiV1alpha1().Sandboxes("default").Get(context.Background(), "test-sandbox", metav1.GetOptions{})
			assert.NoError(t, err)
			assert.Equal(t, tt.expectedState, updatedSbx.Labels[v1alpha1.LabelSandboxState])
		})
	}
}

func TestSandbox_PatchLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		expect map[string]string
	}{
		{
			name: "patch labels",
			labels: map[string]string{
				"foo":     "baz",
				"another": "value",
			},
			expect: map[string]string{
				"foo":     "baz",
				"another": "value",
			},
		},
		{
			name: "without foo",
			labels: map[string]string{
				"another": "value",
			},
			expect: map[string]string{
				"foo":     "bar",
				"another": "value",
			},
		},
		{
			name: "nil labels",
			expect: map[string]string{
				"foo": "bar",
			},
		},
		{
			name:   "empty labels",
			labels: nil,
			expect: map[string]string{
				"foo": "bar",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						"foo": "bar",
					},
				},
			}
			sandbox := ConvertPodToSandboxCR(pod)
			//goland:noinspection GoDeprecation
			client := fake.NewSimpleClientset()
			_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), sandbox, metav1.CreateOptions{})
			assert.NoError(t, err)
			s := AsSandbox(sandbox, client, nil)
			err = s.PatchLabels(context.Background(), map[string]string{
				"foo":     "baz",
				"another": "value",
			})
			assert.NoError(t, err)
			got, err := client.ApiV1alpha1().Sandboxes("default").Get(context.Background(), "test-sandbox", metav1.GetOptions{})
			assert.NoError(t, err)
			assert.Equal(t, "baz", got.Labels["foo"])
			assert.Equal(t, "value", got.Labels["another"])
		})
	}

}

func TestSandbox_Kill(t *testing.T) {
	tests := []struct {
		name              string
		initialState      string
		deletionTimestamp *metav1.Time
		expectError       bool
	}{
		{
			name:         "kill running sandbox",
			initialState: v1alpha1.SandboxStateRunning,
			expectError:  false,
		},
		{
			name:         "kill paused sandbox",
			initialState: v1alpha1.SandboxStatePaused,
			expectError:  false,
		},
		{
			name:              "kill already deleted sandbox",
			initialState:      v1alpha1.SandboxStateRunning,
			deletionTimestamp: &metav1.Time{},
			expectError:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create Pod with initial state
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.LabelSandboxState: tt.initialState,
					},
					DeletionTimestamp: tt.deletionTimestamp,
				},
			}
			sandbox := ConvertPodToSandboxCR(pod)

			// Using fake client
			//goland:noinspection GoDeprecation
			client := fake.NewSimpleClientset()
			_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), sandbox, metav1.CreateOptions{})
			assert.NoError(t, err)

			// Create Sandbox instance
			s := AsSandbox(sandbox, client, nil)

			// Call Kill method
			err = s.Kill(context.Background())

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)

				if tt.deletionTimestamp == nil {
					// Verify that the state is set to killing before deletion
					// Since the Pod has been deleted, we need to check if the status update operation was called
					// In fake client, we can verify by checking if any patch operations occurred
					// But here we can only verify that the method did not return an error
				}
			}
		})
	}
}

func TestSandbox_Patch(t *testing.T) {
	tests := []struct {
		name                string
		initialLabels       map[string]string
		initialAnnotations  map[string]string
		patchStr            string
		expectedLabels      map[string]string
		expectedAnnotations map[string]string
	}{
		{
			name: "add new labels",
			initialLabels: map[string]string{
				"existing": "label",
			},
			initialAnnotations: map[string]string{
				"existing": "annotation",
			},
			patchStr: `{"metadata":{"labels":{"new":"label"},"annotations":{"new":"annotation"}}}`,
			expectedLabels: map[string]string{
				"existing": "label",
				"new":      "label",
			},
			expectedAnnotations: map[string]string{
				"existing": "annotation",
				"new":      "annotation",
			},
		},
		{
			name: "update existing labels",
			initialLabels: map[string]string{
				"existing": "old-value",
			},
			initialAnnotations: map[string]string{},
			patchStr:           `{"metadata":{"labels":{"existing":"new-value"}}}`,
			expectedLabels: map[string]string{
				"existing": "new-value",
			},
			expectedAnnotations: map[string]string{},
		},
		{
			name: "empty patch",
			initialLabels: map[string]string{
				"existing": "label",
			},
			initialAnnotations: map[string]string{
				"existing": "annotation",
			},
			patchStr: `{"metadata":{}}`,
			expectedLabels: map[string]string{
				"existing": "label",
			},
			expectedAnnotations: map[string]string{
				"existing": "annotation",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create Pod with initial labels and annotations
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-sandbox",
					Namespace:   "default",
					Labels:      tt.initialLabels,
					Annotations: tt.initialAnnotations,
				},
			}
			sandbox := ConvertPodToSandboxCR(pod)

			// Using fake client
			//goland:noinspection GoDeprecation
			client := fake.NewSimpleClientset()
			_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), sandbox, metav1.CreateOptions{})
			assert.NoError(t, err)

			// Create Sandbox instance
			s := AsSandbox(sandbox, client, nil)

			// Call Patch method
			err = s.Patch(context.Background(), tt.patchStr)
			assert.NoError(t, err)

			// Verify that the patch is applied correctly
			updatedPod, err := client.ApiV1alpha1().Sandboxes("default").Get(context.Background(), "test-sandbox", metav1.GetOptions{})
			assert.NoError(t, err)

			// For empty maps, we need special handling
			if len(tt.expectedLabels) == 0 {
				if updatedPod.Labels == nil {
					// If the expectation is an empty map but the actual is nil, this is acceptable
					assert.True(t, len(updatedPod.Labels) == 0)
				} else {
					assert.Equal(t, tt.expectedLabels, updatedPod.Labels)
				}
			} else {
				assert.Equal(t, tt.expectedLabels, updatedPod.Labels)
			}

			if len(tt.expectedAnnotations) == 0 {
				if updatedPod.Annotations == nil {
					// If the expectation is an empty map but the actual is nil, this is acceptable
					assert.True(t, len(updatedPod.Annotations) == 0)
				} else {
					assert.Equal(t, tt.expectedAnnotations, updatedPod.Annotations)
				}
			} else {
				assert.Equal(t, tt.expectedAnnotations, updatedPod.Annotations)
			}
		})
	}
}

//goland:noinspection GoDeprecation
func TestSandbox_SetPause(t *testing.T) {
	tests := []struct {
		name          string
		phase         v1alpha1.SandboxPhase
		initialState  string
		pauseFinished bool
		originalPause bool
		operatePause  bool
		expectPaused  bool
		expectedState string
		expectError   bool
	}{
		{
			name:          "pause running / running sandbox",
			phase:         v1alpha1.SandboxRunning,
			initialState:  v1alpha1.SandboxStateRunning,
			originalPause: false,
			operatePause:  true,
			expectPaused:  true,
			expectedState: v1alpha1.SandboxStatePaused,
			expectError:   false,
		},
		{
			name:          "pause running / available sandbox",
			phase:         v1alpha1.SandboxRunning,
			initialState:  v1alpha1.SandboxStateAvailable,
			originalPause: false,
			operatePause:  true,
			expectPaused:  true,
			expectedState: v1alpha1.SandboxStateAvailable,
			expectError:   false,
		},
		{
			name:          "resume paused / paused sandbox",
			phase:         v1alpha1.SandboxPaused,
			initialState:  v1alpha1.SandboxStatePaused,
			originalPause: true,
			pauseFinished: true,
			operatePause:  false,
			expectPaused:  false,
			expectedState: v1alpha1.SandboxStateRunning,
			expectError:   false,
		},
		{
			name:          "resume paused / pausing sandbox",
			phase:         v1alpha1.SandboxPaused,
			initialState:  v1alpha1.SandboxStatePaused,
			originalPause: true,
			pauseFinished: false,
			operatePause:  false,
			expectPaused:  false,
			expectedState: v1alpha1.SandboxStateRunning,
			expectError:   true,
		},
		{
			name:          "resume paused / available sandbox",
			phase:         v1alpha1.SandboxPaused,
			initialState:  v1alpha1.SandboxStateAvailable,
			originalPause: true,
			pauseFinished: true,
			operatePause:  false,
			expectPaused:  false,
			expectedState: v1alpha1.SandboxStateAvailable,
			expectError:   false,
		},
		{
			name:          "pause already paused sandbox",
			phase:         v1alpha1.SandboxPaused,
			initialState:  v1alpha1.SandboxStatePaused,
			originalPause: true,
			operatePause:  true,
			expectPaused:  true,
			expectedState: v1alpha1.SandboxStatePaused,
			expectError:   true,
		},
		{
			name:          "resume already running sandbox",
			phase:         v1alpha1.SandboxRunning,
			initialState:  v1alpha1.SandboxStateRunning,
			originalPause: false,
			operatePause:  false,
			expectPaused:  false,
			expectedState: v1alpha1.SandboxStateRunning,
			expectError:   true,
		},
		{
			name:          "resume killing sandbox",
			phase:         v1alpha1.SandboxPaused,
			initialState:  v1alpha1.SandboxStateKilling,
			originalPause: true,
			operatePause:  false,
			expectPaused:  true,
			expectedState: v1alpha1.SandboxStateKilling,
			expectError:   true,
		},
		{
			name:          "pause killing sandbox",
			phase:         v1alpha1.SandboxRunning,
			initialState:  v1alpha1.SandboxStateKilling,
			originalPause: false,
			operatePause:  true,
			expectPaused:  true,
			expectedState: v1alpha1.SandboxStateKilling,
			expectError:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create Pod with initial state
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.LabelSandboxState: tt.initialState,
					},
					Annotations: map[string]string{},
				},
			}

			sandbox := ConvertPodToSandboxCR(pod)
			sandbox.Status.Phase = tt.phase
			if tt.originalPause {
				sandbox.Spec.Paused = true
				var condStatus metav1.ConditionStatus
				if tt.pauseFinished {
					condStatus = metav1.ConditionTrue
				} else {
					condStatus = metav1.ConditionFalse
				}
				sandbox.Status.Conditions = append(sandbox.Status.Conditions, metav1.Condition{
					Type:   string(v1alpha1.SandboxConditionPaused),
					Status: condStatus,
				})
			}
			// Using fake client
			client := fake.NewSimpleClientset()
			CreateSandboxWithStatus(t, client, sandbox)

			// Create cache
			informerFactory := informers.NewSharedInformerFactoryWithOptions(client, time.Minute*10, informers.WithNamespace("default"))
			sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
			cache, err := NewCache[*v1alpha1.Sandbox]("default", informerFactory, sandboxInformer)
			assert.NoError(t, err)

			// Start cache and wait for sync
			done := make(chan struct{})
			go cache.Run(done)
			select {
			case <-done:
				// Cache synced
			case <-time.After(1 * time.Second):
				// Timeout
				t.Fatal("Cache sync timeout")
			}

			// Create Sandbox instance
			s := AsSandbox(sandbox, client, cache)

			// Call SetPause method
			if tt.operatePause {
				err = s.Pause(context.Background())
			} else {
				if !tt.expectError {
					time.AfterFunc(20*time.Millisecond, func() {
						patch := client2.MergeFrom(s.Sandbox.DeepCopy())
						s.Status.Phase = v1alpha1.SandboxRunning
						SetSandboxCondition(s.Sandbox, string(v1alpha1.SandboxConditionReady), metav1.ConditionTrue, "Resume", "")
						data, err := patch.Data(s.Sandbox)
						assert.NoError(t, err)
						_, err = client.ApiV1alpha1().Sandboxes("default").Patch(
							context.Background(), s.Name, types.MergePatchType, data, metav1.PatchOptions{})
						assert.NoError(t, err)
					})
				}
				err = s.Resume(context.Background())
			}
			if tt.expectError {
				assert.Error(t, err)
				return
			} else {
				assert.NoError(t, err)
			}

			// Get updated Pod
			updatedSbx, err := client.ApiV1alpha1().Sandboxes("default").Get(context.Background(), "test-sandbox", metav1.GetOptions{})
			assert.NoError(t, err)

			// Verify that the Pod state is updated correctly
			// Should have performed patch operation
			assert.Equal(t, tt.expectedState, updatedSbx.Labels[v1alpha1.LabelSandboxState])
			assert.Equal(t, tt.operatePause, updatedSbx.Spec.Paused)
		})
	}
}
