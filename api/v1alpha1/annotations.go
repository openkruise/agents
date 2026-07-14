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

// LabelSandboxUpdateOps marks which SandboxUpdateOps is operating on this sandbox.
const LabelSandboxUpdateOps = InternalPrefix + "update-ops"

// AnnotationPodProbe is the annotation key used by the PodProbeMarker Serverless
// protocol. The sandbox controller writes probe definitions to this annotation
// on the Pod, and the agent-runtime sidecar reads them, executes the probes,
// and writes results to Pod.Status.Conditions.
// See: https://openkruise.io/docs/user-manuals/podprobemarker#support-for-serverless-scenarios
const AnnotationPodProbe = "kruise.io/podprobe"

const True = "true"
const False = "false"
