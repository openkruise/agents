package sandboxcr

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned"
	"github.com/openkruise/agents/client/clientset/versioned/fake"
	informers "github.com/openkruise/agents/client/informers/externalversions"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
func TestPool_ClaimSandbox(t *testing.T) {
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
			client := fake.NewSimpleClientset()

			informerFactory := informers.NewSharedInformerFactory(client, time.Minute*10)
			sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
			c, err := NewCache(informerFactory, sandboxInformer, sandboxInformer)
			if err != nil {
				t.Fatalf("Failed to create cache: %v", err)
			}

			err = c.Run(t.Context())
			assert.NoError(t, err)
			defer c.Stop()

			pool := &Pool{
				Name:      "test-pool",
				Namespace: "default",
				client:    client,
				cache:     c,
			}

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
			c.Refresh()
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
