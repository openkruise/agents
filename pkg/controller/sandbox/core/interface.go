package core

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

var (
	sandboxControllerKind = agentsv1alpha1.GroupVersion.WithKind("Sandbox")
)

type EnsureFuncArgs struct {
	Pod       *corev1.Pod
	Box       *agentsv1alpha1.Sandbox
	NewStatus *agentsv1alpha1.SandboxStatus
}

type SandboxControl interface {
	// EnsureSandboxPhasePending ensure sandbox status phase = Pending
	EnsureSandboxPhasePending(ctx context.Context, args EnsureFuncArgs) error

	// EnsureSandboxPhaseRunning ensure sandbox status phase = Running
	EnsureSandboxPhaseRunning(ctx context.Context, args EnsureFuncArgs) error

	// EnsureSandboxPhasePaused ensure sandbox status phase = Paused
	EnsureSandboxPhasePaused(ctx context.Context, args EnsureFuncArgs) error

	// EnsureSandboxPhaseResuming ensure sandbox status phase = Resume
	EnsureSandboxPhaseResuming(ctx context.Context, args EnsureFuncArgs) error

	// EnsureSandboxPhaseTerminating ensure sandbox status phase = Terminating
	EnsureSandboxPhaseTerminating(ctx context.Context, args EnsureFuncArgs) error
}

func NewSandboxControl(c client.Client) map[string]SandboxControl {
	controls := map[string]SandboxControl{}
	controls[CommonControlName] = &commonControl{Client: c}
	return controls
}
