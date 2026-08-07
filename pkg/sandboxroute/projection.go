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

package sandboxroute

import (
	"errors"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/sandboxid"
	"github.com/openkruise/agents/pkg/utils"
)

// ProjectSandbox constructs a Route from a Sandbox CR. ID, access-token, and
// traffic-auth derivation are centralized here so every component projects
// routes with identical compatibility policy. A sandbox without a Pod IP is
// always treated as creating, matching the existing manager and gateway
// behavior.
func ProjectSandbox(sandbox *agentsv1alpha1.Sandbox) (Route, error) {
	if sandbox == nil {
		return Route{}, errors.New("project route: sandbox is nil")
	}

	ip := sandbox.Status.PodInfo.PodIP
	state := agentsv1alpha1.SandboxStateCreating
	if ip != "" {
		state, _ = utils.GetSandboxState(sandbox)
	}
	annotations := sandbox.GetAnnotations()

	route := Route{
		IP:                 ip,
		ID:                 sandboxid.Resolve(sandbox),
		Namespace:          sandbox.Namespace,
		Name:               sandbox.Name,
		UID:                sandbox.UID,
		Owner:              annotations[agentsv1alpha1.AnnotationOwner],
		State:              state,
		ResourceVersion:    sandbox.ResourceVersion,
		AccessToken:        utils.GetAccessToken(sandbox),
		RequireTrafficAuth: identity.IsAccessTokenRequested(sandbox),
	}
	if err := route.validate(); err != nil {
		return Route{}, err
	}
	return route, nil
}
