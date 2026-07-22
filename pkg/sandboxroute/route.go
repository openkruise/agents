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
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/resourceversion"

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

// Validate checks the metadata required before a route enters a Store.
func (r Route) Validate() error {
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
	if _, err := resourceversion.CompareResourceVersion(r.ResourceVersion, r.ResourceVersion); err != nil {
		return fmt.Errorf("route resource version is invalid: %w", err)
	}
	return nil
}

// AdmitPeerRoute normalizes and validates a Route received from a peer.
func AdmitPeerRoute(route Route) (Route, bool, error) {
	route, legacy, err := normalizePeerRoute(route)
	if err != nil {
		return Route{}, false, err
	}
	if err := route.Validate(); err != nil {
		return Route{}, false, err
	}
	return route, legacy, nil
}

// normalizePeerRoute upgrades a legacy ID-only peer payload to a full Route.
// Routes produced by current peers pass through unchanged.
func normalizePeerRoute(route Route) (Route, bool, error) {
	switch {
	case route.Namespace != "" && route.Name != "":
		return route, false, nil
	case route.Namespace != "" || route.Name != "":
		return Route{}, false, fmt.Errorf("route namespace and name must both be set or both be empty")
	}

	key, err := utils.ParseLegacySandboxID(route.ID)
	if err != nil {
		return Route{}, false, err
	}
	route.Namespace = key.Namespace
	route.Name = key.Name
	return route, true, nil
}
