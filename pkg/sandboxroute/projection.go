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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// ProjectionSource exposes the backend-neutral Sandbox data needed to construct a Route.
type ProjectionSource interface {
	metav1.Object
	GetIP() string
	GetState() (state, reason string)
	GetID() string
	GetAccessToken() string
	RequiresTrafficAuth() bool
}

// ProjectRoute constructs a Route from a projection-ready Sandbox source.
func ProjectRoute(source ProjectionSource) (Route, error) {
	if source == nil {
		return Route{}, errors.New("project route: source is nil")
	}

	ip := source.GetIP()
	annotations := source.GetAnnotations()

	return AdmitRoute(Route{
		IP:                 ip,
		ID:                 source.GetID(),
		Namespace:          source.GetNamespace(),
		Name:               source.GetName(),
		UID:                source.GetUID(),
		Owner:              annotations[agentsv1alpha1.AnnotationOwner],
		State:              stateOf(source, ip),
		ResourceVersion:    source.GetResourceVersion(),
		AccessToken:        source.GetAccessToken(),
		RequireTrafficAuth: source.RequiresTrafficAuth(),
	})
}

// stateOf returns the routing-normalized state. A source without a Pod IP is
// always treated as creating, matching the existing manager and gateway behavior.
func stateOf(source ProjectionSource, ip string) string {
	state, _ := source.GetState()
	if ip == "" {
		return agentsv1alpha1.SandboxStateCreating
	}
	return state
}
