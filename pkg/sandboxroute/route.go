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
)

// Shape identifies how a route participates in routing state.
type Shape string

const (
	// ShapeFull identifies an ObjectKey-backed route.
	ShapeFull Shape = "full"
	// ShapeIDOnly identifies a compatibility route without an ObjectKey.
	ShapeIDOnly Shape = "id_only"
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

// Shape returns whether the route is full or ID-only and rejects partial ObjectKeys.
func (r Route) Shape() (Shape, error) {
	switch {
	case r.Namespace != "" && r.Name != "":
		return ShapeFull, nil
	case r.Namespace == "" && r.Name == "":
		return ShapeIDOnly, nil
	default:
		return "", fmt.Errorf("route namespace and name must both be set or both be empty")
	}
}

// Validate checks the metadata required before a route enters a Store.
func (r Route) Validate() error {
	if _, err := r.Shape(); err != nil {
		return err
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
	return nil
}
