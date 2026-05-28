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

package proxyutils

import (
	"fmt"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy/types"
	"github.com/openkruise/agents/pkg/utils"
)

// GetRuntimeURL resolves the agent-runtime endpoint for a Sandbox.
//
// Lookup order:
//  1. AnnotationRuntimeURL on the Sandbox object.
//  2. AnnotationEnvdURL on the Sandbox object (legacy key, kept for backwards compatibility).
//  3. Pod IP from the cached route plus the well-known consts.RuntimePort, used as a fallback
//     while the controller has not yet stamped the URL annotation.
//
// Returns an empty string when none of the sources is usable (e.g. the pod has not been scheduled
// yet). Callers must treat an empty result as "not ready" and either skip or retry.
func GetRuntimeURL(sbx *agentsv1alpha1.Sandbox) string {
	if sbx == nil {
		return ""
	}
	annotations := sbx.GetAnnotations()
	if u := annotations[agentsv1alpha1.AnnotationRuntimeURL]; u != "" {
		return u
	}
	if u := annotations[agentsv1alpha1.AnnotationEnvdURL]; u != "" { // legacy
		return u
	}
	route := GetRouteFromSandbox(sbx)
	if route.IP == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", route.IP, utils.RuntimePort)
}

func GetRouteFromSandbox(s *agentsv1alpha1.Sandbox) types.Route {
	state, _ := utils.GetSandboxState(s)
	if s.Status.PodInfo.PodIP == "" {
		state = agentsv1alpha1.SandboxStateCreating
	}
	return types.Route{
		IP:              s.Status.PodInfo.PodIP,
		ID:              utils.GetSandboxID(s),
		UID:             s.GetUID(),
		Owner:           s.GetAnnotations()[agentsv1alpha1.AnnotationOwner],
		State:           state,
		ResourceVersion: s.GetResourceVersion(),
	}
}
