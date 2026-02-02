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

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/sandbox/core"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ResourceVersionExpectations = expectations.NewResourceVersionExpectation()
)

// RequeueStrategy defines the requeue behavior for controller reconciliation
type RequeueStrategy struct {
	// Immediate indicates whether to requeue immediately (ctrl.Result{Requeue: true})
	// If false, uses After duration
	Immediate bool

	// After specifies the duration to wait before requeue
	// Only used when Immediate is false
	After time.Duration
}

// RequeueImmediately returns a strategy for immediate requeue
func RequeueImmediately() RequeueStrategy {
	return RequeueStrategy{Immediate: true}
}

// RequeueAfter returns a strategy for delayed requeue
func RequeueAfter(duration time.Duration) RequeueStrategy {
	return RequeueStrategy{After: duration}
}

// NoRequeue returns a strategy that waits for Watch events
func NoRequeue() RequeueStrategy {
	return RequeueStrategy{}
}

// ClaimArgs encapsulates all arguments needed for claim operations
type ClaimArgs struct {
	Claim      *agentsv1alpha1.SandboxClaim
	SandboxSet *agentsv1alpha1.SandboxSet
	NewStatus  *agentsv1alpha1.SandboxClaimStatus
}

// ClaimControl defines the interface for claiming operations
type ClaimControl interface {
	// EnsureClaimClaiming handles claim in Claiming phase
	EnsureClaimClaiming(ctx context.Context, args ClaimArgs) (RequeueStrategy, error)

	// EnsureClaimCompleted handles claim in Completed phase (TTL cleanup)
	EnsureClaimCompleted(ctx context.Context, args ClaimArgs) (RequeueStrategy, error)
}

// NewClaimControl creates a map of claim controls
func NewClaimControl(c client.Client, recorder record.EventRecorder, sandboxClient clients.SandboxClient, cache *sandboxcr.Cache) map[string]ClaimControl {
	controls := map[string]ClaimControl{}
	controls[core.CommonControlName] = NewCommonControl(c, recorder, sandboxClient, cache)
	return controls
}
