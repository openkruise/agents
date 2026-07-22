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
	// AnnotationReservePausedSandboxDuration stores the internal paused-retention policy parsed by pkg/pausedretention.
	AnnotationReservePausedSandboxDuration = InternalPrefix + "reserve-paused-sandbox-duration"

	// AnnotationCleanupCandidate marks an auto-materialised SandboxTemplate as a
	// candidate for garbage collection. A future GC controller will verify that
	// no Sandbox or Checkpoint still references it before performing the actual
	// deletion.
	AnnotationCleanupCandidate = InternalPrefix + "cleanup-candidate"

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
	E2BPrefix      = "e2b." + InternalPrefix
	E2BLabelPrefix = "label:"

	AnnotationEnvdAccessToken = E2BPrefix + "envd-access-token"
	AnnotationEnvdURL         = E2BPrefix + "envd-url"
	// AnnotationCSIVolumeConfig is the annotation key for CSI mount configuration.
	AnnotationCSIVolumeConfig = E2BPrefix + "csi-volume-config"
)

// LabelSandboxUpdateOps marks which SandboxUpdateOps is operating on this sandbox.
const LabelSandboxUpdateOps = InternalPrefix + "update-ops"

const True = "true"
const False = "false"
