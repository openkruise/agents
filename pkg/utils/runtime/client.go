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

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

const (
	// defaultRuntimeTimeout bounds a single runtime API call when the caller does
	// not configure an explicit timeout via WithRequestTimeout.
	defaultRuntimeTimeout = 30 * time.Second

	// accessTokenHeader is the header the agent-runtime BearerAuthMiddleware reads
	// to authenticate a request (see pkg/agent-runtime/auth). Despite the
	// middleware name it validates this header value against the configured
	// access tokens rather than a standard Authorization: Bearer scheme.
	accessTokenHeader = "X-Access-Token"

	// maxRuntimeResponseBody bounds how much of a runtime response body is read,
	// guarding against an unexpectedly large body. Runtime JSON responses are tiny.
	maxRuntimeResponseBody = 1 << 20 // 1 MiB
)

// defaultRetryBackoff is the retry schedule applied by default (retry is enabled
// unless overridden via WithRetry). It mirrors the schedule used by InitRuntime
// so mount and init behave alike when a freshly created runtime is not yet
// reachable: ~200ms, 400ms, 800ms, 1.6s, capped at 10s, up to 5 attempts.
var defaultRetryBackoff = wait.Backoff{
	Duration: 200 * time.Millisecond,
	Factor:   2.0,
	Steps:    5,
	Cap:      10 * time.Second,
}

// Runtime is the control-plane client handle to a single sandbox's agent-runtime.
//
// It groups the runtime's HTTP/HTTPS API surface by capability so callers depend
// on stable, intention-revealing methods rather than on raw endpoints. New
// capabilities are introduced as additional sub-interface accessors (e.g. a
// future Process() or Filesystem()) without changing existing signatures.
//
// A Runtime is bound to one sandbox at construction time; capability methods
// therefore do not take a *Sandbox argument. The transport (plain HTTP or
// HTTPS/mTLS) and addressing are implementation details resolved internally, so
// switching protocols is a construction-time concern, not a per-call one.
//
// Runtime is deliberately policy-neutral: it only performs transport, addressing
// and authentication against the runtime. Callers pass already-encoded request
// payloads (e.g. an opaque driver name and base64 CSI config); the client does
// not resolve PVs, secrets or storage drivers.
type Runtime interface {
	// Storage returns the storage capability group, backed by the runtime
	// /v1/storage/* routes.
	Storage() StorageAPI
}

// Option customizes a Runtime constructed by NewRuntime.
type Option func(*runtimeClient)

// WithHTTPClient injects the http.Client used for runtime calls. Provide a client
// configured with a CA bundle and client certificate to speak HTTPS/mTLS; when
// unset (or nil), http.DefaultClient is used and calls default to plain HTTP.
func WithHTTPClient(c *http.Client) Option {
	return func(rc *runtimeClient) {
		if c != nil {
			rc.client = c
		}
	}
}

// WithRequestTimeout bounds each runtime request. Values <= 0 are ignored and the
// default (defaultRuntimeTimeout) is kept.
func WithRequestTimeout(d time.Duration) Option {
	return func(rc *runtimeClient) {
		if d > 0 {
			rc.timeout = d
		}
	}
}

// WithBaseURL overrides the runtime endpoint that would otherwise be resolved
// from the sandbox via GetRuntimeURL. It is required to target the HTTPS server,
// which the agent-runtime exposes on a separate port from the plain-HTTP one:
// GetRuntimeURL only yields the HTTP endpoint, so an HTTPS/mTLS client must be
// paired with the matching https://host:tlsPort base URL here. A trailing slash
// is tolerated. An empty value is ignored.
func WithBaseURL(baseURL string) Option {
	return func(rc *runtimeClient) {
		if baseURL != "" {
			rc.baseURLOverride = baseURL
		}
	}
}

// WithRetry overrides the retry schedule. Retry is enabled by default
// (defaultRetryBackoff); pass a backoff with Steps <= 1 to effectively disable
// retries. Only transient failures are retried: transport errors, an unresolved
// runtime URL, refresh errors and HTTP 5xx. A 4xx *APIError is permanent and is
// never retried.
func WithRetry(backoff wait.Backoff) Option {
	return func(rc *runtimeClient) {
		rc.backoff = backoff
	}
}

// WithRefresh installs a hook that re-resolves the bound sandbox before each
// attempt. It mirrors InitRuntime's refresh behavior so a runtime URL that the
// controller has not yet stamped (e.g. right after resume/recreate) is picked up
// on a later retry. A nil hook (the default) keeps the sandbox fixed.
func WithRefresh(fn RefreshFunc) Option {
	return func(rc *runtimeClient) {
		rc.refreshFn = fn
	}
}

// runtimeClient is the default Runtime implementation. It resolves the runtime
// endpoint from the bound sandbox (or an explicit override) and speaks the
// runtime HTTP API. It holds no domain policy.
type runtimeClient struct {
	sbx             *agentsv1alpha1.Sandbox
	client          *http.Client
	timeout         time.Duration
	baseURLOverride string
	backoff         wait.Backoff
	refreshFn       RefreshFunc
}

// NewRuntime builds a Runtime bound to sbx. Unless overridden by options it uses
// http.DefaultClient (plain HTTP) and defaultRuntimeTimeout.
func NewRuntime(sbx *agentsv1alpha1.Sandbox, opts ...Option) Runtime {
	rc := &runtimeClient{
		sbx:     sbx,
		client:  http.DefaultClient,
		timeout: defaultRuntimeTimeout,
		backoff: defaultRetryBackoff,
	}
	for _, opt := range opts {
		opt(rc)
	}
	return rc
}

// Storage returns the storage capability group for the bound sandbox.
func (r *runtimeClient) Storage() StorageAPI {
	return &storageAPI{r: r}
}

// resolveBaseURL resolves the runtime endpoint for the given sandbox, preferring
// an explicit override (see WithBaseURL) and otherwise falling back to
// GetRuntimeURL. It returns an empty string when the runtime is not yet
// addressable; callers must treat that as "not ready" (a retryable condition).
func (r *runtimeClient) resolveBaseURL(sbx *agentsv1alpha1.Sandbox) string {
	if r.baseURLOverride != "" {
		return r.baseURLOverride
	}
	return GetRuntimeURL(sbx)
}

// APIError describes a non-2xx response from the runtime. It preserves the HTTP
// status code and the server-provided message so callers can both surface a
// clean reason and decide whether to retry (see IsClientError).
type APIError struct {
	// Path is the runtime API path that was called (e.g. "/v1/storage/mounts").
	Path string
	// StatusCode is the HTTP status returned by the runtime.
	StatusCode int
	// Message is the human-readable reason extracted from the runtime response
	// body (the "message" or "error" JSON field), falling back to the raw body.
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("runtime API %s returned status %d: %s", e.Path, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("runtime API %s returned status %d", e.Path, e.StatusCode)
}

// IsClientError reports whether the runtime rejected the request with a 4xx
// status. Such failures are permanent (bad input, unsupported driver, auth) and
// must not be retried; 5xx failures may be transient and are retryable.
func (e *APIError) IsClientError() bool {
	return e.StatusCode >= 400 && e.StatusCode < 500
}

// call is the shared transport primitive for the capability groups. It sends
// reqBody (when non-nil) as a JSON request to method+path against the resolved
// runtime endpoint, then decodes the JSON response into respOut (when non-nil).
//
// It retries transient failures per the configured backoff (enabled by default;
// see WithRetry): transport errors, an unresolved runtime URL, refresh errors
// and HTTP 5xx are retried, while a 4xx *APIError is permanent and returned
// immediately. Retrying stops as soon as the context is cancelled. When a
// refresh hook is installed (see WithRefresh) the sandbox is re-resolved before
// every attempt so a newly stamped runtime URL is picked up.
//
// It returns:
//   - a plain error for local/transport failures (no endpoint, marshal, dial,
//     timeout, response decode);
//   - an *APIError for any HTTP status >= 400, carrying the status code and the
//     server-provided message so callers can classify and surface it.
//
// The authentication headers mirror the agent-runtime expectations: X-Access-Token
// is the gate enforced by BearerAuthMiddleware; the Basic root credential is
// optional (WithGinAuthenticateUserName ignores an absent one) but kept for
// parity with the other runtime callers.
func (r *runtimeClient) call(ctx context.Context, method, path string, reqBody, respOut any) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(r.sbx)).V(utils.DebugLogLevel)

	// Marshal the request body once: a marshal failure is a permanent,
	// non-retryable programming error.
	var payload []byte
	if reqBody != nil {
		marshalled, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("failed to marshal runtime request body for %s: %w", path, err)
		}
		payload = marshalled
	}

	// sbx is the sandbox used to resolve the endpoint and access token for each
	// attempt; WithRefresh may replace it between retries.
	sbx := r.sbx
	attempt := 0
	do := func() error {
		attempt++
		if r.refreshFn != nil {
			updated, err := r.refreshFn(ctx)
			if err != nil {
				return fmt.Errorf("failed to refresh sandbox before runtime call %s: %w", path, err)
			}
			if updated != nil {
				sbx = updated
			}
		}

		base := r.resolveBaseURL(sbx)
		if base == "" {
			return fmt.Errorf("runtime url not found on sandbox")
		}
		endpoint := strings.TrimRight(base, "/") + path

		var bodyReader io.Reader
		if payload != nil {
			bodyReader = bytes.NewReader(payload)
		}

		reqCtx, cancel := context.WithTimeout(ctx, r.timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, method, endpoint, bodyReader)
		if err != nil {
			return fmt.Errorf("failed to build runtime request for %s: %w", path, err)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token := utils.GetAccessToken(sbx); token != "" {
			req.Header.Set(accessTokenHeader, token)
		}
		// Optional Basic credential (root, empty password). BearerAuthMiddleware does
		// not consume it; WithGinAuthenticateUserName resolves it to the root user
		// when present and ignores it when absent.
		req.Header.Set("Authorization", "Basic cm9vdDo=") // Basic root:

		start := time.Now()
		log.Info("sending runtime request", "method", method, "endpoint", endpoint, "attempt", attempt)

		resp, err := r.client.Do(req)
		if err != nil {
			return fmt.Errorf("failed to call runtime API %s: %w", path, err)
		}
		defer func() {
			// Drain and close to enable connection reuse.
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()

		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxRuntimeResponseBody))

		if resp.StatusCode >= http.StatusBadRequest {
			return &APIError{
				Path:       path,
				StatusCode: resp.StatusCode,
				Message:    extractErrorMessage(bodyBytes),
			}
		}

		if respOut != nil && len(bodyBytes) > 0 {
			if err := json.Unmarshal(bodyBytes, respOut); err != nil {
				return fmt.Errorf("failed to decode runtime API %s response: %w", path, err)
			}
		}

		log.Info("runtime request completed", "method", method, "endpoint", endpoint,
			"statusCode", resp.StatusCode, "cost", time.Since(start), "attempt", attempt)
		return nil
	}

	return retry.OnError(r.backoff, retriableRuntimeError(ctx), do)
}

// retriableRuntimeError builds the retry predicate for call. It stops retrying
// once the context is cancelled, treats a 4xx *APIError as permanent, and treats
// everything else (transport errors, unresolved URL, refresh failures, 5xx) as
// transient.
func retriableRuntimeError(ctx context.Context) func(error) bool {
	return func(err error) bool {
		if err == nil {
			return false
		}
		if ctx.Err() != nil {
			return false
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return !apiErr.IsClientError()
		}
		return true
	}
}

// extractErrorMessage pulls a human-readable reason from a runtime error body.
// The runtime emits two shapes: handlers return {"message": "..."} while the
// auth/permission middlewares return {"error": "..."}. It falls back to the
// trimmed raw body when neither field is present.
func extractErrorMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var parsed struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		if parsed.Message != "" {
			return parsed.Message
		}
		if parsed.Error != "" {
			return parsed.Error
		}
	}
	return strings.TrimSpace(string(body))
}
