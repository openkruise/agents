package sandboxcr

import (
	"context"
	"fmt"
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
	ctx := context.Background()
	_, err := client.ApiV1alpha1().Sandboxes(sbx.Namespace).Create(context.Background(), sbx, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = client.ApiV1alpha1().Sandboxes(sbx.Namespace).UpdateStatus(ctx, sbx, metav1.UpdateOptions{})
	assert.NoError(t, err)
}

//goland:noinspection GoDeprecation
func TestPool_ClaimSandbox(t *testing.T) {
	// Test cases
	tests := []struct {
		name        string
		available   int32
		modifier    func(sbx infra.Sandbox)
		expectError bool
		preModifier func(pod *corev1.Pod)
	}{
		{
			name:        "claim with available pods",
			available:   2,
			modifier:    nil,
			expectError: false,
		},
		{
			name:        "claim with no available pods",
			available:   0,
			modifier:    nil,
			expectError: true,
		},
		{
			name:      "claim with modifier",
			available: 2,
			modifier: func(sbx infra.Sandbox) {
				sbx.SetAnnotations(map[string]string{
					"test-annotation": "test-value",
				})
			},
			expectError: false,
		},
		{
			name:      "no stock",
			available: 10,
			preModifier: func(pod *corev1.Pod) {
				pod.Annotations[v1alpha1.AnnotationLock] = "XX"
			},
			expectError: true,
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

			done := make(chan struct{})
			go c.Run(done)
			<-done
			defer c.Stop()

			pool := &Pool{
				Name:      "test-pool",
				Namespace: "default",
				client:    client,
				cache:     c,
			}

			for i := 0; i < int(tt.available); i++ {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("pod-%d", i),
						Namespace: "default",
						Labels: map[string]string{
							v1alpha1.LabelSandboxPool: pool.Name,
						},
						Annotations:     map[string]string{},
						OwnerReferences: GetSbsOwnerReference(),
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						Conditions: []corev1.PodCondition{
							{
								Type:   corev1.PodReady,
								Status: corev1.ConditionTrue,
							},
						},
						PodIP: "1.2.3.4",
					},
				}
				if tt.preModifier != nil {
					tt.preModifier(pod)
				}
				sbx := ConvertPodToSandboxCR(pod)
				state, reason := sandboxutils.GetSandboxState(sbx)
				assert.Equal(t, v1alpha1.SandboxStateAvailable, state, "reason", reason)
				CreateSandboxWithStatus(t, client, sbx)
			}
			c.Refresh()
			time.Sleep(100 * time.Millisecond)

			user := "test-user"
			sbx, err := pool.ClaimSandbox(context.Background(), user, 1, tt.modifier)

			if tt.expectError {
				assert.Error(t, err)
				return
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, sbx)
				assert.NotEmpty(t, sbx.GetAnnotations()[v1alpha1.AnnotationLock])
				assert.Equal(t, user, sbx.GetAnnotations()[v1alpha1.AnnotationOwner])
				if tt.modifier != nil {
					assert.Equal(t, "test-value", sbx.GetAnnotations()["test-annotation"])
				}
			}
		})
	}
}
