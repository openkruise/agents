package proxyutils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
	"k8s.io/klog/v2"
)

var (
	DefaultGetRouteFunc = getRouteFromSandbox
	DefaultRequestFunc  = requestSandbox
)

func getRouteFromSandbox(s *agentsv1alpha1.Sandbox) proxy.Route {
	state, _ := stateutils.GetSandboxState(s)
	if s.Status.PodInfo.PodIP == "" {
		state = agentsv1alpha1.SandboxStateCreating
	}
	return proxy.Route{
		IP:    s.Status.PodInfo.PodIP,
		ID:    stateutils.GetSandboxID(s),
		Owner: s.GetAnnotations()[agentsv1alpha1.AnnotationOwner],
		State: state,
	}
}

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
