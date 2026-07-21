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

package controller

import (
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/metrics"
	"github.com/openkruise/agents/pkg/sandboxid"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/utils"
)

type gatewayProjectionSource struct {
	*agentsv1alpha1.Sandbox
	state  string
	reason string
}

var _ sandboxroute.ProjectionSource = (*gatewayProjectionSource)(nil)

func newGatewayProjectionSource(sandbox *agentsv1alpha1.Sandbox) *gatewayProjectionSource {
	state, reason := utils.GetSandboxState(sandbox)
	return &gatewayProjectionSource{Sandbox: sandbox, state: state, reason: reason}
}

func (s *gatewayProjectionSource) GetIP() string {
	return s.Status.PodInfo.PodIP
}

func (s *gatewayProjectionSource) GetState() (string, string) {
	return s.state, s.reason
}

func (s *gatewayProjectionSource) GetID() string {
	id, format := sandboxid.ResolveWithFormat(s.Sandbox)
	if format == sandboxid.FormatLegacy {
		metrics.RecordSandboxIDLegacyResolutionGateway()
	}
	return id
}

func (s *gatewayProjectionSource) GetAccessToken() string {
	return utils.GetAccessToken(s.Sandbox)
}

func (s *gatewayProjectionSource) RequiresTrafficAuth() bool {
	return identity.IsAccessTokenRequested(s.Sandbox)
}
