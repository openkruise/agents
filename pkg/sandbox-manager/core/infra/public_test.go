package infra

import (
	"testing"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBaseInfra_GetPoolByObject(t *testing.T) {
	tests := []struct {
		name     string
		addPools []string
		pod      *corev1.Pod
		expect   string
	}{
		{
			name:     "get pool",
			addPools: []string{"pool1"},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						consts.LabelSandboxPool: "pool1",
					},
				},
			},
			expect: "pool1",
		},
		{
			name:     "not found",
			addPools: []string{"pool2"},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						consts.LabelSandboxPool: "pool1",
					},
				},
			},
			expect: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i := &BaseInfra{}
			for _, pool := range tt.addPools {
				i.AddPool(pool, &FakePool{template: &SandboxTemplate{
					ObjectMeta: metav1.ObjectMeta{
						Name: pool,
					},
				}})
			}
			got, ok := i.GetPoolByObject(tt.pod)
			if tt.expect == "" {
				assert.False(t, ok)
			} else {
				assert.True(t, ok)
				assert.Equal(t, tt.expect, got.GetTemplate().Name)
			}
		})
	}
}
