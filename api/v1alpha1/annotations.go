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

package v1alpha1

const (
	InternalPrefix = "agents.kruise.io/"

	AnnotationLock               = InternalPrefix + "lock"
	AnnotationOwner              = InternalPrefix + "owner"
	AnnotationClaimTime          = InternalPrefix + "claim-timestamp"
	AnnotationRestoreFrom        = InternalPrefix + "restore-from"
	AnnotationInitRuntimeRequest = InternalPrefix + "init-runtime-request"
	AnnotationSandboxID          = InternalPrefix + "sandbox-id"
	AnnotationMemberlistURL      = InternalPrefix + "memberlist-url"

	// AnnotationCleanupEnabled marks a sandbox as supporting recycle.
	AnnotationCleanupEnabled = InternalPrefix + "cleanup-enabled"
	// AnnotationCleanup triggers the sandbox recycle flow. Removed by the controller after successful recycle.
	AnnotationCleanup = InternalPrefix + "cleanup"
	// AnnotationCleanupRetainOnFailure controls how long the sandbox is retained after recycle failure.
	// Accepts a Go duration string (e.g., "5m") — the sandbox is retained for that duration and then
	// deleted via ShutdownTime. By default (unset), the sandbox is deleted immediately after recycle failure.
	// If the value is invalid, the sandbox is also deleted immediately with a warning log.
	AnnotationCleanupRetainOnFailure = InternalPrefix + "cleanup-retain-on-failure"
	// AnnotationUpdatedMetadataInClaim stores the keys of labels/annotations added or modified
	// during the claim flow (JSON format, keys only). Used by the recycle flow to reset metadata.
	AnnotationUpdatedMetadataInClaim = InternalPrefix + "updated-metadata-in-claim"

	AnnotationRuntimeURL         = InternalPrefix + "runtime-url"
	AnnotationRuntimeAccessToken = InternalPrefix + "runtime-access-token"
	// AnnotationReservePausedSandboxDuration stores the internal paused-retention policy parsed by pkg/pausedretention.
	AnnotationReservePausedSandboxDuration = InternalPrefix + "reserve-paused-sandbox-duration"

	// AnnotationCleanupCandidate marks an auto-materialised SandboxTemplate as a
	// candidate for garbage collection. A future GC controller will verify that
	// no Sandbox or Checkpoint still references it before performing the actual
	// deletion.
	AnnotationCleanupCandidate = InternalPrefix + "cleanup-candidate"

	// SandboxAnnotationPriority is the annotation key for sandbox priority.
	// If not set, the default value is 0.
	// Larger values indicate higher priority.
	// Note: SandboxSet creates sandboxes with priority 0 by default.
	// Sandbox Manager or Sandbox Claim creates high-priority sandboxes by default.
	SandboxAnnotationPriority = "agents.kruise.io/sandbox-priority"

	// SandboxHashWithoutImageAndResources represents the key of sandbox hash without image and resources.
	// Deprecated, use SandboxHashImmutablePart instead
	SandboxHashWithoutImageAndResources = "sandbox.agents.kruise.io/hash-without-image-resources"

	// SandboxHashImmutablePart represents the key of sandbox hash than exclude immutable part of sandbox
	// e.g. metadata, image and resources
	SandboxHashImmutablePart = "sandbox.agents.kruise.io/hash-immutable-part"
)

// E2B annotations

const (
	E2BPrefix = "e2b." + InternalPrefix

	AnnotationEnvdAccessToken = E2BPrefix + "envd-access-token"
	AnnotationEnvdURL         = E2BPrefix + "envd-url"
	// AnnotationCSIVolumeConfig is the annotation key for CSI mount configuration.
	AnnotationCSIVolumeConfig = E2BPrefix + "csi-volume-config"
)
