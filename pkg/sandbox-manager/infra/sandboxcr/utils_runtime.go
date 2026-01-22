package sandboxcr

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils/sandbox-manager/proxyutils"
	"k8s.io/klog/v2"
)

func InitRuntime(ctx context.Context, sbx *Sandbox, opts infra.InitRuntimeOptions) (time.Duration, error) {
	log := klog.FromContext(ctx).WithValues("sandboxID", sbx.GetName(), "envVars", opts.EnvVars)
	start := time.Now()
	initBody, err := json.Marshal(map[string]any{
		"envVars":     opts.EnvVars,
		"accessToken": opts.AccessToken,
	})
	if err != nil {
		log.Error(err, "failed to marshal initBody")
		return 0, err
	}
	runtimeURL := sbx.GetRuntimeURL()
	if runtimeURL == "" {
		log.Error(nil, "runtimeURL is empty")
		return 0, err
	}
	url := runtimeURL + "/init"
	log.Info("sending request to runtime", "url", url)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(initBody))
	if err != nil {
		log.Error(err, "failed to create request")
		return 0, err
	}

	resp, err := proxyutils.ProxyRequest(r)
	if err != nil {
		log.Error(err, "init runtime request failed")
		return time.Since(start), err
	}
	return time.Since(start), resp.Body.Close()
}
