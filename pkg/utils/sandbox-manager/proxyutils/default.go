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
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
)

var (
	DefaultGetRouteFunc = stateutils.GetRouteFromSandbox
	DefaultRequestFunc  = requestSandbox
)

func requestSandbox(ctx context.Context, s *agentsv1alpha1.Sandbox, method, path string, port int, body io.Reader) (*http.Response, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s))
	if s.Status.Phase != agentsv1alpha1.SandboxRunning {
		return nil, errors.New("sandbox is not running")
	}
	url := fmt.Sprintf("http://%s:%d%s", s.Status.PodInfo.PodIP, port, path)
	r, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}
	log.Info("requesting sandbox", "url", url)
	return ProxyRequest(r)
}
