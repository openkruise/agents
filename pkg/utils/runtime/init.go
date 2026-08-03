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

package runtime

// init.go groups the runtime initialization capability: resolving the init
// request from sandbox annotations and driving the /init handshake against the
// runtime sidecar. The InitAPI capability group rides on the shared call
// transport, so retry/refresh semantics and HTTPS with forced resolution
// (WithTLS) apply to init exactly as they do to the other capability groups.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/logs"
	"github.com/openkruise/agents/pkg/utils/runtime/config"
)

// initPath is the route of the agent-runtime init handshake endpoint. The
// handler authenticates via the accessToken carried in the JSON body (not the
// X-Access-Token header) and answers 204 No Content on success.
const initPath = "/init"

// initRequestTimeout bounds a single /init attempt, preserving the historical
// InitRuntime behavior (1s per attempt across the retry schedule).
const initRequestTimeout = time.Second

// InitAPI is the runtime initialization capability group. It maps to the
// /init handshake endpoint exposed by agent-runtime.
type InitAPI interface {
	// Init drives the POST /init handshake, delivering env vars and the access
	// token in the JSON body. A 204/2xx response means the runtime accepted the
	// init data. When opts.ReInit is true, a 401 response is treated as success:
	// the runtime already holds a token, i.e. it is already initialized.
	Init(ctx context.Context, opts config.InitRuntimeOptions) error
}

// initAPI is the default InitAPI implementation. It delegates transport to the
// owning runtimeClient and carries only the init-specific status mapping.
type initAPI struct {
	r *runtimeClient
}

// Init implements InitAPI by posting the init options to the runtime init
// endpoint through the shared call transport (retry, refresh and optional
// TLS/pinned addressing included).
func (i *initAPI) Init(ctx context.Context, opts config.InitRuntimeOptions) error {
	err := i.r.call(ctx, http.MethodPost, initPath, opts, nil)
	// The runtime rejects an init whose body token is absent or no longer
	// matches its stored token with 401. On a re-init (resume/recreate) that
	// means the runtime is already initialized, which is the desired end state.
	// call never retries a 4xx, so this classification happens exactly once.
	var apiErr *APIError
	if opts.ReInit && errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
		klog.FromContext(ctx).Info("init runtime returned 401, treated as success because ReInit is true",
			"sandbox", klog.KObj(i.r.sbx))
		return nil
	}
	return err
}

// GetInitRuntimeRequest parses init runtime configuration from object annotations.
func GetInitRuntimeRequest(s metav1.Object) (*config.InitRuntimeOptions, error) {
	// Build initRuntimeOpts from annotation at the beginning
	var initRuntimeOpts *config.InitRuntimeOptions
	if initRuntimeRequest := s.GetAnnotations()[agentsv1alpha1.AnnotationInitRuntimeRequest]; initRuntimeRequest != "" {
		var opts config.InitRuntimeOptions
		if err := json.Unmarshal([]byte(initRuntimeRequest), &opts); err != nil {
			return nil, fmt.Errorf("failed to unmarshal init runtime request: %w", err)
		}
		opts.ReInit = true
		initRuntimeOpts = &opts
	}
	return initRuntimeOpts, nil
}

// InitRuntime sends an init request to the sandbox runtime sidecar. It is a
// thin adapter over the Runtime Init capability kept for signature
// compatibility with existing callers.
//
// The sbx parameter is the raw Sandbox API object. When opts.SkipRefresh is
// false and refreshFn is provided, the sandbox is re-resolved before each retry
// attempt so a freshly stamped runtime URL (or Pod IP in TLS mode) is picked
// up. Additional Options (e.g. WithTLS to reach the runtime over HTTPS with
// forced resolution) are appended after the init defaults and may override
// them.
func InitRuntime(ctx context.Context, sbx *agentsv1alpha1.Sandbox, opts config.InitRuntimeOptions, refreshFn RefreshFunc, rtOpts ...Option) (time.Duration, error) {
	ctx = logs.Extend(ctx, "action", "initRuntime")
	start := time.Now()
	options := []Option{WithRequestTimeout(initRequestTimeout)}
	if !opts.SkipRefresh && refreshFn != nil {
		options = append(options, WithRefresh(refreshFn))
	}
	options = append(options, rtOpts...)
	err := NewRuntime(sbx, options...).Init().Init(ctx, opts)
	return time.Since(start), err
}
