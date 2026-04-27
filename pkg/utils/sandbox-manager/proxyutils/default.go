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
