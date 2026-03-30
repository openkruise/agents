package sandboxcr

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	sandboxfake "github.com/openkruise/agents/client/clientset/versioned/fake"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	constantUtils "github.com/openkruise/agents/pkg/utils"
	sandboxManagerUtils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
)

func TestCache_WaitForSandboxSatisfied(t *testing.T) {
	sandboxManagerUtils.InitLogOutput()
	tests := []struct {
		name        string
		setupFunc   func(*testing.T, *Cache, clients.SandboxClient) *agentsv1alpha1.Sandbox
		checkFunc   checkFunc[*agentsv1alpha1.Sandbox]
		timeout     time.Duration
		expectError string
	}{
		{
			name: "unsatisfied condition should timeout",
			setupFunc: func(t *testing.T, cache *Cache, client clients.SandboxClient) *agentsv1alpha1.Sandbox {
				sandbox := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-1",
						Namespace: "default",
						Labels: map[string]string{
							agentsv1alpha1.LabelSandboxIsClaimed: "true",
						},
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
			expectError: "sandbox is not satisfied during double check",
		},
		{
			name: "check function returns error",
			setupFunc: func(t *testing.T, cache *Cache, client clients.SandboxClient) *agentsv1alpha1.Sandbox {
				sandbox := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-2",
						Namespace: "default",
						Labels: map[string]string{
							agentsv1alpha1.LabelSandboxIsClaimed: "true",
						},
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
			setupFunc: func(t *testing.T, cache *Cache, client clients.SandboxClient) *agentsv1alpha1.Sandbox {
				sandbox := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-1",
						Namespace: "default",
						Labels: map[string]string{
							agentsv1alpha1.LabelSandboxIsClaimed: "true",
						},
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
			setupFunc: func(t *testing.T, cache *Cache, client clients.SandboxClient) *agentsv1alpha1.Sandbox {
				sandbox := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-2",
						Namespace: "default",
						Labels: map[string]string{
							agentsv1alpha1.LabelSandboxIsClaimed: "true",
						},
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
			setupFunc: func(t *testing.T, cache *Cache, client clients.SandboxClient) *agentsv1alpha1.Sandbox {
				sandbox := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-2",
						Namespace: "default",
						Labels: map[string]string{
							agentsv1alpha1.LabelSandboxIsClaimed: "true",
						},
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
			setupFunc: func(t *testing.T, cache *Cache, client clients.SandboxClient) *agentsv1alpha1.Sandbox {
				sandbox := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-2",
						Namespace: "default",
						Labels: map[string]string{
							agentsv1alpha1.LabelSandboxIsClaimed: "true",
						},
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
		{
			name: "fallback to apiserver when sandbox not in cache",
			setupFunc: func(t *testing.T, cache *Cache, client clients.SandboxClient) *agentsv1alpha1.Sandbox {
				// Create a sandbox with LabelSandboxIsClaimed = "false"
				// This sandbox will NOT be indexed by IndexClaimedSandboxID
				// so GetClaimedSandbox will fail and trigger apiserver fallback
				sandbox := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox-not-claimed",
						Namespace: "default",
						Labels: map[string]string{
							agentsv1alpha1.LabelSandboxIsClaimed: "false", // Not indexed
						},
					},
					Status: agentsv1alpha1.SandboxStatus{
						Phase: agentsv1alpha1.SandboxPending, // Start as Pending
					},
				}
				CreateSandboxWithStatus(t, client, sandbox)
				// Wait for informer to sync, but it won't be in claimed index
				time.Sleep(50 * time.Millisecond)

				// Update sandbox to Running in background after a short delay
				go func() {
					time.Sleep(100 * time.Millisecond)
					gotSbx, err := client.ApiV1alpha1().Sandboxes(sandbox.Namespace).Get(t.Context(), sandbox.Name, metav1.GetOptions{})
					require.NoError(t, err)
					gotSbx.Status.Phase = agentsv1alpha1.SandboxRunning
					gotSbx.Status.Conditions = []metav1.Condition{
						{
							Type:   string(agentsv1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					}
					_, err = client.ApiV1alpha1().Sandboxes(sandbox.Namespace).UpdateStatus(context.Background(), gotSbx, metav1.UpdateOptions{})
					assert.NoError(t, err)
				}()

				return sandbox
			},
			checkFunc: func(sbx *agentsv1alpha1.Sandbox) (bool, error) {
				// Check if sandbox is running and ready
				return sbx.Status.Phase == agentsv1alpha1.SandboxRunning &&
					sandboxutils.IsSandboxReady(sbx), nil
			},
			timeout:     2 * time.Second,
			expectError: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache, clientSet, err := NewTestCache(t)
			defer cache.Stop()
			require.NoError(t, err)

			// Setup test sandbox
			sandbox := tt.setupFunc(t, cache, clientSet.SandboxClient)
			time.Sleep(10 * time.Millisecond)

			go func() {
				time.Sleep(50 * time.Millisecond)
				gotSbx, err := clientSet.ApiV1alpha1().Sandboxes(sandbox.Namespace).Get(t.Context(), sandbox.Name, metav1.GetOptions{})
				require.NoError(t, err)
				gotSbx.ResourceVersion = "101"
				_, err = clientSet.ApiV1alpha1().Sandboxes(sandbox.Namespace).Update(context.Background(), gotSbx, metav1.UpdateOptions{})
				assert.NoError(t, err)
			}()

			// Call WaitForSandboxSatisfied
			err = cache.WaitForSandboxSatisfied(t.Context(), sandbox, "", tt.checkFunc, tt.timeout)

			// Check results
			if tt.expectError != "" {
				require.Error(t, err)
				if err != nil {
					assert.Contains(t, err.Error(), tt.expectError)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestCache_WaitForSandboxSatisfied_Cancel(t *testing.T) {
	sandboxManagerUtils.InitLogOutput()
	cache, clientSet, err := NewTestCache(t)
	require.NoError(t, err)
	defer cache.Stop()
	sandboxClient := clientSet.SandboxClient
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox-1",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxIsClaimed: "true",
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxPending,
		},
	}
	CreateSandboxWithStatus(t, sandboxClient, sbx)
	EnsureSandboxInCache(t, cache, sbx)
	ctx1, cancel := context.WithCancel(t.Context())
	cancel()
	// never get satisfied or timeout
	err = cache.WaitForSandboxSatisfied(ctx1, sbx, "", func(sbx *agentsv1alpha1.Sandbox) (bool, error) {
		return false, nil
	}, time.Hour)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not satisfied")
	ctx2, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err = cache.WaitForSandboxSatisfied(ctx2, sbx, "", func(sbx *agentsv1alpha1.Sandbox) (bool, error) {
		return false, nil
	}, time.Hour)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not satisfied")
}

func TestCache_GetPersistentVolume(t *testing.T) {
	sandboxManagerUtils.InitLogOutput()
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
			cache, clientSet, err := NewTestCache(t)
			require.NoError(t, err)
			defer cache.Stop()
			k8sClient := clientSet.K8sClient

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
	cache, clientSet, err := NewTestCache(t)
	require.NoError(t, err)
	defer cache.Stop()
	k8sClient := clientSet.K8sClient

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
	_, err = k8sClient.CoreV1().PersistentVolumes().Create(context.TODO(), testPV, metav1.CreateOptions{})
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
	sandboxManagerUtils.InitLogOutput()

	cache, clientSet, err := NewTestCache(t)
	require.NoError(t, err)
	defer cache.Stop()
	k8sClient := clientSet.K8sClient

	testSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret-sync",
			Namespace: constantUtils.DefaultSandboxDeployNamespace,
		},
		Data: map[string][]byte{
			"username": []byte("admin"),
			"password": []byte("password123"),
		},
		Type: corev1.SecretTypeOpaque,
	}
	// Create the secret in the cluster using the client
	_, err = k8sClient.CoreV1().Secrets(constantUtils.DefaultSandboxDeployNamespace).Create(context.TODO(), testSecret, metav1.CreateOptions{})
	assert.NoError(t, err)
	// Wait for cache to be ready
	time.Sleep(300 * time.Millisecond)
	// Verify that the secret is found in cache
	result, err := cache.GetSecret(constantUtils.DefaultSandboxDeployNamespace, "test-secret-sync")
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, "test-secret-sync", result.Name)
	assert.Equal(t, constantUtils.DefaultSandboxDeployNamespace, result.Namespace)
	assert.Equal(t, testSecret.Data, result.Data)
	assert.Equal(t, testSecret.Type, result.Type)
}

func TestCache_GetConfigmap_FromSync(t *testing.T) {
	sandboxManagerUtils.InitLogOutput()

	cache, clientSet, err := NewTestCache(t)
	require.NoError(t, err)
	defer cache.Stop()
	k8sClient := clientSet.K8sClient

	testConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-configmap-sync",
			Namespace: constantUtils.DefaultSandboxDeployNamespace,
		},
		Data: map[string]string{
			"key1": "value1",
			"key2": "value2",
		},
	}
	// Create the configmap in the cluster using the client
	_, err = k8sClient.CoreV1().ConfigMaps(constantUtils.DefaultSandboxDeployNamespace).Create(context.TODO(), testConfigMap, metav1.CreateOptions{})
	assert.NoError(t, err)
	// Wait for cache to be ready
	time.Sleep(300 * time.Millisecond)
	// Verify that the configmap is found in cache
	result, err := cache.GetConfigmap(constantUtils.DefaultSandboxDeployNamespace, "test-configmap-sync")
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, "test-configmap-sync", result.Name)
	assert.Equal(t, constantUtils.DefaultSandboxDeployNamespace, result.Namespace)
	assert.Equal(t, testConfigMap.Data, result.Data)
}

func TestCache_InformerWithFilter_GetSandboxSet(t *testing.T) {
	sandboxManagerUtils.InitLogOutput()

	tests := []struct {
		name             string
		opts             config.SandboxManagerOptions
		seedSandboxSets  []*agentsv1alpha1.SandboxSet
		wantCachedNames  []string // expected SandboxSet names visible in cache (namespace/name)
		queryName        string   // template name to query via GetSandboxSet
		wantGetErr       bool     // whether GetSandboxSet should return an error
		wantGetName      string   // expected name of the returned SandboxSet (if no error)
		wantGetNamespace string   // expected namespace of the returned SandboxSet (if no error)
	}{
		{
			name: "no filter - all SandboxSets visible",
			opts: config.SandboxManagerOptions{},
			seedSandboxSets: []*agentsv1alpha1.SandboxSet{
				{ObjectMeta: metav1.ObjectMeta{Name: "tmpl-1", Namespace: "team-a"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "tmpl-2", Namespace: "team-a"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "tmpl-3", Namespace: "team-b"}},
			},
			wantCachedNames:  []string{"team-a/tmpl-1", "team-a/tmpl-2", "team-b/tmpl-3"},
			queryName:        "tmpl-3",
			wantGetErr:       false,
			wantGetName:      "tmpl-3",
			wantGetNamespace: "team-b",
		},
		{
			name: "namespace filter with multiple SandboxSets in same namespace",
			opts: config.SandboxManagerOptions{
				SandboxNamespace: "team-a",
			},
			seedSandboxSets: []*agentsv1alpha1.SandboxSet{
				{ObjectMeta: metav1.ObjectMeta{Name: "tmpl-1", Namespace: "team-a"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "tmpl-2", Namespace: "team-a"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "tmpl-3", Namespace: "team-b"}},
			},
			wantCachedNames:  []string{"team-a/tmpl-1", "team-a/tmpl-2"},
			queryName:        "tmpl-2",
			wantGetErr:       false,
			wantGetName:      "tmpl-2",
			wantGetNamespace: "team-a",
		},
		{
			name: "label selector filter with multiple SandboxSets",
			opts: config.SandboxManagerOptions{
				SandboxLabelSelector: "env=prod",
			},
			seedSandboxSets: []*agentsv1alpha1.SandboxSet{
				{ObjectMeta: metav1.ObjectMeta{Name: "tmpl-match-selector", Namespace: "team-a", Labels: map[string]string{"env": "prod"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "tmpl-not-match-selector", Namespace: "team-a", Labels: map[string]string{"env": "staging"}}},
			},
			wantCachedNames:  []string{"team-a/tmpl-match-selector"},
			queryName:        "tmpl-match-selector",
			wantGetErr:       false,
			wantGetName:      "tmpl-match-selector",
			wantGetNamespace: "team-a",
		},
		{
			name: "namespace and label selector filter with multiple SandboxSets",
			opts: config.SandboxManagerOptions{
				SandboxNamespace:     "team-a",
				SandboxLabelSelector: "env=prod",
			},
			seedSandboxSets: []*agentsv1alpha1.SandboxSet{
				{ObjectMeta: metav1.ObjectMeta{Name: "tmpl-match-selector", Namespace: "team-a", Labels: map[string]string{"env": "prod"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "tmpl-not-match-selector", Namespace: "team-a", Labels: map[string]string{"env": "staging"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "tmpl-match-selector", Namespace: "team-b", Labels: map[string]string{"env": "prod"}}},
			},
			wantCachedNames:  []string{"team-a/tmpl-match-selector"},
			queryName:        "tmpl-match-selector",
			wantGetErr:       false,
			wantGetName:      "tmpl-match-selector",
			wantGetNamespace: "team-a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				c         *Cache
				clientSet *clients.ClientSet
				err       error
			)

			if tt.opts.SandboxLabelSelector != "" {
				// When a label selector is configured, the fake client does not enforce it
				// natively. We work around this by:
				//   1. Creating the fake clientSet manually so we can access the underlying
				//      *sandboxfake.Clientset and inject a PrependReactor.
				//   2. Writing seed data into the Tracker BEFORE starting the Cache so that
				//      the Informer's initial List call goes through our reactor and returns
				//      only the label-matching objects.
				//   3. The reactor parses the LabelSelector from ListOptions and filters the
				//      objects returned by the Tracker, giving the Informer an accurate view.
				clientSet = clients.NewFakeClientSet(t)
				fakeClient, ok := clientSet.SandboxClient.(*sandboxfake.Clientset)
				require.True(t, ok, "SandboxClient must be *sandboxfake.Clientset")

				// Write seed data into the Tracker before the Cache starts.
				for _, sbs := range tt.seedSandboxSets {
					require.NoError(t,
						fakeClient.Tracker().Add(sbs),
						"failed to add SandboxSet %s/%s to tracker", sbs.Namespace, sbs.Name,
					)
				}

				// Inject a PrependReactor that enforces label selector filtering on List.
				fakeClient.PrependReactor("list", "sandboxsets", func(action k8stesting.Action) (bool, runtime.Object, error) {
					listAction, ok := action.(k8stesting.ListAction)
					if !ok {
						return false, nil, nil
					}
					// Retrieve all objects from the Tracker for this resource/namespace.
					objs, err := fakeClient.Tracker().List(
						agentsv1alpha1.SchemeGroupVersion.WithResource("sandboxsets"),
						agentsv1alpha1.SchemeGroupVersion.WithKind("SandboxSet"),
						listAction.GetNamespace(),
					)
					if err != nil {
						return true, nil, err
					}
					rawList, ok := objs.(*agentsv1alpha1.SandboxSetList)
					if !ok {
						return false, nil, nil
					}

					// Apply label selector filtering when one is present.
					restriction := listAction.GetListRestrictions()
					selector := restriction.Labels
					if selector == nil || selector.Empty() {
						return true, rawList, nil
					}
					filtered := make([]agentsv1alpha1.SandboxSet, 0, len(rawList.Items))
					for _, item := range rawList.Items {
						if selector.Matches(labels.Set(item.Labels)) {
							filtered = append(filtered, item)
						}
					}
					rawList.Items = filtered
					return true, rawList, nil
				})

				// Build and start the Cache with the pre-seeded, reactor-equipped clientSet.
				c, err = NewCache(clientSet, tt.opts)
				require.NoError(t, err)
				require.NoError(t, c.Run(t.Context()))
			} else {
				c, clientSet, err = NewTestCacheWithOptions(t, tt.opts)
				require.NoError(t, err)

				// Seed SandboxSets after the Cache is running (standard path).
				for _, sbs := range tt.seedSandboxSets {
					_, err := clientSet.SandboxClient.ApiV1alpha1().SandboxSets(sbs.Namespace).Create(
						t.Context(), sbs, metav1.CreateOptions{},
					)
					require.NoError(t, err, "failed to create SandboxSet %s/%s", sbs.Namespace, sbs.Name)
				}
			}
			defer c.Stop()

			// Wait for informer sync
			time.Sleep(300 * time.Millisecond)

			// Inspect all SandboxSets in cache
			allItems := c.sandboxSetInformer.GetStore().List()
			cachedKeys := make([]string, 0, len(allItems))
			for _, item := range allItems {
				sbs, ok := item.(*agentsv1alpha1.SandboxSet)
				require.True(t, ok, "unexpected type in sandboxSetInformer store: %T", item)
				cachedKeys = append(cachedKeys, sbs.Namespace+"/"+sbs.Name)
			}
			assert.ElementsMatch(t, tt.wantCachedNames, cachedKeys,
				"cached SandboxSet keys mismatch")

			// Query via GetSandboxSet
			got, err := c.GetSandboxSet(tt.queryName)
			if tt.wantGetErr {
				require.Error(t, err)
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				require.NotNil(t, got)
				assert.Equal(t, tt.wantGetName, got.Name)
				assert.Equal(t, tt.wantGetNamespace, got.Namespace)
			}
		})
	}
}
