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
)

var defaultFeatureGates = map[featuregate.Feature]featuregate.FeatureSpec{
	SandboxGate:                      {Default: true, PreRelease: featuregate.Alpha},
	SandboxSetGate:                   {Default: true, PreRelease: featuregate.Alpha},
	SandboxClaimGate:                 {Default: true, PreRelease: featuregate.Alpha},
	SandboxCreatePodRateLimitGate:    {Default: false, PreRelease: featuregate.Alpha},
	SandboxCreatePodInjectConfigGate: {Default: false, PreRelease: featuregate.Alpha},
	CachePodLabelSelectorGate:        {Default: true, PreRelease: featuregate.Alpha},
	SandboxInPlaceResourceResizeGate: {Default: true, PreRelease: featuregate.Alpha},
}

func init() {
	runtime.Must(utilfeature.DefaultMutableFeatureGate.Add(defaultFeatureGates))
}
