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
	// AnnotationSandboxID records the referenced Sandbox ID on associated resources,
	// such as Checkpoints and TrafficPolicies; LabelSandboxID is the authoritative
	// identity for the Sandbox itself.
	AnnotationSandboxID     = InternalPrefix + "sandbox-id"
	AnnotationMemberlistURL = InternalPrefix + "memberlist-url"

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
	// AnnotationRuntimeTLSPort advertises the port (e.g. "49984") on which the
	// sandbox's agent-runtime listens for HTTPS. Its absence means the runtime
	// only speaks plain HTTP. Clients combine it with locally loaded TLS
	// material to decide whether to reach the runtime over HTTPS.
	AnnotationRuntimeTLSPort = InternalPrefix + "runtime-tls-port"
	// AnnotationReservePausedSandboxDuration stores the internal paused-retention policy parsed by pkg/pausedretention.
	AnnotationReservePausedSandboxDuration = InternalPrefix + "reserve-paused-sandbox-duration"

	// AnnotationCleanupCandidate marks an auto-materialised SandboxTemplate as a
	// candidate for garbage collection. A future GC controller will verify that
	// no Sandbox or Checkpoint still references it before performing the actual
	// deletion.
	AnnotationCleanupCandidate = InternalPrefix + "cleanup-candidate"

	// AnnotationSecurityRules carries the normalized inline egress security
	// rules (a JSON array of SecurityRule) for one Sandbox. Only the Sandbox
	// Manager writes it; the Egress Policy Enforcer evaluates the rules under
	// the calling workload's verified identity.
	AnnotationSecurityRules = InternalPrefix + "security-rules"

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

	// AnnotationWakeOnTraffic enables wake-on-traffic for a paused sandbox.
	// When set to "true", the sandbox-gateway will attempt to resume the sandbox
	// by patching Spec.Paused=false when traffic arrives.
	AnnotationWakeOnTraffic = InternalPrefix + "wake-on-traffic"

	// AnnotationWakeTimeoutSeconds stores the auto-pause timeout (in seconds) to
	// apply when the sandbox is woken by traffic. The gateway reads this to set
	// ResumeOptions.Timeout.PauseTime, re-arming auto-pause after wake.
	AnnotationWakeTimeoutSeconds = InternalPrefix + "wake-timeout-seconds"
)

// E2B annotations

const (
	E2BPrefix = "e2b." + InternalPrefix

	AnnotationEnvdAccessToken = E2BPrefix + "envd-access-token"
	AnnotationEnvdURL         = E2BPrefix + "envd-url"
	// AnnotationCSIVolumeConfig is the annotation key for CSI mount configuration.
	AnnotationCSIVolumeConfig = E2BPrefix + "csi-volume-config"
	// MetadataKeySecurityRules is the reserved E2B metadata key whose value is
	// a JSON array of inline security rules. It is consumed by the Sandbox
	// Manager and normalized into AnnotationSecurityRules; tenants can never
	// write AnnotationSecurityRules directly.
	MetadataKeySecurityRules = E2BPrefix + "security-rules"
)

// AnnotationUpgradeResumeTrigger is set by SandboxUpdateOps on a paused sandbox
// to trigger the resume phase of a two-phase upgrade. The sandbox controller
// enters the Upgrading phase and resumes the sandbox using the OLD template.
// Once resume succeeds (SandboxUpgradingReasonResumeSucceed), SandboxUpdateOps
// patches the template and removes this annotation to trigger the actual
// pod replacement.
const AnnotationUpgradeResumeTrigger = InternalPrefix + "upgrade-resume-trigger"

// AnnotationPodProbe is the annotation key used by the PodProbeMarker Serverless
// protocol. The sandbox controller writes probe definitions to this annotation
// on the Pod, and the agent-runtime sidecar reads them, executes the probes,
// and writes results to Pod.Status.Conditions.
// See: https://openkruise.io/docs/user-manuals/podprobemarker#support-for-serverless-scenarios
const AnnotationPodProbe = "kruise.io/podprobe"
