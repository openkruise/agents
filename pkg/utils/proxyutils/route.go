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
		AccessToken:     s.GetAnnotations()[agentsv1alpha1.AnnotationRuntimeAccessToken],
	}
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
	AccessToken     string    `json:"accessToken,omitempty"`
}

// String implements fmt.Stringer to prevent AccessToken from being leaked in logs.
// Always prints "***" to avoid revealing whether a token is configured.
func (r Route) String() string {
	return fmt.Sprintf("{IP:%s ID:%s UID:%s Owner:%s State:%s ResourceVersion:%s AccessToken:***}",
		r.IP, r.ID, r.UID, r.Owner, r.State, r.ResourceVersion)
}
