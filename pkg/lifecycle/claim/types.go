/*
Copyright 2026.

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

package claim

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandboxmetrics"
	runtimeclient "github.com/openkruise/agents/pkg/utils/runtime"
	runtimeconfig "github.com/openkruise/agents/pkg/utils/runtime/config"
)

const bytesPerMiB = int64(1024 * 1024)

// ResourceList is a neutral, backend-independent summary of a sandbox's CPU,
// memory, and disk footprint.
type ResourceList struct {
	CPUMilli   int64
	MemoryMB   int64
	DiskSizeMB int64
}

// SandboxResource pairs the requested and limiting ResourceList of a sandbox.
type SandboxResource struct {
	Requests ResourceList
	Limits   ResourceList
}

func memoryBytesToFloorMiB(q resource.Quantity) int64 {
	return q.Value() / bytesPerMiB
}

func memoryBytesToCeilMiB(q resource.Quantity) int64 {
	bytes := q.Value()
	if bytes <= 0 {
		return 0
	}
	return (bytes + bytesPerMiB - 1) / bytesPerMiB
}

func calculateResourceList(
	containers []corev1.Container,
	pick func(corev1.ResourceRequirements) corev1.ResourceList,
	memoryToMiB func(resource.Quantity) int64,
) ResourceList {
	out := ResourceList{}
	for _, container := range containers {
		resources := pick(container.Resources)
		if resources == nil {
			continue
		}
		if cpu, ok := resources[corev1.ResourceCPU]; ok {
			out.CPUMilli += cpu.MilliValue()
		}
		if memory, ok := resources[corev1.ResourceMemory]; ok {
			out.MemoryMB += memoryToMiB(memory)
		}
		if disk, ok := resources[corev1.ResourceEphemeralStorage]; ok {
			out.DiskSizeMB += disk.Value() / bytesPerMiB
		}
	}
	return out
}

// CalculateResourceFromContainers sums resource requests and limits from a list
// of containers.
func CalculateResourceFromContainers(containers []corev1.Container) SandboxResource {
	requests := calculateResourceList(containers, func(r corev1.ResourceRequirements) corev1.ResourceList {
		return r.Requests
	}, memoryBytesToFloorMiB)
	limits := calculateResourceList(containers, func(r corev1.ResourceRequirements) corev1.ResourceList {
		return r.Limits
	}, memoryBytesToCeilMiB)
	return SandboxResource{
		Requests: requests,
		Limits:   limits,
	}
}

// SandboxAdmission is the neutral quota admission hook pair used by the claim
// engine. Acquire reserves capacity for a lock string before a sandbox is
// locked; Release gives it back. Both are optional and may be nil.
type SandboxAdmission struct {
	Acquire func(ctx context.Context, lockString string, resource SandboxResource) error
	Release func(ctx context.Context, lockString string) error
}

// SandboxAdmissionReleaseTimeout bounds the detached context used to release
// admission after a failed claim so a slow quota backend cannot block cleanup.
const SandboxAdmissionReleaseTimeout = 250 * time.Millisecond

// LockType records how a claimed sandbox was secured.
type LockType string

const (
	LockTypeCreate    = LockType("create")
	LockTypeUpdate    = LockType("update")
	LockTypeSpeculate = LockType("speculate")
)

// Options is the neutral claim request. It is the single definition shared by
// the sandbox-manager E2B path (via the infra.ClaimSandboxOptions alias) and the
// independent SandboxClaim controller. Every field type is neutral so this
// package keeps a closure free of pkg/sandbox-manager/** and pkg/servers/**.
type Options struct {
	Namespace string `json:"namespace,omitempty"`
	// User specifies the owner of sandbox, Required
	User string `json:"user"`
	// Template specifies the pool to claim sandbox from, Required
	Template string `json:"template"`
	// CandidateCounts is the maximum number of available sandboxes to select from the cache
	CandidateCounts int `json:"candidateCounts"`
	// Lock string used in optimistic lock
	LockString string            `json:"lockString"`
	Admission  *SandboxAdmission `json:"-"`
	// Set Modifier to modify the Sandbox before it is updated. Returning an error aborts persistence.
	Modifier func(sandbox Handle) error `json:"-"`
	// AutoPausePolicy replaces the complete auto-pause policy of the claimed
	// sandbox. A nil value keeps the policy the picked sandbox already carries.
	AutoPausePolicy *v1alpha1.AutoPausePolicy `json:"-"`
	// Probes are merged by name into the claimed sandbox's probe list: same-name
	// entries replace the pool version, new names are appended. A nil value keeps
	// the probes the picked sandbox already carries.
	Probes []v1alpha1.Probe `json:"-"`
	// ProbeUpdatesEnabled declares that probe delivery can update existing Pods
	// for this call. Claims carrying Probes may reuse a pool candidate only when
	// this is true and the candidate is scheduled on a real node.
	ProbeUpdatesEnabled bool `json:"-"`
	// ReserveFailedSandboxFor controls how long failed sandboxes are kept for debugging.
	//   nil                          — backend default (DefaultReserveFailedSandboxFor)
	//   ReserveFailedSandboxNever    — delete immediately
	//   positive                     — reserve for that duration, then delete
	//   ReserveFailedSandboxForever  — reserve forever (never delete)
	ReserveFailedSandboxFor *time.Duration `json:"reserveFailedSandboxFor"`
	// Set InplaceUpdate to trigger an inplace-update (image and/or resources)
	InplaceUpdate *InplaceUpdateOptions `json:"inplaceUpdate"`
	// Set RuntimeConfig to non-nil value to inject runtime configuration
	RuntimeConfig []v1alpha1.RuntimeConfig `json:"runtimeConfig"`
	// Set InitRuntime to non-nil value to init the agent-runtime
	InitRuntime *runtimeconfig.InitRuntimeOptions `json:"initRuntime"`
	// MemoryPersistence overrides memory persistence for this Sandbox; nil inherits its current setting.
	MemoryPersistence *bool `json:"memoryPersistence,omitempty"`
	// Set CSIMount to non-nil value to mount a CSI volume
	CSIMount *runtimeconfig.CSIMountOptions `json:"CSIMount"`
	// Max ClaimTimeout duration
	ClaimTimeout time.Duration `json:"claimTimeout"`
	// Max WaitReadyTimeout duration
	WaitReadyTimeout time.Duration `json:"waitReadyTimeout"`
	// Create a Sandbox instance from the template if no available ones in SandboxSets
	CreateOnNoStock bool `json:"createOnNoStock"`
	// A creating sandbox lasts for SpeculateCreatingDuration may be picked as a candidate when no available ones in SandboxSets.
	// Set to 0 to disable speculation feature
	SpeculateCreatingDuration time.Duration `json:"speculateCreatingDuration"`
	// UserMetadataKeys records the keys of user-specified labels/annotations
	// added during claim (from SandboxClaim.Spec). Only the independent Sandbox
	// controller consumes it, through the cleanup flow that returns the sandbox
	// to the pool.
	UserMetadataKeys *v1alpha1.UpdatedMetadataInClaim `json:"-"`
	// Claim is the SandboxClaim that triggers this claim. May be nil for
	// non-CRD paths such as the E2B API. IdentityProvider implementations must
	// handle a nil Claim gracefully.
	Claim *v1alpha1.SandboxClaim `json:"-"`
	// ControlPlaneSource is the control plane that drives this claim. It is
	// stamped on the claimed sandbox as v1alpha1.LabelSandboxSource so that
	// operation metrics are attributed to whoever handled the instance. Accepts
	// v1alpha1.SandboxSourceE2B and v1alpha1.SandboxSourceK8s; any other value,
	// including the empty one, is treated as v1alpha1.SandboxSourceK8s because
	// agent-sandbox-controller is the mandatory layer.
	ControlPlaneSource string `json:"controlPlaneSource,omitempty"`
	// RuntimeTLSBundle is the client TLS bundle used to reach a TLS-capable
	// agent-runtime during claim post-processing (init handshake and CSI mounts).
	// Nil means the caller is not configured for runtime TLS. The field belongs to
	// whoever drives the claim, never to API callers.
	RuntimeTLSBundle *runtimeclient.TLSBundle `json:"-"`
	// TrafficAccessTokenValidity is Manager-owned issuance policy. API callers
	// cannot override it because the manager replaces it before delegation.
	TrafficAccessTokenValidity time.Duration `json:"-"`
}

// Metrics records per-attempt timing and outcome of a claim. It is serializable
// (no in-memory token), so the traffic access token is returned out-of-band on
// the claimed Handle instead.
type Metrics struct {
	ClaimResults        []sandboxmetrics.SandboxClaimResult
	CreationResults     []SandboxCreationResult
	Retries             int
	Total               time.Duration
	Wait                time.Duration
	RetryCost           time.Duration
	PickAndLock         time.Duration
	WaitReady           time.Duration
	InitRuntime         time.Duration
	CSIMount            time.Duration
	SecurityToken       time.Duration
	TrafficToken        time.Duration
	LockType            LockType
	LastError           error
	PickSandboxFailures []PickSandboxFailure
}

// SandboxCreationResult describes an observed startup outcome for an instance created by this call.
// Ready is true for first readiness and false for startup failure before readiness; unknown outcomes are omitted.
// UID identifies the backend instance, not the Sandbox ID used for delivery.
type SandboxCreationResult struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Ready     bool   `json:"ready"`
	// Duration is derived only from a trusted creation time and the first Ready transition;
	// nil means it is unavailable.
	Duration *time.Duration `json:"duration,omitempty"`
}

// PickSandboxFailure describes a group of claim attempts that failed with the same picked sandbox and reason.
type PickSandboxFailure struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

func (m *Metrics) String() string {
	var lastErrStr string
	if m.LastError != nil {
		// Replace newlines and control characters to ensure single-line output
		lastErrStr = sanitizeErrorMessage(m.LastError)
	}
	return fmt.Sprintf("ClaimMetrics{Retries: %d, Total: %v, Wait: %v, RetryCost: %v, PickAndLock: %v, LockType: %v, WaitReady: %v, InitRuntime: %v, CSIMount: %v, SecurityToken: %v, TrafficToken: %v, LastError: %v}",
		m.Retries, m.Total, m.Wait, m.RetryCost, m.PickAndLock, m.LockType, m.WaitReady, m.InitRuntime, m.CSIMount, m.SecurityToken, m.TrafficToken, lastErrStr)
}

// RecordPickSandboxFailure records one failed claim attempt and aggregates repeated key/reason pairs.
func (m *Metrics) RecordPickSandboxFailure(key string, err error) {
	if err == nil {
		return
	}
	m.mergePickSandboxFailure(PickSandboxFailure{
		Key:    key,
		Reason: sanitizeErrorMessage(err),
		Count:  1,
	})
}

// MergePickSandboxFailures merges pre-aggregated pick failure records into the metrics.
func (m *Metrics) MergePickSandboxFailures(failures []PickSandboxFailure) {
	for _, failure := range failures {
		m.mergePickSandboxFailure(failure)
	}
}

func (m *Metrics) mergePickSandboxFailure(failure PickSandboxFailure) {
	if failure.Count <= 0 {
		failure.Count = 1
	}
	for i := range m.PickSandboxFailures {
		if m.PickSandboxFailures[i].Key == failure.Key && m.PickSandboxFailures[i].Reason == failure.Reason {
			m.PickSandboxFailures[i].Count += failure.Count
			return
		}
	}
	m.PickSandboxFailures = append(m.PickSandboxFailures, failure)
}

func sanitizeErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	replacer := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")
	return replacer.Replace(err.Error())
}
