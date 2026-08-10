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
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
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

	// accessTokenHeader carries the sandbox access token that authenticates a
	// request against the agent-runtime. The runtime validates this header
	// value rather than a standard Authorization: Bearer scheme.
	accessTokenHeader = "X-Access-Token"

	// maxRuntimeResponseBody bounds how much of a runtime response body is read,
	// guarding against an unexpectedly large body. Runtime JSON responses are tiny.
	maxRuntimeResponseBody = 1 << 20 // 1 MiB
)

// basicAuthHeader encodes user (with an empty password) as an HTTP Basic
// Authorization header value. It is the single encoding point for the user
// identity that callers opt into: via WithAuthUser on the Runtime client and
// via the AuthUser argument on the files/process data-plane paths.
func basicAuthHeader(user string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"))
}

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
// and authentication against the runtime. Callers pass fully resolved request
// payloads (e.g. a driver name and its CSI NodePublishVolume request); the client
// does not resolve PVs, secrets or storage drivers.
type Runtime interface {
	// Storage returns the storage capability group, backed by the runtime
	// /v1/storage/* routes.
	Storage() StorageAPI
	// Init returns the initialization capability group, backed by the runtime
	// /init handshake endpoint.
	Init() InitAPI
	// Filesystem returns the filesystem capability group, backed by the
	// E2B-compatible multipart /files route and the envd Filesystem service.
	Filesystem() FilesystemAPI
	// Process returns the process capability group, backed by the envd Process
	// service.
	Process() ProcessAPI
}

// Option customizes a Runtime constructed by NewRuntime.
type Option func(*runtimeClient)

// WithRequestTimeout bounds each runtime request. Values <= 0 are ignored and the
// default (defaultRuntimeTimeout) is kept.
func WithRequestTimeout(d time.Duration) Option {
	return func(rc *runtimeClient) {
		if d > 0 {
			rc.timeout = d
		}
	}
}

// WithRetry overrides the retry schedule. Retry is enabled by default
// (defaultRetryBackoff); pass a backoff with Steps: 1 to effectively disable
// retries (Steps: 0 would never send the request at all). Only transient
// failures are retried: transport errors, an unresolved runtime URL, refresh
// errors and HTTP 5xx. A 4xx *APIError is permanent and is never retried.
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

// WithTLS enables automatic HTTPS/mTLS addressing of the agent-runtime: the
// client needs no hand-built URL or http.Client — it dials the sandbox Pod IP
// on RuntimeTLSPort while verifying the server certificate against
// RuntimeServerSNI (overridable via
// WithAuthority), reproducing `curl --resolve`. CABundle is required to verify
// the server; a client certificate is optional (the runtime server uses
// VerifyClientCertIfGiven). An invalid bundle is reported by the first call.
func WithTLS(m TLSBundle) Option {
	return func(rc *runtimeClient) {
		rc.tlsEnabled = true
		rc.tlsBundle = &m
	}
}

// WithAuthority overrides the TLS authority (SNI + certificate-verification
// hostname and Host header) used in TLS mode. It defaults to RuntimeServerSNI,
// which is covered by the runtime server certificate's wildcard SAN. An empty
// value is ignored. It has no effect unless WithTLS is also supplied.
func WithAuthority(host string) Option {
	return func(rc *runtimeClient) {
		if host != "" {
			rc.authority = host
		}
	}
}

// WithTLSPort overrides the HTTPS port dialed in TLS mode. It defaults to
// RuntimeTLSPort. Values <= 0 are ignored. It has no effect unless WithTLS is
// also supplied.
func WithTLSPort(port int) Option {
	return func(rc *runtimeClient) {
		if port > 0 {
			rc.tlsPort = port
		}
	}
}

// WithAuthUser opts the client into sending the user identity as an HTTP
// Basic Authorization header (empty password) on every call. By default no
// Authorization header is sent: control-plane calls carry no user identity to
// resolve. Callers whose target capability resolves an OS user (e.g. future
// process/filesystem capability groups) supply it explicitly. An empty value
// is ignored.
func WithAuthUser(user string) Option {
	return func(rc *runtimeClient) {
		if user != "" {
			rc.authUser = user
		}
	}
}

// runtimeClient is the default Runtime implementation. It resolves the runtime
// endpoint from the bound sandbox and speaks the runtime HTTP API. It holds no
// domain policy.
type runtimeClient struct {
	sbx       *agentsv1alpha1.Sandbox
	client    *http.Client
	timeout   time.Duration
	backoff   wait.Backoff
	refreshFn RefreshFunc
	// authUser is the optional Basic Authorization identity (see WithAuthUser).
	// Empty means no Authorization header is sent.
	authUser string

	// TLS mode (opt-in via WithTLS). When enabled, calls target the HTTPS server
	// on tlsPort and verify the server certificate against authority while
	// dialing the sandbox Pod IP (the curl --resolve behaviour, see
	// newPinnedTransport). tlsClientConfig is built once at construction time;
	// tlsConfigErr captures a construction failure so call() can surface it as a
	// permanent error instead of retrying.
	tlsEnabled      bool
	tlsBundle       *TLSBundle
	authority       string
	tlsPort         int
	tlsClientConfig *tls.Config
	tlsConfigErr    error
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
	// Finalize TLS mode: apply defaults and pre-build the client tls.Config so an
	// invalid bundle surfaces as a permanent (non-retried) error on first call.
	if rc.tlsEnabled {
		if rc.authority == "" {
			rc.authority = RuntimeServerSNI
		}
		if rc.tlsPort == 0 {
			rc.tlsPort = RuntimeTLSPort
		}
		if rc.tlsBundle != nil {
			rc.tlsClientConfig, rc.tlsConfigErr = buildClientTLSConfig(*rc.tlsBundle, rc.authority)
		} else {
			rc.tlsConfigErr = fmt.Errorf("WithTLS requires a TLS bundle")
		}
	}
	return rc
}

// Storage returns the storage capability group for the bound sandbox.
func (r *runtimeClient) Storage() StorageAPI {
	return &storageAPI{r: r}
}

// Init returns the initialization capability group for the bound sandbox.
func (r *runtimeClient) Init() InitAPI {
	return &initAPI{r: r}
}

// Filesystem returns the filesystem capability group for the bound sandbox.
func (r *runtimeClient) Filesystem() FilesystemAPI {
	return &filesystemAPI{r: r}
}

// Process returns the process capability group for the bound sandbox.
func (r *runtimeClient) Process() ProcessAPI {
	return &processAPI{r: r}
}

// resolveTransport resolves the endpoint and the HTTP client for a single call
// against sbx, and is the single place where the plain-HTTP/TLS decision is
// applied. Every capability group routes through it, so the JSON, multipart and
// connect-gRPC protocols cannot drift apart on addressing or TLS.
//
// plainClient is the client used when TLS is off. It is a parameter rather than
// r.client because each protocol keeps its own plaintext client (the shared
// JSON one, runtimeFilesHTTPClient, runtimeGRPCHTTPClient), which tests
// substitute independently; passing it in preserves that seam untouched.
//
// In TLS mode the returned client carries a per-call pinned transport: the
// request is addressed by authority (so the certificate validates) while the
// connection goes to the sandbox Pod IP. The transport is rebuilt per call
// because a refresh may change the Pod IP between attempts.
func (r *runtimeClient) resolveTransport(sbx *agentsv1alpha1.Sandbox, plainClient *http.Client) (string, *http.Client, error) {
	// A TLS-config construction failure is permanent: report it before spending
	// an attempt on a client that cannot handshake.
	if r.tlsEnabled && r.tlsConfigErr != nil {
		return "", nil, fmt.Errorf("invalid runtime TLS configuration: %w", r.tlsConfigErr)
	}
	base := r.resolveBaseURL(sbx)
	if base == "" {
		return "", nil, fmt.Errorf("runtime url not found on sandbox")
	}
	client := plainClient
	if dialIP := r.dialIPFor(sbx); dialIP != "" {
		client = &http.Client{Transport: newPinnedTransport(dialIP, r.tlsPort, r.tlsClientConfig.Clone())}
	}
	return base, client, nil
}

// resolveBaseURL resolves the runtime endpoint for the given sandbox. In TLS
// mode (see WithTLS) it returns the HTTPS authority URL
// (https://<authority>:<tlsPort>) so net/http derives the Host header and TLS
// ServerName from the certificate hostname; the connection is pinned to the
// sandbox Pod IP separately by dialIPFor. Otherwise it falls back to
// GetRuntimeURL. It returns an empty string when the runtime is not yet
// addressable; callers must treat that as "not ready" (a retryable condition).
func (r *runtimeClient) resolveBaseURL(sbx *agentsv1alpha1.Sandbox) string {
	if r.tlsEnabled {
		if sbx == nil || sbx.Status.PodInfo.PodIP == "" {
			return ""
		}
		return fmt.Sprintf("https://%s:%d", r.authority, r.tlsPort)
	}
	return GetRuntimeURL(sbx)
}

// dialIPFor returns the sandbox Pod IP that the TLS connection must be pinned to
// in automatic TLS mode. It returns "" when addressing is delegated to the
// request URL host (plain-HTTP mode), in which case the standard http.Client
// dials by URL host.
func (r *runtimeClient) dialIPFor(sbx *agentsv1alpha1.Sandbox) string {
	if !r.tlsEnabled || sbx == nil {
		return ""
	}
	return sbx.Status.PodInfo.PodIP
}

// transportLogValues describes the transport this client speaks to sbx as
// structured log key-values, so a capability group can state in its own logs
// whether the call goes over HTTPS (with forced resolution) or plaintext HTTP
// without raising the verbosity of the whole call path.
//
// The values reflect the construction-time transport decision, not readiness:
// in TLS mode the endpoint is the fixed authority URL, while dialTarget is
// derived from the Pod IP of the given sandbox and may still be empty before a
// refresh resolves it.
func (r *runtimeClient) transportLogValues(sbx *agentsv1alpha1.Sandbox) []any {
	if !r.tlsEnabled {
		// Plain HTTP addresses the runtime by the URL host itself, so there is
		// nothing to force-resolve.
		return []any{
			"transport", "http",
			"endpoint", GetRuntimeURL(sbx),
			"forcedResolution", false,
		}
	}
	var caBundleBytes int
	if r.tlsBundle != nil {
		caBundleBytes = len(r.tlsBundle.CABundle)
	}
	var dialTarget string
	if ip := r.dialIPFor(sbx); ip != "" {
		dialTarget = net.JoinHostPort(ip, strconv.Itoa(r.tlsPort))
	}
	return []any{
		"transport", "https",
		"endpoint", fmt.Sprintf("https://%s:%d", r.authority, r.tlsPort),
		// forcedResolution reports the `curl --resolve` behaviour: the dial goes
		// to dialTarget (the sandbox Pod IP) while SNI and certificate
		// verification stay on the endpoint authority. An empty dialTarget means
		// the Pod IP is not resolved yet, i.e. the call cannot be addressed.
		"forcedResolution", dialTarget != "",
		"dialTarget", dialTarget,
		"authority", r.authority,
		"tlsPort", r.tlsPort,
		// A client certificate is optional: its presence is what upgrades the
		// connection from server-authenticated TLS to mutual TLS.
		"mutualTLS", r.tlsClientConfig != nil && len(r.tlsClientConfig.Certificates) > 0,
		"caBundleBytes", caBundleBytes,
		"tlsConfigErr", r.tlsConfigErr,
	}
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
// The X-Access-Token header is always attached when the sandbox carries an
// access token: it is the gate expected by the agent-runtime APIs. The Basic
// user credential is opt-in via WithAuthUser: control-plane calls send none by
// default, and callers whose capability resolves an OS user supply it at
// construction time.
//
// Logging: the per-attempt request/response pair stays at V(DebugLogLevel)
// because every capability group funnels through here, while a failed attempt is
// reported at the default verbosity — retry.OnError discards every intermediate
// error, so an attempt that is retried away would otherwise leave no trace. Both
// carry the resolved transport (scheme, forced resolution, dial target, mutual
// TLS) so a connectivity failure can be attributed to the protocol and
// addressing actually used.
func (r *runtimeClient) call(ctx context.Context, method, path string, reqBody, respOut any) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(r.sbx))
	debugLog := log.V(utils.DebugLogLevel)

	// A TLS-config construction failure is a permanent, non-retryable error:
	// surface it before entering the retry loop.
	if r.tlsEnabled && r.tlsConfigErr != nil {
		return fmt.Errorf("invalid runtime TLS configuration: %w", r.tlsConfigErr)
	}

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

		// The endpoint and the client are resolved together so this JSON path
		// applies exactly the same TLS decision as the multipart and connect-gRPC
		// capability groups (see resolveTransport).
		base, httpClient, err := r.resolveTransport(sbx, r.client)
		if err != nil {
			return err
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
		// Basic user credential is caller-supplied (WithAuthUser); absent by default.
		if r.authUser != "" {
			req.Header.Set("Authorization", basicAuthHeader(r.authUser))
		}

		// attemptValues records what this attempt puts on the wire: the resolved
		// transport of the (possibly refreshed) sandbox plus the full request URL.
		attemptValues := append(r.transportLogValues(sbx),
			"method", method, "requestURL", endpoint, "attempt", attempt)

		start := time.Now()
		debugLog.Info("sending runtime request", attemptValues...)

		resp, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("failed to call runtime API %s: %w", path, err)
		}
		defer func() {
			// Drain and close so the shared transport can reuse the connection;
			// the one-shot pinned transport closes it eagerly instead
			// (keep-alives disabled).
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()

		bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, maxRuntimeResponseBody))

		if resp.StatusCode >= http.StatusBadRequest {
			// The status code alone already classifies the failure, so a mid-body
			// read error only degrades the diagnostic message: fall back to the
			// read error when the partial body carries no reason.
			msg := extractErrorMessage(bodyBytes)
			if msg == "" && readErr != nil {
				msg = fmt.Sprintf("failed to read error response body: %v", readErr)
			}
			return &APIError{
				Path:       path,
				StatusCode: resp.StatusCode,
				Message:    msg,
			}
		}

		// On a 2xx a truncated body would otherwise surface as a misleading
		// decode error (or a silently empty respOut): report the transport
		// failure itself so the retry predicate treats it as transient.
		if readErr != nil {
			return fmt.Errorf("failed to read runtime API %s response body: %w", path, readErr)
		}

		if respOut != nil && len(bodyBytes) > 0 {
			if err := json.Unmarshal(bodyBytes, respOut); err != nil {
				return fmt.Errorf("failed to decode runtime API %s response: %w", path, err)
			}
		}

		debugLog.Info("runtime request completed",
			append(attemptValues, "statusCode", resp.StatusCode, "cost", time.Since(start))...)
		return nil
	}

	// Report every failed attempt with its transport: the retry loop swallows all
	// but the last error, and a 4xx that a capability group later reclassifies as
	// success (e.g. init's 401 on re-init) must not surface as an ERROR entry, so
	// the error travels as a log value instead of via Error().
	return retry.OnError(r.backoff, retriableRuntimeError(ctx), func() error {
		err := do()
		if err != nil {
			log.Info("runtime request attempt failed",
				append(r.transportLogValues(sbx), "method", method, "path", path,
					"attempt", attempt, "err", err.Error())...)
		}
		return err
	})
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
