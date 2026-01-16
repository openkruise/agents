package sandboxcr

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sinformers "k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	cacheForClientgo "k8s.io/client-go/tools/cache"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned/fake"
	informers "github.com/openkruise/agents/client/informers/externalversions"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
)

//goland:noinspection GoDeprecation
func NewTestCache(t *testing.T, enableCoreResources ...bool) (*Cache, *k8sfake.Clientset, *fake.Clientset) {
	enableCore := false
	if len(enableCoreResources) > 0 {
		enableCore = enableCoreResources[0]
	}
	var k8sClient *k8sfake.Clientset
	var coreInformerFactory k8sinformers.SharedInformerFactory
	var persistentVolumeInformer, secretInformer cacheForClientgo.SharedIndexInformer

	if enableCore {
		k8sClient = k8sfake.NewSimpleClientset()
		coreInformerFactory = k8sinformers.NewSharedInformerFactory(k8sClient, time.Minute*10)
		persistentVolumeInformer = coreInformerFactory.Core().V1().PersistentVolumes().Informer()
		secretInformer = coreInformerFactory.Core().V1().Secrets().Informer()
	}
	// Initialize sandbox cache with all required informers
	sandboxClient := fake.NewSimpleClientset()
	sandboxInformerFactory := informers.NewSharedInformerFactory(sandboxClient, time.Minute*10)
	sandboxInformer := sandboxInformerFactory.Api().V1alpha1().Sandboxes().Informer()
	sandboxSetInformer := sandboxInformerFactory.Api().V1alpha1().SandboxSets().Informer()

	var cache *Cache
	var err error

	if enableCore {
		cache, err = NewCache(sandboxInformerFactory, sandboxInformer, sandboxSetInformer,
			coreInformerFactory, persistentVolumeInformer, secretInformer)
	} else {
		cache, err = NewCache(sandboxInformerFactory, sandboxInformer, sandboxSetInformer, nil)
	}
	assert.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = cache.Run(ctx)
	}()

	time.Sleep(200 * time.Millisecond)

	return cache, k8sClient, sandboxClient
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
			cache, _, client := NewTestCache(t)
			defer cache.Stop()

			// Setup test sandbox
			sandbox := tt.setupFunc(t, cache, client)
			time.Sleep(10 * time.Millisecond)

			go func() {
				time.Sleep(50 * time.Millisecond)
				gotSbx, err := client.ApiV1alpha1().Sandboxes(sandbox.Namespace).Get(t.Context(), sandbox.Name, metav1.GetOptions{})
				assert.NoError(t, err)
				gotSbx.ResourceVersion = "101"
				_, err = client.ApiV1alpha1().Sandboxes(sandbox.Namespace).Update(context.Background(), gotSbx, metav1.UpdateOptions{})
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

func TestCache_GetPersistentVolume(t *testing.T) {
	utils.InitLogOutput()
	tests := []struct {
		name           string
		setupPV        func() *corev1.PersistentVolume
		pvName         string
		expectFound    bool
		expectError    bool
		expectErrorMsg string
	}{
		{
			name: "get existing persistent volume from cache",
			setupPV: func() *corev1.PersistentVolume {
				return &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-pv-existing",
					},
					Spec: corev1.PersistentVolumeSpec{
						Capacity: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("10Gi"),
						},
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteOnce,
						},
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: "/tmp/test-pv",
							},
						},
					},
				}
			},
			pvName:      "test-pv-existing",
			expectFound: true,
			expectError: false,
		},
		{
			name: "get non-existing persistent volume - fallback to api server",
			setupPV: func() *corev1.PersistentVolume {
				return &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-pv-another",
					},
					Spec: corev1.PersistentVolumeSpec{
						Capacity: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("10Gi"),
						},
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteOnce,
						},
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: "/tmp/test-pv",
							},
						},
					},
				}
			},
			pvName:         "non-existing-pv",
			expectFound:    false,
			expectError:    true,
			expectErrorMsg: "not found in cache",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache, k8sClient, _ := NewTestCache(t, true)
			defer cache.Stop()

			if tt.setupPV != nil {
				testPV := tt.setupPV()
				_, err := k8sClient.CoreV1().PersistentVolumes().Create(context.TODO(), testPV, metav1.CreateOptions{})
				assert.NoError(t, err)
				// Wait for informer sync
				time.Sleep(300 * time.Millisecond)
			}
			// Try to get the resource from cache
			result, err := cache.GetPersistentVolume(tt.pvName)

			if tt.expectFound {
				if tt.expectError {
					assert.Error(t, err)
					if tt.expectErrorMsg != "" {
						assert.Contains(t, err.Error(), tt.expectErrorMsg)
					}
				} else {
					assert.NoError(t, err)
					assert.NotNil(t, result)
					assert.Equal(t, tt.pvName, result.Name)
					expectedPV := tt.setupPV()
					assert.Equal(t, expectedPV.Spec.Capacity, result.Spec.Capacity)
					assert.Equal(t, expectedPV.Spec.AccessModes, result.Spec.AccessModes)
				}
			} else {
				if err != nil && !tt.expectError {
					directResult, directErr := k8sClient.CoreV1().PersistentVolumes().Get(context.TODO(), tt.pvName, metav1.GetOptions{})
					if directErr != nil {
						assert.Contains(t, err.Error(), "not found in cache")
					} else {
						t.Logf("Resource exists in API server but not in cache: %s", tt.pvName)
						assert.NotEmpty(t, directResult.Name, "Direct API result should have name")
					}
				} else if tt.expectError {
					assert.Error(t, err)
					if tt.expectErrorMsg != "" {
						assert.Contains(t, err.Error(), tt.expectErrorMsg)
					}
				}
			}
		})
	}
}

func TestCache_GetPersistentVolume_FromSync(t *testing.T) {
	utils.InitLogOutput()
	cache, k8sClient, _ := NewTestCache(t, true)
	defer cache.Stop()

	testPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pv-sync",
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("10Gi"),
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/tmp/test-pv",
				},
			},
		},
	}
	_, err := k8sClient.CoreV1().PersistentVolumes().Create(context.TODO(), testPV, metav1.CreateOptions{})
	assert.NoError(t, err)
	// Wait for cache to be ready
	time.Sleep(300 * time.Millisecond)
	// Verify that the Pv is found in cache
	result, err := cache.GetPersistentVolume("test-pv-sync")
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, "test-pv-sync", result.Name)
	assert.Equal(t, testPV.Spec.Capacity, result.Spec.Capacity)
	assert.Equal(t, testPV.Spec.AccessModes, result.Spec.AccessModes)
}

func TestCache_GetSecret_FromSync(t *testing.T) {
	utils.InitLogOutput()

	cache, k8sClient, _ := NewTestCache(t, true)
	defer cache.Stop()

	testSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret-sync",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"username": []byte("admin"),
			"password": []byte("password123"),
		},
		Type: corev1.SecretTypeOpaque,
	}
	// Create the secret in the cluster using the client
	_, err := k8sClient.CoreV1().Secrets("default").Create(context.TODO(), testSecret, metav1.CreateOptions{})
	assert.NoError(t, err)
	// Wait for cache to be ready
	time.Sleep(300 * time.Millisecond)
	// Verify that the secret is found in cache
	result, err := cache.GetSecret("default", "test-secret-sync")
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, "test-secret-sync", result.Name)
	assert.Equal(t, "default", result.Namespace)
	assert.Equal(t, testSecret.Data, result.Data)
	assert.Equal(t, testSecret.Type, result.Type)
}
