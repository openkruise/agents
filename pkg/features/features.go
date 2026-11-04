/*
Copyright 2025 The Kruise Authors.

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

package features

import (
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/component-base/featuregate"

	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

const (
	// SandboxGate enable Sandbox-controller to create a sandbox pod.
	SandboxGate featuregate.Feature = "Sandbox"

	// SandboxSetGate enable SandboxSet-controller to create a sandbox pod.
	SandboxSetGate featuregate.Feature = "SandboxSet"

	// SandboxClaimGate enable SandboxClaim-controller to claim sandboxes from SandboxSet pools.
	SandboxClaimGate featuregate.Feature = "SandboxClaim"

	// SandboxCreatePodRateLimitGate enables rate limiting for sandbox controller creating pod.
	SandboxCreatePodRateLimitGate featuregate.Feature = "SandboxCreatePodRateLimitGate"

	// SandboxCreatePodInjectConfigGate enables injecting sidecar config for sandbox pod
	SandboxCreatePodInjectConfigGate featuregate.Feature = "SandboxCreatePodInjectConfigGate"

	// CachePodLabelSelectorGate enables label selector filtering on the Pod informer cache
	// to reduce memory consumption.
	CachePodLabelSelectorGate featuregate.Feature = "CachePodLabelSelector"

	// SandboxInPlaceResourceResizeGate enables in-place resource resize when claiming sandboxes.
	SandboxInPlaceResourceResizeGate featuregate.Feature = "SandboxInPlaceResourceResize"

	// SandboxMultiClusterNaming enables embedding a cluster ID hash in the Sandbox generateName
	// to prevent naming collisions across multiple clusters.
	SandboxMultiClusterNaming featuregate.Feature = "SandboxMultiClusterNaming"

	// SandboxUpgradeResumeFromFailedStepGate enables phase-aware resume behavior for sandbox upgrades.
	// When enabled, the sandbox controller resumes from the previously failed upgrade step instead of
	// always restarting from PreUpgrade after a template change.
	SandboxUpgradeResumeFromFailedStepGate featuregate.Feature = "SandboxUpgradeResumeFromFailedStep"

	// SecurityIdentityProviderGate enables issuing sandbox access tokens via an external
	// identity provider service instead of random UUID generation.
	SecurityIdentityProviderGate featuregate.Feature = "SecurityIdentityProvider"

	// SandboxPauseCheckpointGate historically enabled creating Checkpoint CRs
	// during sandbox pause to capture pod state for resume. It is now always
	// on and no longer consumed anywhere; the definition is kept so existing
	// --feature-gates configurations referencing it do not fail validation.
	SandboxPauseCheckpointGate featuregate.Feature = "SandboxPauseCheckpoint"

	// CommitGate enables the Commit controller to commit container images from Sandbox pods.
	CommitGate featuregate.Feature = "Commit"

	// PoolAutoscalerGate enables the PoolAutoscaler controller for automatic
	// scaling of SandboxSet warming pools.
	PoolAutoscalerGate featuregate.Feature = "PoolAutoscaler"

	// AutoPauseControllerGate enables probe-driven auto-pause/resume logic
	// in the sandbox controller. When enabled, the controller injects
	// Spec.Probes into the pod, reads probe results from Pod.Status.Conditions,
	// and evaluates AutoPausePolicy to automatically pause/resume sandboxes.
	// When disabled, no probe is injected, no pause/resume decision is made,
	// and no probe results are consumed. It defaults to on: the only cluster
	// dependency, OpenKruise, is guarded separately by KruiseIntegrationGate,
	// and Sandboxes without Spec.Probes are untouched either way.
	AutoPauseControllerGate featuregate.Feature = "AutoPauseController"

	// KruiseIntegrationGate enables the controller's access to OpenKruise
	// cluster resources — today the PodProbeMarker CRD that delivers Spec.Probes
	// to pods on real (non virtual-kubelet) nodes, where kruise-daemon executes
	// them. It only matters while AutoPauseControllerGate is enabled.
	//
	// Enabled, it declares that the cluster runs OpenKruise: a missing
	// PodProbeMarker CRD is logged and reported with a Warning event and keeps
	// real-node reconciles failing, rather than degrading into silently
	// undelivered probes. Disabled, no PodProbeMarker is read or written and no
	// informer is started, so a cluster without OpenKruise still runs probes on
	// virtual-kubelet nodes through the kruise.io/podprobe annotation; real-node
	// probe conditions then stay Unknown and pause decisions fail closed. Markers
	// left over from when the gate was on keep executing until their owning pod is
	// garbage-collected.
	KruiseIntegrationGate featuregate.Feature = "KruiseIntegration"
)

var defaultFeatureGates = map[featuregate.Feature]featuregate.FeatureSpec{
	SandboxGate:                            {Default: true, PreRelease: featuregate.Alpha},
	SandboxSetGate:                         {Default: true, PreRelease: featuregate.Alpha},
	SandboxClaimGate:                       {Default: true, PreRelease: featuregate.Alpha},
	SandboxCreatePodRateLimitGate:          {Default: false, PreRelease: featuregate.Alpha},
	SandboxCreatePodInjectConfigGate:       {Default: false, PreRelease: featuregate.Alpha},
	CachePodLabelSelectorGate:              {Default: true, PreRelease: featuregate.Alpha},
	SandboxInPlaceResourceResizeGate:       {Default: true, PreRelease: featuregate.Alpha},
	SandboxMultiClusterNaming:              {Default: false, PreRelease: featuregate.Alpha},
	SandboxUpgradeResumeFromFailedStepGate: {Default: true, PreRelease: featuregate.Alpha},
	SecurityIdentityProviderGate:           {Default: false, PreRelease: featuregate.Alpha},
	SandboxPauseCheckpointGate:             {Default: false, PreRelease: featuregate.Alpha},
	CommitGate:                             {Default: false, PreRelease: featuregate.Alpha},
	PoolAutoscalerGate:                     {Default: true, PreRelease: featuregate.Alpha},
	AutoPauseControllerGate:                {Default: true, PreRelease: featuregate.Alpha},
	KruiseIntegrationGate:                  {Default: false, PreRelease: featuregate.Alpha},
}

func init() {
	runtime.Must(utilfeature.DefaultMutableFeatureGate.Add(defaultFeatureGates))
}
