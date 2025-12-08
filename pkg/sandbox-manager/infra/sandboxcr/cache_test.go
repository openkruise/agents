package sandboxcr

import (
	"context"
	"testing"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned/fake"
	informers "github.com/openkruise/agents/client/informers/externalversions"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

//goland:noinspection GoDeprecation
func TestCache_SelectSandboxes(t *testing.T) {
	// Create fake client set
	client := fake.NewSimpleClientset()

	// Create cache
	informerFactory := informers.NewSharedInformerFactoryWithOptions(client, time.Minute*10, informers.WithNamespace("default"))
	sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
	cache, err := NewCache[*v1alpha1.Sandbox]("default", informerFactory, sandboxInformer)
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}
	// Start cache
	done := make(chan struct{})
	go cache.Run(done)
	<-done
	// Create test pods
	sandboxes := []*v1alpha1.Sandbox{
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
	for _, sandbox := range sandboxes {
		_, err := client.ApiV1alpha1().Sandboxes(sandbox.Namespace).Create(context.Background(), sandbox, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create sandbox %s: %v", sandbox.Name, err)
		}
	}

	// Wait for informer sync
	t.Log("waiting informer synced")
	start := time.Now()
	for {
		time.Sleep(100 * time.Millisecond)
		_, err := cache.GetSandbox("pod1")
		if err == nil {
			break
		}
		t.Logf("cannot get pod1 from cache: %v", err)
		if time.Since(start) > time.Second {
			t.Fatalf("timeout waiting for informer to sync")
		}
	}

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
			pods, err := cache.SelectSandboxes(tt.labels...)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			if len(pods) != tt.expectedCount {
				t.Errorf("Expected %d sandboxes, got %d", tt.expectedCount, len(pods))
			}

			// Verify all returned pods are in the default namespace
			for _, pod := range pods {
				if pod.Namespace != "default" {
					t.Errorf("Expected sandbox in 'default' namespace, got %s", pod.Namespace)
				}
			}
		})
	}

	// Stop cache
	cache.Stop()
}

//goland:noinspection GoDeprecation
func TestCache_GetSandbox(t *testing.T) {
	// Create fake client set
	client := fake.NewSimpleClientset()

	// Create cache
	informerFactory := informers.NewSharedInformerFactoryWithOptions(client, time.Minute*10, informers.WithNamespace("default"))
	sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
	cache, err := NewCache[*v1alpha1.Sandbox]("default", informerFactory, sandboxInformer)
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// Start cache
	done := make(chan struct{})
	go cache.Run(done)
	<-done

	// Test cases
	tests := []struct {
		name        string
		sandbox     *v1alpha1.Sandbox
		lookupName  string
		expectError bool
	}{
		{
			name: "existing sandbox",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
			},
			lookupName:  "test-sandbox",
			expectError: false,
		},
		{
			name: "non-existing sandbox",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
			},
			lookupName:  "non-existing-sandbox",
			expectError: true,
		},
		{
			name: "sandbox in different namespace",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "other-namespace",
				},
			},
			lookupName:  "test-sandbox",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Add pod to fake client
			if tt.sandbox != nil {
				_, err := client.ApiV1alpha1().Sandboxes(tt.sandbox.Namespace).Create(
					context.Background(), tt.sandbox, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create sandbox: %v", err)
				}

				// Wait for informer sync
				time.Sleep(100 * time.Millisecond)
			}

			// Test GetPod
			_, err := cache.GetSandbox(tt.lookupName)

			if tt.expectError {
				if err == nil {
					t.Error("Expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
			}

			// Clean up pod
			if tt.sandbox != nil {
				_ = client.ApiV1alpha1().Sandboxes(tt.sandbox.Namespace).Delete(
					context.Background(), tt.sandbox.Name, metav1.DeleteOptions{})
				time.Sleep(100 * time.Millisecond)
			}
		})
	}

	// Stop cache
	cache.Stop()
}
