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
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"k8s.io/apimachinery/pkg/types"
)

func GetRouteFromSandbox(s *agentsv1alpha1.Sandbox) Route {
	state, _ := utils.GetSandboxState(s)
	if s.Status.PodInfo.PodIP == "" {
		state = agentsv1alpha1.SandboxStateCreating
	}
	return Route{
		IP:              s.Status.PodInfo.PodIP,
		ID:              utils.GetSandboxID(s),
		UID:             s.GetUID(),
		Owner:           s.GetAnnotations()[agentsv1alpha1.AnnotationOwner],
		State:           state,
		ResourceVersion: s.GetResourceVersion(),
	}
}

// ShouldDeleteRoute reports whether a route in the given sandbox state should be
// removed from a route table rather than stored.
//
// Only a terminal (dead) sandbox loses its route. Every other state — including
// creating, paused and running — is stored with its state, and consumers (e.g.
// the gateway filter) decide routability from Route.State. Centralising this rule
// keeps the manager and gateway /refresh handlers from diverging on the same event.
func ShouldDeleteRoute(state string) bool {
	return state == agentsv1alpha1.SandboxStateDead
}

// Route represents an internal sandbox routing rule.
// Moved from pkg/proxy to break the pkg/utils → pkg/proxy layer violation.
type Route struct {
	IP              string    `json:"ip"`
	ID              string    `json:"id"`
	UID             types.UID `json:"uid"`
	Owner           string    `json:"owner"`
	State           string    `json:"state"`
	ResourceVersion string    `json:"resourceVersion"`
}
