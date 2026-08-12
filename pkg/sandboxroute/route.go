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
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/resourceversion"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/sandboxid"
	"github.com/openkruise/agents/pkg/utils"
)

// Route represents one sandbox routing rule.
type Route struct {
	IP                 string    `json:"ip"`
	ID                 string    `json:"id"`
	Namespace          string    `json:"namespace,omitempty"`
	Name               string    `json:"name,omitempty"`
	UID                types.UID `json:"uid"`
	Owner              string    `json:"owner"`
	State              string    `json:"state"`
	ResourceVersion    string    `json:"resourceVersion"`
	AccessToken        string    `json:"accessToken,omitempty"`
	RequireTrafficAuth bool      `json:"requireTrafficAuth,omitempty"`
}

// String implements fmt.Stringer without exposing the access token.
func (r Route) String() string {
	return fmt.Sprintf(
		"{IP:%s ID:%s Namespace:%s Name:%s UID:%s Owner:%s State:%s ResourceVersion:%s AccessToken:*** RequireTrafficAuth:%t}",
		r.IP, r.ID, r.Namespace, r.Name, r.UID, r.Owner, r.State, r.ResourceVersion, r.RequireTrafficAuth,
	)
}

// ObjectKey returns the route's ObjectKey when it is full.
func (r Route) ObjectKey() (types.NamespacedName, bool) {
	if r.Namespace == "" || r.Name == "" {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{Namespace: r.Namespace, Name: r.Name}, true
}

func (r Route) validate() error {
	if r.Namespace == "" || r.Name == "" {
		return fmt.Errorf("route namespace and name must not be empty")
	}
	if r.ID == "" {
		return fmt.Errorf("route ID must not be empty")
	}
	if r.UID == "" {
		return fmt.Errorf("route UID must not be empty")
	}
	if r.ResourceVersion == "" {
		return fmt.Errorf("route resource version must not be empty")
	}
	if err := validateResourceVersion(r.ResourceVersion); err != nil {
		return fmt.Errorf("route resource version is invalid: %w", err)
	}
	return nil
}

func validateResourceVersion(rv string) error {
	// Self-comparison reuses Kubernetes' positive canonical-integer validation
	// without imposing a resource-version length limit.
	_, err := resourceversion.CompareResourceVersion(rv, rv)
	return err
}

// RouteFromSandbox constructs a Route from a Sandbox CR. ID, access-token,
// and traffic-auth derivation are centralized here so every component
// projects routes with identical compatibility policy. A sandbox without a
// Pod IP is always treated as creating, matching the existing manager and
// gateway behavior.
func RouteFromSandbox(sandbox *agentsv1alpha1.Sandbox) (Route, error) {
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
