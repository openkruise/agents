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

package sidecarutils

import "k8s.io/apimachinery/pkg/util/sets"

const (
	// ContainerNameRuntimeAgent is the container name for the agent-runtime sidecar.
	ContainerNameRuntimeAgent = "agent-runtime"
	// ContainerNameCSISidecar is the container name for the CSI sidecar.
	ContainerNameCSISidecar = "csi-sidecar"
	// ContainerNameCSIAgentSidecar is the container name for the CSI agent sidecar.
	ContainerNameCSIAgentSidecar = "csi-agent-sidecar"
	// ContainerNameIstioProxy is the container name for the Istio proxy sidecar.
	ContainerNameIstioProxy = "istio-proxy"
)

// RuntimeContainerNames is the set of all runtime/sidecar container names.
var RuntimeContainerNames = sets.NewString(
	ContainerNameRuntimeAgent,
	ContainerNameCSISidecar,
	ContainerNameCSIAgentSidecar,
	ContainerNameIstioProxy,
)
