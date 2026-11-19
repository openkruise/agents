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

package filter

import (
	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	serving "github.com/openkruise/agents/pkg/sandbox-gateway/sessionservice"
)

// FilterFactory creates a per-stream dispatcher. Request headers determine which
// independent filter handles the stream, including all subsequent callbacks.
func FilterFactory(c interface{}, callbacks api.FilterCallbackHandler) api.StreamFilter {
	return &routingFilter{StreamFilter: &api.PassThroughStreamFilter{}, config: c.(*FilterConfig), callbacks: callbacks}
}

type routingFilter struct {
	api.StreamFilter
	config    *FilterConfig
	callbacks api.FilterCallbackHandler
}

func (f *routingFilter) DecodeHeaders(header api.RequestHeaderMap, endStream bool) api.StatusType {
	headers := make(map[string]string)
	header.Range(func(key, value string) bool {
		headers[key] = value
		return true
	})
	authority := f.config.Adapter.ParseRequest(headers).Authority
	// Explicit Sandbox headers retain E2B routing and authentication, regardless
	// of whether the gateway is reached through a Kubernetes Service hostname.
	_, sandboxHeaderPresent := headers[f.config.GetSandboxHeaderName()]
	if _, _, matched, _ := serving.ParseAuthority(authority); matched && !sandboxHeaderPresent {
		f.StreamFilter = &sessionServiceFilter{
			callbacks:      f.callbacks,
			config:         f.config.Config,
			servingService: f.config.servingService,
			authority:      authority,
		}
	} else {
		f.StreamFilter = &sandboxFilter{
			callbacks:      f.callbacks,
			config:         f.config.Config,
			adapter:        f.config.Adapter,
			jwtAuthManager: f.config.jwtAuthManager,
		}
	}
	return f.StreamFilter.DecodeHeaders(header, endStream)
}
