package sandboxcr

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned"
	"github.com/openkruise/agents/client/clientset/versioned/fake"
	informers "github.com/openkruise/agents/client/informers/externalversions"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
)

func GetSbsOwnerReference() []metav1.OwnerReference {
	sbs := &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-sandboxset",
			UID:  "12345",
		},
	}
	return []metav1.OwnerReference{*metav1.NewControllerRef(sbs, v1alpha1.SandboxSetControllerKind)}
}

func CreateSandboxWithStatus(t *testing.T, client versioned.Interface, sbx *v1alpha1.Sandbox) {
	_, err := client.ApiV1alpha1().Sandboxes(sbx.Namespace).Create(t.Context(), sbx, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = client.ApiV1alpha1().Sandboxes(sbx.Namespace).UpdateStatus(t.Context(), sbx, metav1.UpdateOptions{})
	assert.NoError(t, err)
}

//goland:noinspection GoDeprecation
func NewTestPool(t *testing.T) (*Pool, versioned.Interface) {
	client := fake.NewSimpleClientset()

	informerFactory := informers.NewSharedInformerFactory(client, time.Minute*10)
	sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
	c, err := NewCache(informerFactory, sandboxInformer, sandboxInformer, nil)
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	err = c.Run(t.Context())
	assert.NoError(t, err)

	return &Pool{
		Name:      "test-pool",
		Namespace: "default",
		client:    client,
		cache:     c,
	}, client
}

//goland:noinspection GoDeprecation
func TestPool_ClaimSandbox(t *testing.T) {
	SetClaimLockTimeout(100 * time.Millisecond)
	// Test cases
	tests := []struct {
		name        string
		available   int32
		options     infra.ClaimSandboxOptions
		preModifier func(pod *v1alpha1.Sandbox)
		postCheck   func(t *testing.T, sbx infra.Sandbox)
		expectError string
	}{
		{
			name:      "claim with available pods",
			available: 2,
		},
		{
			name:        "claim with no available pods",
			available:   0,
			expectError: "no stock",
		},
		{
			name:      "claim with modifier",
			available: 2,
			options: infra.ClaimSandboxOptions{
				Modifier: func(sbx infra.Sandbox) {
					sbx.SetAnnotations(map[string]string{
						"test-annotation": "test-value",
					})
				},
			},
			postCheck: func(t *testing.T, sbx infra.Sandbox) {
				assert.Equal(t, "test-value", sbx.GetAnnotations()["test-annotation"])
			},
		},
		{
			name:      "all locked",
			available: 10,
			preModifier: func(sbx *v1alpha1.Sandbox) {
				sbx.Annotations[v1alpha1.AnnotationLock] = "XX"
			},
			expectError: "no candidate",
		},
		{
			name:      "claim with image",
			available: 1,
			options: infra.ClaimSandboxOptions{
				Image: "new-image",
			},
			postCheck: func(t *testing.T, sbx infra.Sandbox) {
				assert.Equal(t, "new-image", sbx.(*Sandbox).Spec.Template.Spec.Containers[0].Image)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool, client := NewTestPool(t)

			for i := 0; i < int(tt.available); i++ {
				sbx := &v1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("sbx-%d", i),
						Namespace: "default",
						Labels: map[string]string{
							v1alpha1.LabelSandboxPool: pool.Name,
						},
						Annotations:     map[string]string{},
						OwnerReferences: GetSbsOwnerReference(),
					},
					Spec: v1alpha1.SandboxSpec{
						SandboxTemplate: v1alpha1.SandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:  "main",
											Image: "old-image",
										},
									},
								},
							},
						},
					},
					Status: v1alpha1.SandboxStatus{
						Phase: v1alpha1.SandboxRunning,
						Conditions: []metav1.Condition{
							{
								Type:   string(v1alpha1.SandboxConditionReady),
								Status: metav1.ConditionTrue,
							},
						},
						PodInfo: v1alpha1.PodInfo{
							PodIP: "1.2.3.4",
						},
					},
				}
				if tt.preModifier != nil {
					tt.preModifier(sbx)
				}
				state, reason := sandboxutils.GetSandboxState(sbx)
				assert.Equal(t, v1alpha1.SandboxStateAvailable, state, "reason", reason)
				CreateSandboxWithStatus(t, client, sbx)
			}
			time.Sleep(10 * time.Millisecond)

			user := "test-user"
			sbx, err := pool.ClaimSandbox(t.Context(), user, 1, tt.options)

			if tt.expectError != "" {
				assert.Error(t, err)
				assert.True(t, strings.Contains(err.Error(), tt.expectError))
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, sbx)
				assert.NotEmpty(t, sbx.GetAnnotations()[v1alpha1.AnnotationLock])
				assert.Equal(t, user, sbx.GetAnnotations()[v1alpha1.AnnotationOwner])
				if tt.postCheck != nil {
					tt.postCheck(t, sbx)
				}
			}
		})
	}
}

func TestPool_checkSandboxInplaceUpdate(t *testing.T) {
	utils.InitLogOutput()
	tests := []struct {
		name                   string
		reserveFailedSandboxes string
		generation             int64
		observedGeneration     int64
		condStatus             metav1.ConditionStatus
		condReason             string
		condMessage            string
		expectResult           bool
		expectError            error
		expectDeleted          bool
	}{
		{
			name:               "success",
			generation:         1,
			observedGeneration: 1,
			condStatus:         metav1.ConditionTrue,
			condReason:         v1alpha1.SandboxReadyReasonPodReady,
			expectResult:       true,
		},
		{
			name:               "not satisfied: out-dated cache",
			generation:         2,
			observedGeneration: 1,
			condStatus:         metav1.ConditionTrue,
			condReason:         v1alpha1.SandboxReadyReasonPodReady,
			expectResult:       false,
		},
		{
			name:               "not satisfied: inplace updating",
			generation:         1,
			observedGeneration: 1,
			condStatus:         metav1.ConditionFalse,
			condReason:         v1alpha1.SandboxReadyReasonInplaceUpdating,
			expectResult:       false,
		},
		{
			name:               "not satisfied: start container failed, deleted",
			generation:         1,
			observedGeneration: 1,
			condStatus:         metav1.ConditionFalse,
			condReason:         v1alpha1.SandboxReadyReasonStartContainerFailed,
			condMessage:        "by test",
			expectResult:       false,
			expectError:        retriableError{Message: "sandbox inplace update failed: by test"},
			expectDeleted:      true,
		},
		{
			name:                   "not satisfied: start container failed, reserved",
			generation:             1,
			observedGeneration:     1,
			reserveFailedSandboxes: v1alpha1.True,
			condStatus:             metav1.ConditionFalse,
			condReason:             v1alpha1.SandboxReadyReasonStartContainerFailed,
			condMessage:            "by test",
			expectResult:           false,
			expectError:            retriableError{Message: "sandbox inplace update failed: by test"},
			expectDeleted:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool, client := NewTestPool(t)
			pool.Annotations = map[string]string{
				v1alpha1.AnnotationReserveFailedSandbox: tt.reserveFailedSandboxes,
			}
			sbx := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx-1",
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.LabelSandboxPool: pool.Name,
					},
					Annotations: map[string]string{},
					Generation:  tt.generation,
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:    string(v1alpha1.SandboxConditionReady),
							Status:  tt.condStatus,
							Reason:  tt.condReason,
							Message: tt.condMessage,
						},
					},
					PodInfo: v1alpha1.PodInfo{
						PodIP: "1.2.3.4",
					},
					ObservedGeneration: tt.observedGeneration,
				},
			}
			CreateSandboxWithStatus(t, client, sbx)
			time.Sleep(10 * time.Millisecond)

			gotSbx, err := pool.cache.GetSandbox(sandboxutils.GetSandboxID(sbx))
			assert.NoError(t, err)
			if err != nil {
				return
			}
			result, err := pool.checkSandboxInplaceUpdate(t.Context(), gotSbx)
			assert.Equal(t, tt.expectResult, result)
			if tt.expectError != nil {
				assert.Error(t, err)
				assert.True(t, errors.Is(err, tt.expectError))
			} else {
				assert.NoError(t, err)
			}
			time.Sleep(10 * time.Millisecond)
			_, err = client.ApiV1alpha1().Sandboxes(sbx.Namespace).Get(t.Context(), sbx.Name, metav1.GetOptions{})
			if tt.expectDeleted {
				assert.True(t, apierrors.IsNotFound(err))
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
