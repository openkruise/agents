package utils

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSelectPods(t *testing.T) {
	client := fake.NewClientset()
	informerFactory := informers.NewSharedInformerFactoryWithOptions(client, time.Minute*10, informers.WithNamespace("default"))
	podInformer := informerFactory.Core().V1().Pods().Informer()
	stop := make(chan struct{})
	informerFactory.Start(stop)
	defer close(stop)

	err := AddLabelSelectorIndexerToInformer[*corev1.Pod](podInformer)
	if err != nil {
		t.Fatalf("Failed to add label selector indexer: %v", err)
	}

	// Create test pods
	pods := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod1",
				Namespace: "default",
				Labels: map[string]string{
					"app": "test",
					"env": "dev",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod2",
				Namespace: "default",
				Labels: map[string]string{
					"app": "test",
					"env": "prod",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod3",
				Namespace: "default",
				Labels: map[string]string{
					"app": "other",
					"env": "dev",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod4",
				Namespace: "other-namespace",
				Labels: map[string]string{
					"app": "test",
					"env": "dev",
				},
			},
		},
	}

	// Add pods to fake client
	for _, pod := range pods {
		_, err := client.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create pod %s: %v", pod.Name, err)
		}
	}

	// Wait for informer sync
	time.Sleep(100 * time.Millisecond)

	// Test cases
	tests := []struct {
		name          string
		labels        []string
		expectedCount int
	}{
		{
			name:          "select by single label app=test",
			labels:        []string{"app", "test"},
			expectedCount: 2, // pod1 and pod2
		},
		{
			name:          "select by single label env=dev",
			labels:        []string{"env", "dev"},
			expectedCount: 2, // pod1 and pod3
		},
		{
			name:          "select by two labels app=test env=dev",
			labels:        []string{"app", "test", "env", "dev"},
			expectedCount: 1, // pod1 only
		},
		{
			name:          "select by non-existing label",
			labels:        []string{"app", "non-existing"},
			expectedCount: 0,
		},
		{
			name:          "select with odd number of label arguments",
			labels:        []string{"app"},
			expectedCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pods, err := SelectObjectFromInformerWithLabelSelector[*corev1.Pod](podInformer, tt.labels...)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			if len(pods) != tt.expectedCount {
				t.Errorf("Expected %d pods, got %d", tt.expectedCount, len(pods))
			}

			// Verify all returned pods are in the default namespace
			for _, pod := range pods {
				if pod.Namespace != "default" {
					t.Errorf("Expected pod in 'default' namespace, got %s", pod.Namespace)
				}
			}
		})
	}
}
