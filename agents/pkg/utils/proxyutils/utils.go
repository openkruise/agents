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
	"io"
	"net/http"

	"k8s.io/klog/v2"
)

// ProxyRequest proxies the request to the sandbox
// When apiServerURL is provided, it will proxy through the apiServer (requires restConfig to be provided as well, otherwise connect directly via SandboxIP
func ProxyRequest(r *http.Request) (*http.Response, error) {
	log := klog.FromContext(r.Context())
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return nil, fmt.Errorf("failed to proxy request to sandbox: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func(Body io.ReadCloser) {
			_ = Body.Close()
		}(resp.Body)
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Error(err, "failed to read response body")
			body = []byte(err.Error())
		}
		return resp, fmt.Errorf("sandbox proxy response not 2xx. code: %d, body: %s", resp.StatusCode, string(body))
	}
	return resp, nil
}
