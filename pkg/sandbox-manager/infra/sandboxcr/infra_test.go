package sandboxcr

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned/fake"
	informers "github.com/openkruise/agents/client/informers/externalversions"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

//goland:noinspection GoDeprecation
func TestInfra_SelectSandboxes(t *testing.T) {
	tests := []struct {
		name           string
		pods           []*corev1.Pod
		options        infra.SandboxSelectorOptions
		expectedCount  int
		expectedStates []string
	}{
		{
			name: "select running sandboxes",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod1",
						Namespace: "default",
						Labels: map[string]string{
							v1alpha1.LabelSandboxState: v1alpha1.SandboxStateRunning,
							v1alpha1.LabelSandboxPool:  "test-template",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod2",
						Namespace: "default",
						Labels: map[string]string{
							v1alpha1.LabelSandboxState: v1alpha1.SandboxStatePaused,
							v1alpha1.LabelSandboxPool:  "test-template",
						},
					},
				},
			},
			options: infra.SandboxSelectorOptions{
				WantRunning:   true,
				WantPaused:    false,
				WantAvailable: false,
			},
			expectedCount:  1,
			expectedStates: []string{v1alpha1.SandboxStateRunning},
		},
		{
			name: "select paused sandboxes",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod1",
						Namespace: "default",
						Labels: map[string]string{
							v1alpha1.LabelSandboxState: v1alpha1.SandboxStateRunning,
							v1alpha1.LabelSandboxPool:  "test-template",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod2",
						Namespace: "default",
						Labels: map[string]string{
							v1alpha1.LabelSandboxState: v1alpha1.SandboxStatePaused,
							v1alpha1.LabelSandboxPool:  "test-template",
						},
					},
				},
			},
			options: infra.SandboxSelectorOptions{
				WantRunning:   false,
				WantPaused:    true,
				WantAvailable: false,
			},
			expectedCount:  1,
			expectedStates: []string{v1alpha1.SandboxStatePaused},
		},
		{
			name: "select multiple states",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod1",
						Namespace: "default",
						Labels: map[string]string{
							v1alpha1.LabelSandboxState: v1alpha1.SandboxStateRunning,
							v1alpha1.LabelSandboxPool:  "test-template",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod2",
						Namespace: "default",
						Labels: map[string]string{
							v1alpha1.LabelSandboxState: v1alpha1.SandboxStatePaused,
							v1alpha1.LabelSandboxPool:  "test-template",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod3",
						Namespace: "default",
						Labels: map[string]string{
							v1alpha1.LabelSandboxState: v1alpha1.SandboxStateAvailable,
							v1alpha1.LabelSandboxPool:  "test-template",
						},
					},
				},
			},
			options: infra.SandboxSelectorOptions{
				WantRunning:   true,
				WantPaused:    true,
				WantAvailable: false,
			},
			expectedCount:  2,
			expectedStates: []string{v1alpha1.SandboxStateRunning, v1alpha1.SandboxStatePaused},
		},
		{
			name: "select with labels",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod1",
						Namespace: "default",
						Labels: map[string]string{
							v1alpha1.LabelSandboxState: v1alpha1.SandboxStateRunning,
							v1alpha1.LabelSandboxPool:  "test-template",
							"custom-label":             "value1",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod2",
						Namespace: "default",
						Labels: map[string]string{
							v1alpha1.LabelSandboxState: v1alpha1.SandboxStateRunning,
							v1alpha1.LabelSandboxPool:  "test-template",
							"custom-label":             "value2",
						},
					},
				},
			},
			options: infra.SandboxSelectorOptions{
				WantRunning:   true,
				WantPaused:    false,
				WantAvailable: false,
				Labels: map[string]string{
					"custom-label": "value1",
				},
			},
			expectedCount:  1,
			expectedStates: []string{v1alpha1.SandboxStateRunning},
		},
		{
			name: "no matching sandboxes",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod1",
						Namespace: "default",
						Labels: map[string]string{
							v1alpha1.LabelSandboxState: v1alpha1.SandboxStateRunning,
							v1alpha1.LabelSandboxPool:  "test-template",
						},
					},
				},
			},
			options: infra.SandboxSelectorOptions{
				WantRunning:   false,
				WantPaused:    true,
				WantAvailable: false,
			},
			expectedCount:  0,
			expectedStates: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake client
			client := fake.NewSimpleClientset()

			// Create Pod
			for _, pod := range tt.pods {
				sbx := ConvertPodToSandboxCR(pod)
				_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), sbx, metav1.CreateOptions{})
				assert.NoError(t, err)
			}

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

			// Create Infra instance
			infraInstance := &Infra{
				BaseInfra: infra.BaseInfra{
					Namespace: "default",
				},
				Cache:  cache,
				Client: client,
			}

			// Call SelectSandboxes method
			sandboxes, err := infraInstance.SelectSandboxes(tt.options)
			assert.NoError(t, err)
			assert.Equal(t, tt.expectedCount, len(sandboxes))

			// Verify returned sandbox states
			actualStates := make([]string, len(sandboxes))
			for i, sandbox := range sandboxes {
				actualStates[i] = sandbox.GetState()
			}

			// Sort to ensure comparison consistency
			sort.Strings(actualStates)
			sort.Strings(tt.expectedStates)
			assert.Equal(t, tt.expectedStates, actualStates)

			// Stop cache
			cache.Stop()
		})
	}
}

//goland:noinspection GoDeprecation
func TestInfra_GetSandbox(t *testing.T) {
	sbx := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				v1alpha1.LabelSandboxID:    "test-pod",
				v1alpha1.LabelSandboxPool:  "test-pool",
				v1alpha1.LabelSandboxState: v1alpha1.SandboxStateRunning,
			},
		},
	}
	client := fake.NewSimpleClientset()
	_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), sbx, metav1.CreateOptions{})
	assert.NoError(t, err)
	infraInstance, err := NewInfra("default", client, nil)
	assert.NoError(t, err)
	err = infraInstance.Run(context.Background())
	assert.NoError(t, err)
	sandbox, err := infraInstance.GetSandbox("test-pod")
	assert.NoError(t, err)
	_, ok := sandbox.(*Sandbox)
	assert.True(t, ok)
	sandboxes, err := infraInstance.SelectSandboxes(infra.SandboxSelectorOptions{WantRunning: true})
	assert.NoError(t, err)
	assert.Equal(t, 1, len(sandboxes))
	_, ok = sandboxes[0].(*Sandbox)
	assert.True(t, ok)
	noSandboxes, err := infraInstance.SelectSandboxes(infra.SandboxSelectorOptions{WantPaused: true})
	assert.NoError(t, err)
	assert.Equal(t, 0, len(noSandboxes))
}
