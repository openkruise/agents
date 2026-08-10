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

// common SandboxSet annotations

const (
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
)

// E2B annotations

const (
	E2BPrefix      = "e2b." + InternalPrefix
	E2BLabelPrefix = "label:"

	AnnotationEnvdAccessToken = E2BPrefix + "envd-access-token"
	AnnotationEnvdURL         = E2BPrefix + "envd-url"
	// AnnotationCSIVolumeConfig is the annotation key for CSI mount configuration.
	AnnotationCSIVolumeConfig = E2BPrefix + "csi-volume-config"
)

// AnnotationUpgradeResumeTrigger is set by SandboxUpdateOps on a paused sandbox
// to trigger the resume phase of a two-phase upgrade. The sandbox controller
// enters the Upgrading phase and resumes the sandbox using the OLD template.
// Once resume succeeds (SandboxUpgradingReasonResumeSucceed), SandboxUpdateOps
// patches the template and removes this annotation to trigger the actual
// pod replacement.
const AnnotationUpgradeResumeTrigger = InternalPrefix + "upgrade-resume-trigger"

// LabelSandboxUpdateOps marks which SandboxUpdateOps is operating on this sandbox.
const LabelSandboxUpdateOps = InternalPrefix + "update-ops"

// LabelSandboxUpgradeFailed marks a sandbox whose upgrade has failed.
// The controller sets it when the Upgrading condition reports a failure reason
// and removes it once the sandbox is no longer in a failed state.
const LabelSandboxUpgradeFailed = InternalPrefix + "upgrade-failed"

const True = "true"
const False = "false"
