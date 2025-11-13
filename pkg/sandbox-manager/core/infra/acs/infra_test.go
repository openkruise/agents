package acs

import (
	"context"
	"testing"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestInfra_GetSandbox(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				consts.LabelSandboxID:    "test-pod",
				consts.LabelSandboxPool:  "test-pool",
				consts.LabelSandboxState: consts.SandboxStateRunning,
			},
		},
	}
	eventer := events.NewEventer()
	client := fake.NewClientset(pod)
	infraInstance, err := NewInfra("default", ".", eventer, client, nil)
	assert.NoError(t, err)
	err = infraInstance.Run(context.Background())
	assert.NoError(t, err)
	sandbox, err := infraInstance.GetSandbox("test-pod")
	assert.NoError(t, err)
	_, ok := sandbox.(*Sandbox)
	assert.True(t, ok)
	sandboxes, err := infraInstance.SelectSandboxes(infra.SandboxSelectorOptions{WantRunning: true})
	assert.NoError(t, err)
	assert.Equal(t, 1, len(sandboxes))
	_, ok = sandboxes[0].(*Sandbox)
	assert.True(t, ok)
	noSandboxes, err := infraInstance.SelectSandboxes(infra.SandboxSelectorOptions{WantPaused: true})
	assert.NoError(t, err)
	assert.Equal(t, 0, len(noSandboxes))
}

func TestInfra_Debug(t *testing.T) {
	client := fake.NewClientset()
	eventer := events.NewEventer()
	infraInstance, err := NewInfra("default", ".", eventer, client, nil)
	assert.NoError(t, err)
	info := infraInstance.LoadDebugInfo()
	assert.Equal(t, "acs", info["infra"])
}
