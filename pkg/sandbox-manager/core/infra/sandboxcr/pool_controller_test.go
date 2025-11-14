package sandboxcr

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned"
	"github.com/openkruise/agents/client/clientset/versioned/fake"
	informers "github.com/openkruise/agents/client/informers/externalversions"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

var updatedHash = "updated-hash"
var legacyHash = "legacy-hash"

func getBaseSandbox(idx int32) *v1alpha1.Sandbox {
	return &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-" + strconv.Itoa(int(idx)),
			Namespace: "default",
			Labels: map[string]string{
				consts.LabelSandboxID:    "sandbox-" + strconv.Itoa(int(idx)),
				consts.LabelTemplateHash: updatedHash,
				consts.LabelSandboxPool:  "test-template",
			},
		},
	}
}

func CreateSandboxWithStatus(t *testing.T, client versioned.Interface, sbx *v1alpha1.Sandbox) {
	ctx := context.Background()
	_, err := client.ApiV1alpha1().Sandboxes("default").Create(ctx, sbx, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = client.ApiV1alpha1().Sandboxes("default").UpdateStatus(ctx, sbx, metav1.UpdateOptions{})
	assert.NoError(t, err)
}

//goland:noinspection GoDeprecation
func TestPool_Reconcile(t *testing.T) {
	utils.InitKLogOutput()
	tests := []struct {
		name                    string
		specReplicas            int32
		createCreatingSandboxes int32
		createPendingSandboxes  int32
		createClaimedSandboxes  int32
		createFailedSandboxes   int32
		createLegacySandboxes   int32
		createDeletedSandboxes  int32
		createKilledSandboxes   int32
		expectTotalSandboxes    int32
		expectNewSandboxes      int32
		expectRemovedSandboxes  int32
	}{
		{
			name:                 "simple scale up from 0 to 1",
			specReplicas:         1,
			expectTotalSandboxes: 1,
			expectNewSandboxes:   1,
		},
		{
			name:                   "1 claimed, scale up from 1 to 2",
			specReplicas:           2,
			createClaimedSandboxes: 1,
			expectTotalSandboxes:   2,
			expectNewSandboxes:     1,
		},
		{
			name:                   "1 pending, scale up from 1 to 2",
			specReplicas:           2,
			createPendingSandboxes: 1,
			expectTotalSandboxes:   2,
			expectNewSandboxes:     1,
		},
		{
			name:                   "1 legacy, scale up from 1 to 2, 1 gc",
			specReplicas:           2,
			createLegacySandboxes:  1,
			expectTotalSandboxes:   2,
			expectNewSandboxes:     2,
			expectRemovedSandboxes: 1,
		},
		{
			name:                   "1 deleted, scale up from 1 to 2, 1 gc",
			specReplicas:           2,
			createDeletedSandboxes: 1,
			expectTotalSandboxes:   2,
			expectNewSandboxes:     2,
			expectRemovedSandboxes: 1,
		},
		{
			name:                   "1 killed, scale up from 1 to 2, 1 gc",
			specReplicas:           2,
			createKilledSandboxes:  1,
			expectTotalSandboxes:   2,
			expectNewSandboxes:     2,
			expectRemovedSandboxes: 1,
		},
		{
			name:                   "simple scale down from 2 to 1",
			specReplicas:           1,
			createPendingSandboxes: 2,
			expectTotalSandboxes:   1,
			expectNewSandboxes:     0,
			expectRemovedSandboxes: 1,
		},
		{
			name:                   "scale down from 2 to 1, but no pending",
			specReplicas:           1,
			createClaimedSandboxes: 2,
			expectTotalSandboxes:   2, // there's no pending sandboxes to be deleted
			expectNewSandboxes:     0,
			expectRemovedSandboxes: 0,
		},
		{
			name:                   "not scaled but gc",
			specReplicas:           12,
			createPendingSandboxes: 2,
			createClaimedSandboxes: 2,
			createFailedSandboxes:  2, // should gc
			createKilledSandboxes:  2, // should gc
			createLegacySandboxes:  2, // should gc
			createDeletedSandboxes: 2, // should gc
			expectTotalSandboxes:   12,
			expectNewSandboxes:     8,
			expectRemovedSandboxes: 8,
		},
		{
			name:                    "scale down from 6 -> 2 and gc (remove creating)",
			specReplicas:            2,
			createCreatingSandboxes: 2,
			createClaimedSandboxes:  2,
			createFailedSandboxes:   2, // should gc
			expectTotalSandboxes:    2,
			expectNewSandboxes:      0,
			expectRemovedSandboxes:  4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			client := fake.NewSimpleClientset()
			var idx int32
			for i := int32(0); i < tt.createCreatingSandboxes; i++ {
				sbx := getBaseSandbox(idx)
				sbx.Status.Phase = v1alpha1.SandboxPending
				sbx.Labels["type"] = "creating"
				CreateSandboxWithStatus(t, client, sbx)
				idx++
			}
			for i := int32(0); i < tt.createPendingSandboxes; i++ {
				sbx := getBaseSandbox(idx)
				sbx.Status.Phase = v1alpha1.SandboxRunning
				sbx.Labels["type"] = "pending"
				sbx.Labels[consts.LabelSandboxState] = consts.SandboxStatePending
				CreateSandboxWithStatus(t, client, sbx)
				idx++
			}
			claimedStates := []string{consts.SandboxStateRunning, consts.SandboxStatePaused}
			for i := int32(0); i < tt.createClaimedSandboxes; i++ {
				sbx := getBaseSandbox(idx)
				sbx.Status.Phase = v1alpha1.SandboxRunning
				sbx.Labels["type"] = "claimed"
				sbx.Labels[consts.LabelSandboxState] = claimedStates[int(idx)%len(claimedStates)]
				CreateSandboxWithStatus(t, client, sbx)
				idx++
			}
			failedPhases := []v1alpha1.SandboxPhase{v1alpha1.SandboxFailed, v1alpha1.SandboxSucceeded}
			for i := int32(0); i < tt.createFailedSandboxes; i++ {
				sbx := getBaseSandbox(idx)
				sbx.Status.Phase = failedPhases[int(idx)%len(failedPhases)]
				sbx.Labels["type"] = "failed"
				CreateSandboxWithStatus(t, client, sbx)
				idx++
			}
			for i := int32(0); i < tt.createLegacySandboxes; i++ {
				sbx := getBaseSandbox(idx)
				sbx.Status.Phase = v1alpha1.SandboxRunning
				sbx.Labels[consts.LabelTemplateHash] = legacyHash
				sbx.Labels["type"] = "legacy"
				CreateSandboxWithStatus(t, client, sbx)
				idx++
			}
			for i := int32(0); i < tt.createDeletedSandboxes; i++ {
				sbx := getBaseSandbox(idx)
				sbx.Status.Phase = v1alpha1.SandboxRunning
				sbx.Labels["type"] = "deleted"
				sbx.DeletionTimestamp = ptr.To(metav1.Now())
				CreateSandboxWithStatus(t, client, sbx)
				idx++
			}
			for i := int32(0); i < tt.createKilledSandboxes; i++ {
				sbx := getBaseSandbox(idx)
				killPerformed := idx%2 > 0
				if killPerformed {
					sbx.Status.Phase = v1alpha1.SandboxTerminating
				} else {
					sbx.Status.Phase = v1alpha1.SandboxRunning
					sbx.Labels["type"] = "killed"
					sbx.Labels[consts.LabelSandboxState] = consts.SandboxStateKilling
				}
				CreateSandboxWithStatus(t, client, sbx)
				idx++
			}
			informerFactory := informers.NewSharedInformerFactoryWithOptions(client, time.Minute*10, informers.WithNamespace("default"))
			sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
			cache, err := NewCache[*v1alpha1.Sandbox]("default", informerFactory, sandboxInformer)
			assert.NoError(t, err)
			done := make(chan struct{})
			cache.Run(done)
			<-done
			newPodKey := "is-new-pod"
			pool := &Pool{
				client:         client,
				cache:          cache,
				reconcileQueue: make(chan context.Context, 1),
				template: &infra.SandboxTemplate{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-template",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelTemplateHash: updatedHash,
						},
					},
					Spec: infra.SandboxTemplateSpec{
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									consts.LabelTemplateHash: updatedHash,
									newPodKey:                "true",
								},
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test",
										Image: "test",
									},
								},
							},
						},
					},
				},
			}
			pool.Spec.Replicas.Store(tt.specReplicas)
			cache.Refresh()
			time.Sleep(time.Millisecond * 100)
			err = pool.Reconcile(ctx)
			assert.NoError(t, err)
			sandboxes, err := client.ApiV1alpha1().Sandboxes("default").List(ctx, metav1.ListOptions{})
			assert.NoError(t, err)
			var newSandboxes int32
			var totalSandboxes int32
			for _, sbx := range sandboxes.Items {
				if sbx.DeletionTimestamp == nil {
					if sbx.Labels[newPodKey] == "true" {
						newSandboxes++
					}
					totalSandboxes++
				}
			}
			existingSandboxes := totalSandboxes - newSandboxes
			removedSandboxes := idx - existingSandboxes
			assert.Equal(t, tt.expectTotalSandboxes, totalSandboxes)
			assert.Equal(t, tt.expectNewSandboxes, newSandboxes)
			assert.Equal(t, tt.expectRemovedSandboxes, removedSandboxes)
			info := pool.LoadDebugInfo()
			// 检查 Reconcile 前的状态
			assert.Equal(t, tt.createPendingSandboxes+tt.createClaimedSandboxes+tt.createCreatingSandboxes, info["total"])
			assert.Equal(t, tt.createPendingSandboxes, info["pending"])
			assert.Equal(t, tt.createClaimedSandboxes, info["claimed"])
			assert.Equal(t, tt.createCreatingSandboxes, info["creating"])
		})
	}
}

func TestPool_performScale_Down(t *testing.T) {
	utils.InitKLogOutput()

	tests := []struct {
		name             string
		specReplicas     int
		initialSandboxes []*v1alpha1.Sandbox
		checkFunc        func(t *testing.T, sandboxes []v1alpha1.Sandbox)
	}{
		{
			name:         "scale down pending sandboxes",
			specReplicas: 0,
			initialSandboxes: []*v1alpha1.Sandbox{
				createTestSandbox("pending-1", v1alpha1.SandboxRunning, map[string]string{
					consts.LabelSandboxState: consts.SandboxStatePending,
				}),
			},
			checkFunc: func(t *testing.T, sandboxes []v1alpha1.Sandbox) {
				assert.Equal(t, 0, len(sandboxes))
			},
		},
		{
			name:         "scale down creating sandboxes",
			specReplicas: 0,
			initialSandboxes: []*v1alpha1.Sandbox{
				createTestSandbox("creating-1", v1alpha1.SandboxPending, nil),
			},
			checkFunc: func(t *testing.T, sandboxes []v1alpha1.Sandbox) {
				assert.Equal(t, 0, len(sandboxes))
			},
		},
		{
			name:         "scale down mixed sandboxes (creating first)",
			specReplicas: 1,
			initialSandboxes: []*v1alpha1.Sandbox{
				createTestSandbox("pending-1", v1alpha1.SandboxRunning, map[string]string{
					consts.LabelSandboxState: consts.SandboxStatePending,
				}),
				createTestSandbox("pending-2", v1alpha1.SandboxRunning, map[string]string{
					consts.LabelSandboxState: consts.SandboxStatePending,
				}),
				createTestSandbox("creating-1", v1alpha1.SandboxPending, nil),
				createTestSandbox("creating-2", v1alpha1.SandboxPending, nil),
			},
			checkFunc: func(t *testing.T, sandboxes []v1alpha1.Sandbox) {
				assert.Equal(t, 1, len(sandboxes))
				// pending left
				assert.True(t, strings.HasPrefix(sandboxes[0].Name, "pending"))
			},
		},
		{
			name:         "scale down skips locked sandboxes",
			specReplicas: 0,
			initialSandboxes: []*v1alpha1.Sandbox{
				createTestSandbox("locked-pending", v1alpha1.SandboxRunning, map[string]string{
					consts.LabelSandboxState: consts.SandboxStatePending,
				}, map[string]string{
					consts.AnnotationLock:  "some-lock",
					consts.AnnotationOwner: "some-owner",
				}),
				createTestSandbox("unlocked-pending", v1alpha1.SandboxRunning, map[string]string{
					consts.LabelSandboxState: consts.SandboxStatePending,
				}),
			},
			checkFunc: func(t *testing.T, sandboxes []v1alpha1.Sandbox) {
				assert.Equal(t, 1, len(sandboxes))
				assert.Equal(t, "locked-pending", sandboxes[0].Name)
			},
		},
		{
			name:         "scale down manager-owned locked sandboxes",
			specReplicas: 0,
			initialSandboxes: []*v1alpha1.Sandbox{
				createTestSandbox("manager-locked", v1alpha1.SandboxRunning, map[string]string{
					consts.LabelSandboxState: consts.SandboxStatePending,
				}, map[string]string{
					consts.AnnotationLock:  "some-lock",
					consts.AnnotationOwner: consts.OwnerManager,
				}),
				createTestSandbox("unlocked-pending", v1alpha1.SandboxRunning, map[string]string{
					consts.LabelSandboxState: consts.SandboxStatePending,
				}),
			},
			checkFunc: func(t *testing.T, sandboxes []v1alpha1.Sandbox) {
				assert.Equal(t, 0, len(sandboxes))
			},
		},
		{
			name:         "scale down skips claimed sandboxes",
			specReplicas: 0,
			initialSandboxes: []*v1alpha1.Sandbox{
				createTestSandbox("pending-1", v1alpha1.SandboxRunning, map[string]string{
					consts.LabelSandboxState: consts.SandboxStatePending,
				}),
				createTestSandbox("claimed-1", v1alpha1.SandboxRunning, map[string]string{
					consts.LabelSandboxState: consts.SandboxStateRunning,
				}),
			},
			checkFunc: func(t *testing.T, sandboxes []v1alpha1.Sandbox) {
				assert.Equal(t, 1, len(sandboxes))
				assert.Equal(t, "claimed-1", sandboxes[0].Name)
			},
		},
		{
			name:         "scale down skips deleted sandboxes",
			specReplicas: 0,
			initialSandboxes: []*v1alpha1.Sandbox{
				createTestSandbox("pending-1", v1alpha1.SandboxRunning, map[string]string{
					consts.LabelSandboxState: consts.SandboxStatePending,
				}),
				func() *v1alpha1.Sandbox {
					sbx := createTestSandbox("deleted-1", v1alpha1.SandboxRunning, map[string]string{
						consts.LabelSandboxState: consts.SandboxStatePending,
					})
					sbx.DeletionTimestamp = ptr.To(metav1.Now())
					return sbx
				}(),
			},
			checkFunc: func(t *testing.T, sandboxes []v1alpha1.Sandbox) {
				assert.Equal(t, 1, len(sandboxes))
				assert.Equal(t, "deleted-1", sandboxes[0].Name)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			client := fake.NewSimpleClientset()

			// 设置缓存和池
			informerFactory := informers.NewSharedInformerFactoryWithOptions(client, time.Minute*10, informers.WithNamespace("default"))
			sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
			cache, err := NewCache[*v1alpha1.Sandbox]("default", informerFactory, sandboxInformer)
			assert.NoError(t, err)

			done := make(chan struct{})
			go cache.Run(done)
			<-done
			time.Sleep(100 * time.Millisecond) // 等待缓存同步

			pool := &Pool{
				client:         client,
				cache:          cache,
				reconcileQueue: make(chan context.Context, 1),
				template: &infra.SandboxTemplate{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-template",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelTemplateHash: "test-hash",
						},
					},
					Spec: infra.SandboxTemplateSpec{
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "test", Image: "test"},
								},
							},
						},
					},
				},
			}

			// 创建初始沙箱
			for _, sbx := range tt.initialSandboxes {
				CreateSandboxWithStatus(t, client, sbx)
			}

			// 重新获取沙箱以确保缓存同步
			allSandboxes, err := client.ApiV1alpha1().Sandboxes("default").List(ctx, metav1.ListOptions{})
			assert.NoError(t, err)

			// 转换为 []*v1alpha1.Sandbox
			sandboxPtrs := make([]*v1alpha1.Sandbox, len(allSandboxes.Items))
			for i := range allSandboxes.Items {
				sandboxPtrs[i] = &allSandboxes.Items[i]
			}

			// 分组沙箱并执行缩容
			groups, err := pool.GroupAllSandboxes(ctx, sandboxPtrs)
			assert.NoError(t, err)

			actualReplicas := len(allSandboxes.Items)
			err = pool.performScale(ctx, groups, tt.specReplicas, actualReplicas)
			assert.NoError(t, err)

			// 验证结果
			sandboxes, err := client.ApiV1alpha1().Sandboxes("default").List(ctx, metav1.ListOptions{})
			assert.NoError(t, err)
			tt.checkFunc(t, sandboxes.Items)

			cache.Stop()
			time.Sleep(100 * time.Millisecond)
			close(done)
		})
	}
}

// Helper function to create test sandboxes
func createTestSandbox(name string, phase v1alpha1.SandboxPhase, labels map[string]string, annotations ...map[string]string) *v1alpha1.Sandbox {
	sbx := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				consts.LabelSandboxPool:  "test-template",
				consts.LabelTemplateHash: "test-hash",
			},
		},
		Status: v1alpha1.SandboxStatus{
			Phase: phase,
		},
	}

	// 添加额外标签
	for k, v := range labels {
		sbx.Labels[k] = v
	}

	// 添加注解（如果提供）
	if len(annotations) > 0 && annotations[0] != nil {
		sbx.Annotations = annotations[0]
	} else if len(annotations) == 0 {
		// 初始化空的注解映射
		sbx.Annotations = make(map[string]string)
	}

	return sbx
}
