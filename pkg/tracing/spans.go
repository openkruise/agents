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

package tracing

// Span name constants for sandbox-manager.
const (
	SpanManagerClaimSandbox      = "manager.ClaimSandbox"
	SpanManagerCloneSandbox      = "manager.CloneSandbox"
	SpanManagerDeleteSandbox     = "manager.DeleteSandbox"
	SpanManagerPauseSandbox      = "manager.PauseSandbox"
	SpanManagerResumeSandbox     = "manager.ResumeSandbox"
	SpanManagerCreateSnapshot    = "manager.CreateSnapshot"
	SpanManagerWaitForCheckpoint = "manager.WaitForCheckpoint"

	SpanInfraClaimSandbox     = "infra.ClaimSandbox"
	SpanInfraCloneSandbox     = "infra.CloneSandbox"
	SpanInfraCreateCheckpoint = "infra.CreateCheckpoint"
	SpanInfraProcessCSIMounts = "infra.ProcessCSIMounts"
	SpanInfraPause            = "infra.Pause"
	SpanInfraResume           = "infra.Resume"
	SpanInfraKill             = "infra.Kill"

	SpanProxySyncRoute = "proxy.syncRoute"
)

// Span name constants for sandbox-controller.
const (
	SpanControllerReconcile             = "controller.Reconcile"
	SpanControllerEnsureSandboxRunning  = "controller.EnsureSandboxRunning"
	SpanControllerEnsureSandboxUpdated  = "controller.EnsureSandboxUpdated"
	SpanControllerEnsureSandboxPaused   = "controller.EnsureSandboxPaused"
	SpanControllerEnsureSandboxResumed  = "controller.EnsureSandboxResumed"
	SpanControllerEnsureSandboxUpgraded = "controller.EnsureSandboxUpgraded"
	SpanControllerCreatePod             = "controller.CreatePod"
	SpanControllerDeletePod             = "controller.DeletePod"
	SpanControllerPatchPod              = "controller.PatchPod"
	SpanControllerRemoveFinalizer       = "controller.RemoveFinalizer"
	SpanControllerCheckpoint            = "controller.Checkpoint"
	SpanControllerProcessCSIMounts      = "controller.ProcessCSIMounts"
	SpanControllerUpdateStatus          = "controller.updateSandboxStatus"
)

// Attribute key constants for Spans.
const (
	// AttrRequestID carries the normalized request ID on the manager root Span.
	AttrRequestID           = "request.id"
	AttrSandboxID           = "sandbox.id"
	AttrSandboxName         = "sandbox.name"
	AttrSandboxNamespace    = "sandbox.namespace"
	AttrSandboxPhase        = "sandbox.phase"
	AttrPodName             = "pod.name"
	AttrCheckpointName      = "checkpoint.name"
	AttrPhaseBefore         = "phase.before"
	AttrPhaseAfter          = "phase.after"
	AttrClaimLockType       = "claim.lock_type"
	AttrClaimRetries        = "claim.retries"
	AttrClaimDuration       = "claim.duration"
	AttrCloneCheckpointID   = "clone.checkpoint_id"
	AttrSnapshotKeepRunning = "snapshot.keep_running"
	AttrSnapshotTTL         = "snapshot.ttl"
	AttrCheckpointDuration  = "checkpoint.duration"
	AttrCSIVolumeCount      = "csi.volume_count"
	AttrCSIVolumes          = "csi.volumes"
	AttrRouteID             = "route.id"
	AttrPeersSynced         = "peers.synced"
	AttrReuseTriggered      = "reuse.triggered"

	// AttrReconcileNoop marks a Reconcile (or its EnsureSandbox* child) Span that
	// performed no real write operation. Spans carrying this attribute are dropped
	// by FilteringSpanProcessor to keep empty Reconcile iterations out of traces.
	AttrReconcileNoop = "reconcile.noop"
)

// writeSpanNames is the set of child Span names that represent a real write
// operation. When StartControllerSpan creates any of these, it marks the current
// Reconcile as having written (see MarkWrite), so the enclosing Reconcile and
// EnsureSandbox* Spans are retained instead of being filtered as no-op.
var writeSpanNames = map[string]bool{
	SpanControllerCreatePod:        true,
	SpanControllerDeletePod:        true,
	SpanControllerPatchPod:         true,
	SpanControllerRemoveFinalizer:  true,
	SpanControllerCheckpoint:       true,
	SpanControllerProcessCSIMounts: true,
	SpanControllerUpdateStatus:     true,
}
