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

	"github.com/go-logr/logr"
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
)

type EnsureFuncArgs struct {
	Pod       *corev1.Pod
	Box       *agentsv1alpha1.Sandbox
	NewStatus *agentsv1alpha1.SandboxStatus
}

type SandboxControl interface {
	// EnsureSandboxRunning handle sandbox with status phase = Pending
	EnsureSandboxRunning(ctx context.Context, args EnsureFuncArgs) error

	// EnsureSandboxUpdated handle sandbox with status phase = Running
	EnsureSandboxUpdated(ctx context.Context, args EnsureFuncArgs) error

	// EnsureSandboxPaused handle sandbox with status phase = Paused
	EnsureSandboxPaused(ctx context.Context, args EnsureFuncArgs) error

	// EnsureSandboxResumed handle sandbox with status phase = Resuming
	EnsureSandboxResumed(ctx context.Context, args EnsureFuncArgs) error

	// EnsureSandboxTerminated handle sandbox with status phase = Terminating
	EnsureSandboxTerminated(ctx context.Context, args EnsureFuncArgs) error
}

func NewSandboxControl(c client.Client, recorder record.EventRecorder) map[string]SandboxControl {
	controls := map[string]SandboxControl{}
	controls[CommonControlName] = NewCommonControl(c, recorder)
	return controls
}

// InPlaceUpdateHandler defines the interface for inplace update handlers
type InPlaceUpdateHandler interface {
	GetInPlaceUpdateControl() *inplaceupdate.InPlaceUpdateControl
	GetRecorder() record.EventRecorder
	GetLogger(ctx context.Context, box *agentsv1alpha1.Sandbox) logr.Logger
}
