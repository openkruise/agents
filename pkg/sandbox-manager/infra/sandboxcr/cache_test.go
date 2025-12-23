package sandboxcr

import (
	"context"
	"testing"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned/fake"
	informers "github.com/openkruise/agents/client/informers/externalversions"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

//goland:noinspection GoDeprecation
func NewTestCache() (cache *Cache, client *fake.Clientset) {
	client = fake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(client, time.Minute*10)
	sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
	sandboxSetInformer := informerFactory.Api().V1alpha1().SandboxSets().Informer()
	cache, err := NewCache(informerFactory, sandboxInformer, sandboxSetInformer)
	if err != nil {
		panic(err)
	}
	err = cache.Run(context.Background())
	if err != nil {
		panic(err)
	}
	return cache, client
}

func TestCache_WaitForSandboxSatisfied(t *testing.T) {
	utils.InitLogOutput()
	tests := []struct {
		name        string
		setupFunc   func(*testing.T, *Cache, *fake.Clientset) *agentsv1alpha1.Sandbox
		checkFunc   checkFunc
		timeout     time.Duration
		expectError string
	}{
		{
			name: "unsatisfied condition should timeout",
			setupFunc: func(t *testing.T, cache *Cache, client *fake.Clientset) *agentsv1alpha1.Sandbox {
				sandbox := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-1",
						Namespace: "default",
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxPending,
					},
				}
				CreateSandboxWithStatus(t, client, sandbox)
				return sandbox
			},
			checkFunc: func(sbx *agentsv1alpha1.Sandbox) (bool, error) {
				return false, nil
			},
			timeout:     100 * time.Millisecond,
			expectError: "double check failed",
		},
		{
			name: "check function returns error",
			setupFunc: func(t *testing.T, cache *Cache, client *fake.Clientset) *agentsv1alpha1.Sandbox {
				sandbox := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-2",
						Namespace: "default",
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxPending,
					},
				}
				CreateSandboxWithStatus(t, client, sandbox)
				return sandbox
			},
			checkFunc: func(sbx *agentsv1alpha1.Sandbox) (bool, error) {
				if sbx.ResourceVersion == "101" {
					return false, assert.AnError
				}
				return false, nil
			},
			timeout:     1 * time.Second,
			expectError: assert.AnError.Error(),
		},
		{
			name: "wait task conflict",
			setupFunc: func(t *testing.T, cache *Cache, client *fake.Clientset) *agentsv1alpha1.Sandbox {
				sandbox := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-1",
						Namespace: "default",
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxPending,
					},
				}
				CreateSandboxWithStatus(t, client, sandbox)
				go func() {
					_ = cache.WaitForSandboxSatisfied(t.Context(), sandbox, "another", func(sbx *agentsv1alpha1.Sandbox) (bool, error) {
						return false, nil // never satisfied
					}, time.Hour)
				}()
				return sandbox
			},
			checkFunc: func(sbx *agentsv1alpha1.Sandbox) (bool, error) {
				return false, nil
			},
			timeout:     1 * time.Second,
			expectError: "already exists",
		},
		{
			name: "sandbox satisfied after waiting",
			setupFunc: func(t *testing.T, cache *Cache, client *fake.Clientset) *agentsv1alpha1.Sandbox {
				sandbox := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-2",
						Namespace: "default",
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxPending,
					},
				}
				CreateSandboxWithStatus(t, client, sandbox)
				return sandbox
			},
			checkFunc: func(sbx *agentsv1alpha1.Sandbox) (bool, error) {
				return sbx.ResourceVersion == "101", nil // this update will always be made
			},
			timeout:     1 * time.Second,
			expectError: "",
		},
		{
			name: "sandbox already satisfied",
			setupFunc: func(t *testing.T, cache *Cache, client *fake.Clientset) *agentsv1alpha1.Sandbox {
				sandbox := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-2",
						Namespace: "default",
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxPending,
					},
				}
				CreateSandboxWithStatus(t, client, sandbox)
				return sandbox
			},
			checkFunc: func(sbx *agentsv1alpha1.Sandbox) (bool, error) {
				return true, nil
			},
			timeout:     1 * time.Second,
			expectError: "",
		},
		{
			name: "return error without waiting",
			setupFunc: func(t *testing.T, cache *Cache, client *fake.Clientset) *agentsv1alpha1.Sandbox {
				sandbox := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-2",
						Namespace: "default",
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxPending,
					},
				}
				CreateSandboxWithStatus(t, client, sandbox)
				return sandbox
			},
			checkFunc: func(sbx *agentsv1alpha1.Sandbox) (bool, error) {
				return false, assert.AnError
			},
			timeout:     1 * time.Second,
			expectError: assert.AnError.Error(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache, client := NewTestCache()
			defer cache.Stop()

			// Setup test sandbox
			sandbox := tt.setupFunc(t, cache, client)
			time.Sleep(10 * time.Millisecond)

			go func() {
				time.Sleep(50 * time.Millisecond)
				sandbox.ResourceVersion = "101"
				_, err := client.ApiV1alpha1().Sandboxes("default").Update(context.Background(), sandbox, metav1.UpdateOptions{})
				assert.NoError(t, err)
			}()

			// Call WaitForSandboxSatisfied
			err := cache.WaitForSandboxSatisfied(t.Context(), sandbox, "", tt.checkFunc, tt.timeout)

			// Check results
			if tt.expectError != "" {
				assert.Error(t, err)
				if err != nil {
					assert.Contains(t, err.Error(), tt.expectError)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
