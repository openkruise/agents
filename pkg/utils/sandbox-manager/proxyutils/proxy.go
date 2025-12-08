package proxyutils

import (
	"fmt"
	"io"
	"net/http"

	"k8s.io/klog/v2"
)

// ProxyRequest proxies the request to the sandbox
// When apiServerURL is provided, it will proxy through the apiServer (requires restConfig to be provided as well, otherwise connect directly via SandboxIP
func ProxyRequest(r *http.Request, path string, port int, ip string) (*http.Response, error) {
	resp, err := proxyRequestDirectly(r, ip, path, port)
	if err != nil {
		klog.ErrorS(err, "failed to proxy request")
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func(Body io.ReadCloser) {
			_ = Body.Close()
		}(resp.Body)
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			klog.ErrorS(err, "failed to read response body")
			body = []byte(err.Error())
		}
		return resp, fmt.Errorf("sandbox proxy response not 2xx. code: %d, body: %s", resp.StatusCode, string(body))
	}
	return resp, nil
}

// proxyRequestDirectly proxies the request directly to the sandbox IP
//
//goland:noinspection HttpUrlsUsage
func proxyRequestDirectly(r *http.Request, ip string, path string, port int) (*http.Response, error) {
	// Construct the target URL for the sandbox
	targetURL := fmt.Sprintf("http://%s:%d%s", ip, port, path)

	// Create a new request to the sandbox
	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to create proxy request: %w", err)
	}

	// Copy headers from the original request
	for key, values := range r.Header {
		for _, value := range values {
			proxyReq.Header.Add(key, value)
		}
	}

	// Create HTTP Client with default transport that supports HTTP/2
	client := &http.Client{
		Transport: http.DefaultTransport,
	}

	// Send the request to the sandbox
	resp, err := client.Do(proxyReq)
	if err != nil {
		return nil, fmt.Errorf("failed to proxy request to sandbox: %w", err)
	}

	return resp, nil
}
