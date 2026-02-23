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
