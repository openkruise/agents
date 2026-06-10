/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package core

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"github.com/openkruise/agents/pkg/utils/inplaceupdate"
)

var (
	sandboxControllerKind       = agentsv1alpha1.GroupVersion.WithKind("Sandbox")
	ResourceVersionExpectations = expectations.NewResourceVersionExpectation()
	ScaleExpectation            = expectations.NewScaleExpectations()
)

type EnsureFuncArgs struct {
	Pod       *corev1.Pod
	Box       *agentsv1alpha1.Sandbox
	NewStatus *agentsv1alpha1.SandboxStatus
}

type SandboxControl interface {
	// EnsureSandboxRunning handle sandbox with status phase = Pending
	EnsureSandboxRunning(ctx context.Context, args EnsureFuncArgs) (time.Duration, error)

	// EnsureSandboxUpdated handle sandbox with status phase = Running
	EnsureSandboxUpdated(ctx context.Context, args EnsureFuncArgs) error

	// EnsureSandboxPaused handle sandbox with status phase = Paused
	EnsureSandboxPaused(ctx context.Context, args EnsureFuncArgs) error

	// EnsureSandboxResumed handle sandbox with status phase = Resuming
	EnsureSandboxResumed(ctx context.Context, args EnsureFuncArgs) error

	// EnsureSandboxUpgraded handle sandbox with status phase = Upgrading
	EnsureSandboxUpgraded(ctx context.Context, args EnsureFuncArgs) error

	// EnsureSandboxTerminated handle sandbox with status phase = Terminating
	EnsureSandboxTerminated(ctx context.Context, args EnsureFuncArgs) error
}

type SandboxControlArgs struct {
	Client            client.Client
	APIReader         client.Reader
	Recorder          record.EventRecorder
	RateLimiter       *RateLimiter
	CheckpointControl *CheckpointControl
	PodControl        *PodControl
}

func NewSandboxControl(args SandboxControlArgs) map[string]SandboxControl {
	controls := map[string]SandboxControl{}
	controls[CommonControlName] = NewCommonControl(args)
	return controls
}

// SandboxInitializer handles sandbox post-recreation initialization after resume or recreate upgrade.
// When a sandbox pod becomes available again, runtime and dynamic CSI mounts need to be re-initialized.
type SandboxInitializer interface {
	Initialize(ctx context.Context, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus) error
}

// TraceHelper abstracts operation tracing for sandbox controllers.
// Internal (ACS) builds supply a real implementation that writes trace logs;
// external (common) builds use the default noopTraceHelper.
type TraceHelper interface {
	TraceOperation(ctx context.Context, phase string, obj interface{}, operation func() error) error
	TraceOperationTreatNotFoundAsSuccess(ctx context.Context, phase string, obj interface{}, operation func() error) error
}

// noopTraceHelper is the default TraceHelper that performs no tracing.
type noopTraceHelper struct{}

func (n *noopTraceHelper) TraceOperation(_ context.Context, _ string, _ interface{}, operation func() error) error {
	return operation()
}

func (n *noopTraceHelper) TraceOperationTreatNotFoundAsSuccess(_ context.Context, _ string, _ interface{}, operation func() error) error {
	return operation()
}

// NewNoopTraceHelper returns a TraceHelper that simply executes the operation without tracing.
func NewNoopTraceHelper() TraceHelper {
	return &noopTraceHelper{}
}

// InPlaceUpdateHandler defines the interface for inplace update handlers
type InPlaceUpdateHandler interface {
	GetInPlaceUpdateControl() *inplaceupdate.InPlaceUpdateControl
	GetRecorder() record.EventRecorder
}
