package k8s

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestGetDeployment(t *testing.T) {
	// 创建fake client set
	client := fake.NewClientset()

	// 创建cache
	cache, err := NewCache(client, "default")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// 启动缓存
	done := make(chan struct{})
	go cache.Run(done)
	<-done

	// 测试用例
	tests := []struct {
		name        string
		deployment  *appsv1.Deployment
		lookupName  string
		expectError bool
	}{
		{
			name: "existing deployment",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deployment",
					Namespace: "default",
				},
			},
			lookupName:  "test-deployment",
			expectError: false,
		},
		{
			name: "non-existing deployment",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deployment",
					Namespace: "default",
				},
			},
			lookupName:  "non-existing-deployment",
			expectError: true,
		},
		{
			name: "deployment in different namespace",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deployment",
					Namespace: "other-namespace",
				},
			},
			lookupName:  "test-deployment",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 添加deployment到fake client
			if tt.deployment != nil {
				_, err := client.AppsV1().Deployments(tt.deployment.Namespace).Create(
					context.Background(), tt.deployment, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create deployment: %v", err)
				}

				// 等待informer同步
				time.Sleep(100 * time.Millisecond)
			}

			// 测试GetDeployment
			_, err := cache.GetDeployment(tt.lookupName)

			if tt.expectError {
				if err == nil {
					t.Error("Expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
			}

			// 清理deployment
			if tt.deployment != nil {
				_ = client.AppsV1().Deployments(tt.deployment.Namespace).Delete(
					context.Background(), tt.deployment.Name, metav1.DeleteOptions{})
				time.Sleep(100 * time.Millisecond)
			}
		})
	}

	// 停止缓存
	cache.Stop()
}

func TestSelectPods(t *testing.T) {
	// 创建fake client set
	client := fake.NewClientset()

	// 创建cache
	cache, err := NewCache(client, "default")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// 启动缓存
	done := make(chan struct{})
	go cache.Run(done)
	<-done

	// 创建测试用的pods
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

	// 添加pods到fake client
	for _, pod := range pods {
		_, err := client.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create pod %s: %v", pod.Name, err)
		}
	}

	// 等待informer同步
	time.Sleep(100 * time.Millisecond)

	// 测试用例
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
			pods, err := cache.SelectPods(tt.labels...)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			if len(pods) != tt.expectedCount {
				t.Errorf("Expected %d pods, got %d", tt.expectedCount, len(pods))
			}

			// 验证所有返回的pods都在default命名空间中
			for _, pod := range pods {
				if pod.Namespace != "default" {
					t.Errorf("Expected pod in 'default' namespace, got %s", pod.Namespace)
				}
			}
		})
	}

	// 停止缓存
	cache.Stop()
}

func TestGetPod(t *testing.T) {
	// 创建fake client set
	client := fake.NewClientset()

	// 创建cache
	cache, err := NewCache(client, "default")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// 启动缓存
	done := make(chan struct{})
	go cache.Run(done)
	<-done

	// 测试用例
	tests := []struct {
		name        string
		pod         *corev1.Pod
		lookupName  string
		expectError bool
	}{
		{
			name: "existing pod",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
			},
			lookupName:  "test-pod",
			expectError: false,
		},
		{
			name: "non-existing pod",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
			},
			lookupName:  "non-existing-pod",
			expectError: true,
		},
		{
			name: "pod in different namespace",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "other-namespace",
				},
			},
			lookupName:  "test-pod",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 添加pod到fake client
			if tt.pod != nil {
				_, err := client.CoreV1().Pods(tt.pod.Namespace).Create(
					context.Background(), tt.pod, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create pod: %v", err)
				}

				// 等待informer同步
				time.Sleep(100 * time.Millisecond)
			}

			// 测试GetPod
			_, err := cache.GetPod(tt.lookupName)

			if tt.expectError {
				if err == nil {
					t.Error("Expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
			}

			// 清理pod
			if tt.pod != nil {
				_ = client.CoreV1().Pods(tt.pod.Namespace).Delete(
					context.Background(), tt.pod.Name, metav1.DeleteOptions{})
				time.Sleep(100 * time.Millisecond)
			}
		})
	}

	// 停止缓存
	cache.Stop()
}

func TestCache_GetAllPods(t *testing.T) {
	// 创建fake client set
	client := fake.NewClientset()

	// 创建cache
	cache, err := NewCache(client, "") // 使用空字符串作为namespace，这样可以获取所有命名空间的sandboxes
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// 启动缓存
	done := make(chan struct{})
	go cache.Run(done)
	<-done

	// 创建测试用的sandboxes
	sandboxes := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sandbox1",
				Namespace: "default",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sandbox2",
				Namespace: "default",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sandbox3",
				Namespace: "other-namespace",
			},
		},
	}

	// 添加sandboxes到fake client
	for _, sandbox := range sandboxes {
		_, err := client.CoreV1().Pods(sandbox.Namespace).Create(context.Background(), sandbox, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create sandbox %s: %v", sandbox.Name, err)
		}
	}

	// 等待informer同步
	time.Sleep(100 * time.Millisecond)

	// 测试用例
	tests := []struct {
		name          string
		namespace     string
		expectedCount int
		expectError   bool
	}{
		{
			name:          "get all sandboxes in default namespace",
			namespace:     "default",
			expectedCount: 2, // sandbox1 and sandbox2
			expectError:   false,
		},
		{
			name:          "get all sandboxes in other namespace",
			namespace:     "other-namespace",
			expectedCount: 1, // sandbox3
			expectError:   false,
		},
		{
			name:          "get all sandboxes in non-existing namespace",
			namespace:     "non-existing",
			expectedCount: 0,
			expectError:   false,
		},
		{
			name:          "get all sandboxes with empty namespace",
			namespace:     "",
			expectedCount: 0,
			expectError:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandboxes, err := cache.GetAllPods(tt.namespace)

			if tt.expectError {
				if err == nil {
					t.Error("Expected error but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if len(sandboxes) != tt.expectedCount {
				t.Errorf("Expected %d sandboxes, got %d", tt.expectedCount, len(sandboxes))
			}

			// 验证所有返回的sandboxes都在指定的命名空间中
			for _, sandbox := range sandboxes {
				if sandbox.Namespace != tt.namespace {
					t.Errorf("Expected sandbox in '%s' namespace, got %s", tt.namespace, sandbox.Namespace)
				}
			}
		})
	}

	// 停止缓存
	cache.Stop()
}
