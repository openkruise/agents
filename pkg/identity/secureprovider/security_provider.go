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

package secureprovider

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/sandboxid"
	"github.com/openkruise/agents/pkg/utils/runtime"
)

// Environment variable keys for the remote identity provider configuration.
// They are read at process startup and supply the connection parameters used to
// talk to the external identity provider service when the secure provider is
// installed.
const (
	EnvIdentityProviderEndpoint           = "IDENTITY_PROVIDER_ENDPOINT"
	EnvIdentityProviderAuthToken          = "IDENTITY_PROVIDER_AUTH_TOKEN"
	EnvIdentityProviderCAFile             = "IDENTITY_PROVIDER_CA_FILE"
	EnvIdentityProviderCertFile           = "IDENTITY_PROVIDER_CERT_FILE"
	EnvIdentityProviderKeyFile            = "IDENTITY_PROVIDER_KEY_FILE"
	EnvIdentityProviderTimeout            = "IDENTITY_PROVIDER_TIMEOUT"
	EnvIdentityProviderInsecureSkipVerify = "IDENTITY_PROVIDER_INSECURE_SKIP_VERIFY"

	// EnvIdentityProviderCertDir is the mount path of a Kubernetes Secret volume
	// containing mTLS certificates. When set, the provider automatically resolves:
	//   <dir>/ca.crt      → CA certificate
	//   <dir>/client.crt   → client certificate
	//   <dir>/client.key   → client private key
	// Individual file env vars (CA_FILE, CERT_FILE, KEY_FILE) take precedence if set.
	EnvIdentityProviderCertDir = "IDENTITY_PROVIDER_CERT_DIR"

	// EnvIdentityProviderTokenDir is the mount path of the Kubernetes Secret volume
	// that holds the controller's identity provider auth token.
	// When set, the provider reads the auth token from <dir>/token.
	// Falls back to IDENTITY_PROVIDER_AUTH_TOKEN env var when not set or unreadable.
	EnvIdentityProviderTokenDir = "IDENTITY_PROVIDER_TOKEN_DIR"
)

// Default file names looked up under EnvIdentityProviderCertDir when individual
// certificate file env vars are not provided. They follow the conventional layout
// produced by mounting a Kubernetes TLS Secret as a volume.
const (
	DefaultCACertFileName     = "ca.crt"
	DefaultClientCertFileName = "client.crt"
	DefaultClientKeyFileName  = "client.key"

	// DefaultAuthTokenFileName is the file name for the auth token read from the
	// Secret mounted at the token dir. The Secret's "token" data key maps to this
	// file name.
	DefaultAuthTokenFileName = "token"
)

// Remote identity provider API action names sent via the X-Api-Action-Name HTTP
// header. The remote service uses this header to route and authorize requests by
// action. New identity provider APIs (e.g. RefreshToken, ValidateToken,
// RevokeToken) should add their action constants here.
const (
	ActionIssueToken              = "GetIdentityToken"
	ActionIssueGenericAccessToken = "GetGenericToken"
)

// maxIdentityProviderResponseBytes caps the size of a response body read from the
// remote identity provider. Legitimate responses are small JSON payloads (token,
// expiration, request id, etc.) well under a few KiB; capping at 1 MiB protects the
// caller from OOM if a misbehaving or compromised endpoint streams an unbounded
// body. The cap is applied via io.LimitReader before io.ReadAll.
const maxIdentityProviderResponseBytes = 1 << 20 // 1 MiB

// failingProvider is an IdentityProvider that always returns an error. It is used
// when the secure provider is enabled but cannot
// be constructed due to misconfiguration (e.g. missing endpoint, unreadable certs).
// This makes configuration errors immediately visible to operators instead of silently
// degrading to a meaningless UUID token that would pollute runtime credentials.
type failingProvider struct {
	reason string
}

func (f *failingProvider) IssueToken(_ context.Context, _ *agentsv1alpha1.Sandbox, _ identity.TokenOptions) (*identity.TokenResponse, error) {
	return nil, fmt.Errorf("remote identity provider unavailable: %s", f.reason)
}

// PropagateSecurityToken surfaces the original construction failure. rtOpts is
// ignored: no propagator ever runs on this path, so the resolved transport is
// irrelevant here.
func (f *failingProvider) PropagateSecurityToken(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *identity.TokenResponse,
	_ ...runtime.Option) error {
	return fmt.Errorf("remote identity provider unavailable: %s", f.reason)
}

// buildSecureProvider constructs the secure identity provider from environment
// configuration. It is invoked by InstallFileBackedProviderFromEnv, which the binary
// calls after flag parsing once it has decided that the secure provider is enabled.
//
// The function never returns nil. On misconfiguration it returns a failingProvider
// that surfaces the configuration error on every call, ensuring operators are aware
// of the problem rather than silently issuing meaningless UUID tokens.
//
// Behavior:
//   - IDENTITY_PROVIDER_ENDPOINT not set: returns a failingProvider — the operator
//     enabled the provider but forgot to configure the endpoint.
//   - IDENTITY_PROVIDER_ENDPOINT set but token provider construction fails (e.g.
//     unreadable cert files): returns a failingProvider with the construction error.
//   - IDENTITY_PROVIDER_ENDPOINT set and construction succeeds: returns a
//     secureIdentityProvider that performs HTTPS token issuance and runs every
//     registered propagator.
func buildSecureProvider() identity.IdentityProvider {
	endpoint := os.Getenv(EnvIdentityProviderEndpoint)
	if endpoint == "" {
		reason := fmt.Sprintf("%s not set while the secure identity provider is enabled", EnvIdentityProviderEndpoint)
		klog.ErrorS(nil, "identity provider misconfigured", "reason", reason)
		return &failingProvider{reason: reason}
	}

	logEndpoint := loggableEndpoint(endpoint)
	klog.InfoS("initializing secure identity provider",
		"endpoint", logEndpoint)
	opts := identityProviderOptionsFromEnv(endpoint)
	tokenProvider, err := NewIdentityProviderTokenProvider(opts)
	if err != nil {
		reason := fmt.Sprintf("failed to create identity provider token provider: %v", err)
		klog.ErrorS(err, "identity provider construction failed", "endpoint", logEndpoint)
		return &failingProvider{reason: reason}
	}
	klog.InfoS("identity provider initialized in secure mode",
		"endpoint", logEndpoint)
	return newSecureIdentityProvider(tokenProvider)
}

// identityProviderOptionsFromEnv assembles IdentityProviderOptions from the remote
// identity provider environment variables. The endpoint parameter is the pre-read value
// of IDENTITY_PROVIDER_ENDPOINT obtained by the caller (buildSecureProvider)
// to avoid a redundant os.Getenv call and the theoretical risk of two reads diverging.
//
// Auth token resolution priority (first match wins):
//  1. File at IDENTITY_PROVIDER_TOKEN_DIR/token — mounted from the Secret that
//     holds the controller's identity provider auth token.
//  2. IDENTITY_PROVIDER_AUTH_TOKEN env var (direct value, for dev/testing).
//
// Certificate resolution priority (first match wins):
//  1. Individual file env vars (IDENTITY_PROVIDER_CA_FILE, _CERT_FILE, _KEY_FILE).
//  2. IDENTITY_PROVIDER_CERT_DIR with the conventional file names produced by mounting
//     a Kubernetes TLS Secret as a volume.
//  3. Empty — server-side TLS only, no mTLS.
func identityProviderOptionsFromEnv(endpoint string) IdentityProviderOptions {
	opts := IdentityProviderOptions{
		Endpoint: endpoint,
	}

	// Resolve certificate file paths: individual env vars take precedence over cert dir.
	certDir := os.Getenv(EnvIdentityProviderCertDir)
	opts.CAFile = resolveFilePath(os.Getenv(EnvIdentityProviderCAFile), certDir, DefaultCACertFileName)
	opts.CertFile = resolveFilePath(os.Getenv(EnvIdentityProviderCertFile), certDir, DefaultClientCertFileName)
	opts.KeyFile = resolveFilePath(os.Getenv(EnvIdentityProviderKeyFile), certDir, DefaultClientKeyFileName)

	// Resolve auth token: read from the auth token Secret mounted at tokenDir/token.
	// Falls back to the IDENTITY_PROVIDER_AUTH_TOKEN env var when the file is not available.
	tokenDir := os.Getenv(EnvIdentityProviderTokenDir)
	opts.AuthToken = resolveAuthToken(os.Getenv(EnvIdentityProviderAuthToken), tokenDir)

	if certDir != "" {
		klog.InfoS("identity provider cert dir resolved",
			"certDir", certDir,
			"caFile", opts.CAFile,
			"certFile", opts.CertFile,
			"keyFile", opts.KeyFile)
	}

	if timeoutStr := os.Getenv(EnvIdentityProviderTimeout); timeoutStr != "" {
		if d, err := time.ParseDuration(timeoutStr); err == nil {
			opts.Timeout = d
		} else {
			klog.ErrorS(err, "invalid identity provider timeout value, falling back to default",
				"envVar", EnvIdentityProviderTimeout,
				"value", timeoutStr)
		}
	}

	if skipVerifyStr := os.Getenv(EnvIdentityProviderInsecureSkipVerify); skipVerifyStr != "" {
		if v, err := strconv.ParseBool(skipVerifyStr); err == nil {
			opts.InsecureSkipVerify = v
		}
	}

	return opts
}

// resolveAuthToken resolves the identity provider auth token with the following priority:
//  1. File at tokenDir/token — read from the auth token Secret mounted as a
//     volume. The Secret's "token" data key produces this file.
//  2. Explicit env var value (IDENTITY_PROVIDER_AUTH_TOKEN) as a direct token string.
//
// Returns an empty string when neither source provides a token.
func resolveAuthToken(envValue, tokenDir string) string {
	// Priority 1: read from mounted Secret file.
	tokenFile := resolveFilePath("", tokenDir, DefaultAuthTokenFileName)
	if tokenFile != "" {
		data, err := os.ReadFile(tokenFile) //nolint:gosec // G304: operator-configured mounted Secret path, not user input
		if err == nil {
			token := strings.TrimSpace(string(data))
			if token != "" {
				klog.InfoS("identity provider auth token resolved from mounted secret file",
					"file", tokenFile)
				return token
			}
		} else {
			klog.V(5).InfoS("cannot read auth token file, falling back to env var",
				"file", tokenFile, "error", err)
		}
	}

	// Priority 2: direct value from environment variable.
	if envValue != "" {
		klog.InfoS("identity provider auth token resolved from environment variable",
			"envVar", EnvIdentityProviderAuthToken)
	}
	return envValue
}

// resolveFilePath returns the explicit path when provided; otherwise it joins
// certDir with defaultName. Returns an empty string when both are empty, signalling
// that the corresponding certificate material is not configured.
func resolveFilePath(explicit, certDir, defaultName string) string {
	if explicit != "" {
		return explicit
	}
	if certDir != "" {
		return filepath.Join(certDir, defaultName)
	}
	return ""
}

// identityProviderTokenProvider is the TokenProvider that issues tokens by
// calling the external identity provider service via HTTPS. It is intentionally a thin
// business wrapper around identityProviderClient: it adds TokenType-aware request
// construction, business-level response validation (e.g. non-empty access token) and
// structured business logs, while delegating all HTTP / TLS plumbing to the client.
type identityProviderTokenProvider struct {
	client *identityProviderClient
}

// identityProviderClient is the shared HTTP client for the remote identity provider
// service. It encapsulates the wiring common to every remote identity provider API:
//   - HTTPS POST to a single endpoint
//   - Content-Type: application/json
//   - Authorization: Bearer <token>
//   - X-Api-Action-Name: <action>
//   - JSON-encoded request body and JSON-decoded response body
//   - HTTP 200 OK as the only success status
//
// Future remote identity provider APIs (e.g. RefreshToken, ValidateToken,
// RevokeToken) should reuse this client via Invoke() rather than re-implementing the
// HTTP / TLS plumbing.
type identityProviderClient struct {
	endpoint   string
	authToken  string
	httpClient *http.Client

	// tokenFunc, when non-nil, resolves the Bearer token freshly on every request.
	// It is used by the sandbox-manager credential source to read an auto-rotated
	// projected ServiceAccount token from file per request. When nil the static
	// authToken (controller's mounted-file deployment) is used instead.
	tokenFunc func() (string, error)
}

// IdentityProviderOptions captures the runtime configuration of the remote identity
// provider client. Instances are typically populated from environment variables via
// identityProviderOptionsFromEnv.
type IdentityProviderOptions struct {
	// Endpoint is the HTTPS URL of the remote identity provider token issuance API.
	Endpoint string

	// AuthToken is the Bearer token used to authenticate with the identity provider.
	AuthToken string

	// Timeout bounds a single HTTP request to the identity provider. Defaults to 10s
	// when zero.
	Timeout time.Duration

	// CAFile is the path to the PEM-encoded CA certificate used to verify the identity
	// provider's server certificate. When empty the system root CA pool is used.
	CAFile string

	// CertFile is the path to the PEM-encoded client certificate for mTLS authentication.
	// Must be paired with KeyFile.
	CertFile string

	// KeyFile is the path to the PEM-encoded client private key for mTLS authentication.
	// Must be paired with CertFile.
	KeyFile string

	// CAData holds PEM-encoded CA certificate bytes. When non-empty it takes
	// precedence over CAFile. The sandbox-manager credential source populates it
	// with material fetched from a Kubernetes Secret, while the controller keeps
	// using CAFile from a mounted volume.
	CAData []byte

	// CertData holds PEM-encoded client certificate bytes for mTLS. When paired with
	// KeyData it takes precedence over CertFile/KeyFile.
	CertData []byte

	// KeyData holds PEM-encoded client private key bytes for mTLS. Must be paired
	// with CertData.
	KeyData []byte

	// TokenFunc, when non-nil, resolves the Bearer token on every request instead of
	// using the static AuthToken. It supports short-lived, auto-rotated tokens read
	// from a projected ServiceAccount token file.
	TokenFunc func() (string, error)

	// InsecureSkipVerify disables TLS certificate verification. It is intended for
	// development and testing environments only and must never be enabled in production.
	InsecureSkipVerify bool

	// StrictTransport opts into the sandbox-manager Secret-backed credential modes,
	// which send a tenant client certificate and a ServiceAccount token and therefore
	// refuse any transport weaker than HTTPS with full verification. When true the
	// client is only built for an https endpoint without embedded userinfo,
	// InsecureSkipVerify is rejected instead of honored, and HTTP redirects are
	// rejected rather than followed. The controller's volume-mounted deployment leaves
	// this false and keeps its existing transport behavior.
	StrictTransport bool
}

// errIdentityProviderRedirectRejected is returned by the strict-transport redirect policy.
// The sentinel itself carries no redirect target, because a Location header is
// server-controlled and may carry credentials. net/http re-wraps it in a *url.Error that
// does contain the target, so Invoke discards that wrapper before the error can be logged.
var errIdentityProviderRedirectRejected = errors.New("identity provider returned an HTTP redirect, which is rejected in secret-backed credential mode")

// NewIdentityProviderTokenProvider creates the TokenProvider that issues
// tokens by calling the external identity provider service via HTTPS.
//
// The returned provider has no built-in fallback: any failure is propagated to the
// caller. Whether the default UUID provider stays in place is decided at the
// registration layer (identity.RegisterProvider), so the package keeps a single,
// well-defined fallback boundary.
func NewIdentityProviderTokenProvider(opts IdentityProviderOptions) (TokenProvider, error) {
	client, err := newIdentityProviderClient(opts)
	if err != nil {
		return nil, err
	}
	return &identityProviderTokenProvider{client: client}, nil
}

// newIdentityProviderClient constructs a reusable remote identity provider HTTP
// client from opts. It centralizes TLS configuration, connection pooling and timeout
// tuning so that every remote identity provider API (IssueToken today, and future
// RefreshToken / ValidateToken / RevokeToken) inherits the exact same network behavior.
func newIdentityProviderClient(opts IdentityProviderOptions) (*identityProviderClient, error) {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	if opts.StrictTransport {
		if err := validateStrictTransport(opts); err != nil {
			return nil, err
		}
	}

	tlsConfig, err := buildTLSConfig(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to build TLS config for identity provider: %w", err)
	}

	transport := &http.Transport{
		TLSClientConfig: tlsConfig,

		// Connection pooling: reuse connections to the single identity provider endpoint.
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 10,
		MaxConnsPerHost:     20,

		// Idle connection reaping: proactively close idle connections to avoid stale sockets
		// and upstream load balancer idle timeouts (typically 60s–120s).
		IdleConnTimeout: 30 * time.Second,

		// TLS handshake timeout: bound the TLS negotiation to avoid hanging on slow endpoints.
		TLSHandshakeTimeout: 5 * time.Second,

		// Response header timeout: fail fast if the server accepts the connection but
		// does not start responding within this duration.
		ResponseHeaderTimeout: 8 * time.Second,

		// Expect-Continue timeout: for POST requests, limit the wait for "100 Continue".
		ExpectContinueTimeout: 1 * time.Second,

		// Disable keep-alive if explicitly requested (not recommended for production).
		DisableKeepAlives: false,

		// Force attempt HTTP/2 when TLS is used, improving multiplexing and latency.
		ForceAttemptHTTP2: true,
	}

	httpClient := &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
	if opts.StrictTransport {
		httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
			return errIdentityProviderRedirectRejected
		}
	}

	return &identityProviderClient{
		endpoint:   opts.Endpoint,
		authToken:  opts.AuthToken,
		tokenFunc:  opts.TokenFunc,
		httpClient: httpClient,
	}, nil
}

// validateStrictTransport rejects local HTTPS configuration that would let the
// Secret-backed credential modes send a tenant client certificate or a ServiceAccount
// token over an unauthenticated channel. The endpoint is retained in diagnostic logs, so
// userinfo is rejected here to keep that logged address free of credentials.
//
// It also rejects an address that cannot be dialed at all. Such a defect is discoverable
// by parsing alone, so it must fail startup instead of surfacing on the first business
// call that needs the identity provider.
func validateStrictTransport(opts IdentityProviderOptions) error {
	if opts.InsecureSkipVerify {
		return fmt.Errorf("%s must not be enabled while %s=%s",
			EnvIdentityProviderInsecureSkipVerify, EnvIdentityProviderCredentialSource, CredentialSourceSecret)
	}

	parsed, err := url.Parse(opts.Endpoint)
	if err != nil {
		// url.Parse wraps a failure in a *url.Error carrying the raw endpoint, which may
		// embed credentials, so only the reason is surfaced.
		var parseErr *url.Error
		if errors.As(err, &parseErr) {
			return fmt.Errorf("identity provider endpoint is not a valid URL: %v", parseErr.Err)
		}
		return errors.New("identity provider endpoint is not a valid URL")
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("identity provider endpoint must use https while %s=%s, got scheme %q",
			EnvIdentityProviderCredentialSource, CredentialSourceSecret, parsed.Scheme)
	}
	if parsed.Hostname() == "" {
		// Hostname rather than Host: url.Parse accepts "https://:443", whose Host is ":443"
		// but whose hostname is empty. net/http rejects either only once a request is sent,
		// and startup never probes the endpoint. The endpoint itself is omitted from the
		// message for the same reason as the userinfo rejection below.
		return errors.New("identity provider endpoint must include a host")
	}
	if port := parsed.Port(); port != "" {
		// url.Parse guarantees an explicit port is all digits, but not that it is in range,
		// and a long enough digit string overflows Atoi.
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("identity provider endpoint has a port outside 1-65535: %q", port)
		}
	}
	if parsed.User != nil {
		return fmt.Errorf("identity provider endpoint must not embed userinfo credentials")
	}
	return nil
}

// loggableEndpoint reduces an identity provider endpoint to scheme://host/path for
// diagnostic logs. The address is operator-supplied and may carry credentials in its
// userinfo, query or fragment; url.Parse keeps each of those separately from the host and
// path, so rebuilding from only those three fields drops every credential-bearing
// component. Rejection cannot substitute for this: the endpoint is logged before
// validateStrictTransport runs, and query and fragment are legitimate URL parts that a
// non-strict deployment may use.
//
// An endpoint that does not parse is reported as a placeholder rather than echoed, since
// the raw value is exactly what must not reach the log.
func loggableEndpoint(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "<unparsable endpoint>"
	}
	return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: parsed.Path}).String()
}

// redactURLError renders a failed identity provider call without the request URL. net/http
// reports both request-construction and transport failures as a *url.Error whose message
// embeds the full URL, and it masks only a userinfo password — a credential in the query or
// the fragment survives verbatim into the log that reports the error. The operation and the
// underlying cause carry the diagnostic value, so the URL is reduced to its loggable form.
//
// The cause is wrapped rather than formatted so errors.Is keeps working upstream, where a
// cancellation is deliberately logged below the error threshold.
func redactURLError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	return fmt.Errorf("%s %s: %w", urlErr.Op, loggableEndpoint(urlErr.URL), urlErr.Err)
}

// buildTLSConfig assembles the tls.Config used to talk to the remote identity
// provider. It supports a custom CA certificate, client certificate based mTLS and
// the InsecureSkipVerify escape hatch for non-production environments.
func buildTLSConfig(opts IdentityProviderOptions) (*tls.Config, error) {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: opts.InsecureSkipVerify, //nolint:gosec // configurable for dev/test environments
	}

	// Load custom CA certificate. In-memory CAData (fetched from a Kubernetes Secret
	// by the sandbox-manager credential source) takes precedence over the CAFile path
	// used by the controller's volume-mounted deployment.
	caPEM := opts.CAData
	caSource := "in-memory"
	if len(caPEM) == 0 && opts.CAFile != "" {
		caCert, err := os.ReadFile(opts.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA file %s: %w", opts.CAFile, err)
		}
		caPEM = caCert
		caSource = opts.CAFile
	}
	if len(caPEM) > 0 {
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("failed to parse CA certificate from %s", caSource)
		}
		tlsConfig.RootCAs = caPool
	}

	// Load client certificate for mTLS.
	//
	// mTLS requires CertFile and KeyFile to be configured as a pair. When only one of
	// them is set, the asymmetric configuration is almost certainly an operator
	// mis-configuration (e.g. Secret missing one of the keys, only one env var
	// exported, IDENTITY_PROVIDER_CERT_DIR missing client.crt or client.key). Without
	// surfacing it as a warning we would silently fall back to "server-side TLS only"
	// while the operator believes mTLS is enabled — that mode of failure typically
	// only manifests later as opaque TLS handshake errors from the upstream identity
	// provider, which are very hard to diagnose.
	//
	// In-memory CertData/KeyData (Secret-backed sandbox-manager) take precedence over
	// the CertFile/KeyFile paths (volume-mounted controller). Both halves of a pair
	// must be present together, otherwise the asymmetric configuration is treated as a
	// mis-configuration and mTLS is skipped with a warning.
	hasCertData := len(opts.CertData) > 0 && len(opts.KeyData) > 0
	hasCertFile := opts.CertFile != "" && opts.KeyFile != ""
	switch {
	case hasCertData:
		cert, err := tls.X509KeyPair(opts.CertData, opts.KeyData)
		if err != nil {
			return nil, fmt.Errorf("failed to load client certificate (in-memory): %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	case hasCertFile:
		cert, err := tls.LoadX509KeyPair(opts.CertFile, opts.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client certificate (%s, %s): %w", opts.CertFile, opts.KeyFile, err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	case len(opts.CertData) > 0 || len(opts.KeyData) > 0 || opts.CertFile != "" || opts.KeyFile != "":
		klog.Warningf("identity provider mTLS partially configured, client certificate will NOT be loaded: certFile=%q, keyFile=%q, certData=%dB, keyData=%dB (cert and key must be set together)",
			opts.CertFile, opts.KeyFile, len(opts.CertData), len(opts.KeyData))
	}

	return tlsConfig, nil
}

func (p *identityProviderTokenProvider) IssueToken(ctx context.Context, req identity.TokenRequest) (*identity.TokenResponse, error) {
	logger := klog.FromContext(ctx)
	start := time.Now()

	var tokenResp identity.TokenResponse
	if err := p.client.Invoke(ctx, ActionIssueToken, req, &tokenResp); err != nil {
		return nil, err
	}

	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("identity provider returned empty access token")
	}

	logger.Info("token issued from identity provider", "requestId", tokenResp.RequestID, "tokenType", req.TokenType, "cost", time.Since(start))
	return &tokenResp, nil
}

// Invoke performs a JSON-over-HTTPS POST to the remote identity provider with the
// given action name. req is JSON-encoded as the request body. respOut, when non-nil,
// receives the JSON-decoded response body on HTTP 200 OK. Pass nil for status-only APIs.
//
// Non-2xx responses, network errors and JSON encode/decode failures are surfaced as
// errors with the action name embedded for easier troubleshooting. Business-level
// validation (e.g. required fields in the response payload) is the caller's
// responsibility.
func (c *identityProviderClient) Invoke(ctx context.Context, actionName string, req interface{}, respOut interface{}) error {
	logger := klog.FromContext(ctx).WithValues("action", actionName, "endpoint", loggableEndpoint(c.endpoint))

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal %s request: %w", actionName, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create %s HTTP request: %w", actionName, redactURLError(err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Api-Action-Name", actionName)
	token := c.authToken
	if c.tokenFunc != nil {
		resolved, tokenErr := c.tokenFunc()
		if tokenErr != nil {
			return fmt.Errorf("failed to resolve identity provider auth token for %s: %w", actionName, tokenErr)
		}
		// A credential source that resolves per request must never fall through to an
		// anonymous call: the identity provider would answer with an unrelated
		// authorization error instead of the real cause, and the request would leave
		// the process without the tenant identity it was supposed to carry.
		if resolved == "" {
			return fmt.Errorf("identity provider auth token for %s resolved to an empty value", actionName)
		}
		token = resolved
	}
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}

	logger.V(5).Info("calling identity provider")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		// A rejected redirect is reported by net/http as a *url.Error whose URL field is
		// the raw Location header. An identity provider redirect commonly carries a
		// credential in its query, and net/http only masks a userinfo password, so the
		// wrapper is dropped and only the sentinel survives. Invoke is the single funnel
		// for identity provider transport errors, which keeps this redaction in one place.
		if errors.Is(err, errIdentityProviderRedirectRejected) {
			return fmt.Errorf("failed to call identity provider %s: %w", actionName, errIdentityProviderRedirectRejected)
		}
		return fmt.Errorf("failed to call identity provider %s: %w", actionName, redactURLError(err))
	}
	defer resp.Body.Close()

	// Cap the response body at maxIdentityProviderResponseBytes to defend against a
	// misbehaving endpoint streaming an unbounded body, which would otherwise cause
	// io.ReadAll to grow without bound and OOM the process. Legitimate identity
	// provider responses are small JSON payloads, so the cap is never reached in
	// practice; if it is reached, the truncated body still fails JSON unmarshal and
	// surfaces a clear error to the caller.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxIdentityProviderResponseBytes))
	if err != nil {
		return fmt.Errorf("failed to read identity provider %s response: %w", actionName, err)
	}

	if resp.StatusCode != http.StatusOK {
		// The response body is deliberately omitted. Callers log this error, and an
		// identity provider error page can echo request material back, so only the
		// action name and status code are kept as diagnostic context.
		return fmt.Errorf("identity provider %s returned status %d", actionName, resp.StatusCode)
	}

	if respOut == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, respOut); err != nil {
		return fmt.Errorf("failed to unmarshal identity provider %s response: %w", actionName, err)
	}
	return nil
}

// TokenProvider is this package's low-level interface for issuing sandbox
// access tokens over the remote identity provider transport. It previously
// lived in the public identity package as identity.TokenProvider, which was
// removed when token issuance was merged into the high-level
// identity.IdentityProvider (gaining sbx/claim parameters). This package
// keeps this narrower abstraction so secureIdentityProvider can wrap
// either the HTTPS-based identityProviderTokenProvider or a test mock, while only
// the high-level IdentityProvider is exposed to external callers.
type TokenProvider interface {
	// IssueToken generates the sandbox ID token for the given token request.
	// The context can carry deadlines and cancellation signals.
	IssueToken(ctx context.Context, req identity.TokenRequest) (*identity.TokenResponse, error)

	// IssueTrafficAccessToken mints the gateway access token (a JWT) for the
	// given request via the ActionIssueTrafficAccessToken action,
	// mapping its distinct response shape onto identity.TokenResponse.
	IssueTrafficAccessToken(ctx context.Context, req trafficAccessTokenRequest) (*identity.TokenResponse, error)
}

// secureIdentityProvider is the secure IdentityProvider implementation. It wraps a
// TokenProvider (typically the HTTPS-based identityProviderTokenProvider) and dispatches
// post-token side effects through this package's own propagator registry, providing
// real token issuance and propagation — as opposed to the default UUID-based no-op
// provider.
//
// Propagators are read live from the package-level propagators slice each time
// PropagateSecurityToken runs (no defensive snapshot), so files inside this package
// may register their propagators in arbitrary init order.
type secureIdentityProvider struct {
	tokenProvider TokenProvider
}

// newSecureIdentityProvider creates a secure IdentityProvider that delegates token
// issuance to the given TokenProvider and dispatches every registered propagator after a
// successful issuance.
func newSecureIdentityProvider(tokenProvider TokenProvider) identity.IdentityProvider {
	return &secureIdentityProvider{tokenProvider: tokenProvider}
}

// buildAgentTokenRequest assembles the TokenRequest from the sandbox
// object. Since IdentityProvider.IssueToken no longer receives a pre-built
// TokenRequest, the secure provider owns request composition: it derives the
// SandboxInfo projection and security metadata directly from sbx (annotations
// prefixed with identity.SecurityMetadataPrefix). A nil sbx yields a bare Agent
// request so principal/refresh paths that omit the workload object do not panic.
func buildAgentTokenRequest(sbx *agentsv1alpha1.Sandbox) identity.TokenRequest {
	req := identity.TokenRequest{TokenType: identity.TokenTypeAgent}
	if sbx == nil {
		return req
	}
	req.Sandbox = sandboxInfoFromSandbox(sbx)
	req.Metadata = identity.ExtractSecurityMetadata(sbx)
	return req
}

// sandboxInfoFromSandbox projects the sandbox workload identity fields shared by
// every token request (both the ID token and the gateway access
// token). Callers guard against a nil sbx.
func sandboxInfoFromSandbox(sbx *agentsv1alpha1.Sandbox) *identity.SandboxInfo {
	return &identity.SandboxInfo{
		PodName:      sbx.Name,
		PodNamespace: sbx.Namespace,
		SandboxID:    sandboxid.Resolve(sbx),
		SandboxName:  sbx.Name,
		SandboxUID:   string(sbx.UID),
	}
}

// IssueToken assembles the atomic request from the sandbox object and delegates
// to the wrapped low-level TokenProvider. It routes by opts.Kind: the gateway
// access token (TokenKindAccessToken) goes through IssueTrafficAccessToken and
// also carries opts.RequestedValidity as the requested token lifetime, while the
// sandbox ID token (the default) goes through IssueToken.
func (s *secureIdentityProvider) IssueToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox, opts identity.TokenOptions) (*identity.TokenResponse, error) {
	if opts.Kind == identity.TokenKindAccessToken {
		return s.tokenProvider.IssueTrafficAccessToken(ctx, buildTrafficAccessTokenRequest(sbx, opts.RequestedValidity))
	}
	return s.tokenProvider.IssueToken(ctx, buildAgentTokenRequest(sbx))
}

// PropagateSecurityToken executes every registered propagator sequentially. It always
// runs all propagators even when one fails so that independent side effects are not
// short-circuited. All propagator errors are accumulated with errors.Join and returned
// as a single combined error so operators can see every failure in one message rather
// than having to chase individual structured log entries.
//
// The propagator slice is read directly from this package's registry. The registry
// is populated only during init() (which runs single-threaded before main), so reads here
// require no additional synchronization.
//
// rtOpts is the transport the caller resolved for this sandbox and is forwarded
// verbatim to every propagator. This method neither resolves nor inspects it, so
// the TLS decision stays at the caller's boundary.
func (s *secureIdentityProvider) PropagateSecurityToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox, tokenResp *identity.TokenResponse,
	rtOpts ...runtime.Option) error {
	current := propagators
	if len(current) == 0 {
		return nil
	}
	log := klog.FromContext(ctx)
	start := time.Now()
	var propErrs []error
	for i, propagator := range current {
		propStart := time.Now()
		if propErr := propagator(ctx, sbx, tokenResp, rtOpts...); propErr != nil {
			log.Error(propErr, "security token propagator failed", "propagatorIndex", i, "cost", time.Since(propStart))
			propErrs = append(propErrs, propErr)
		} else {
			log.V(5).Info("security token propagator completed", "propagatorIndex", i, "cost", time.Since(propStart))
		}
	}
	log.Info("security token propagation completed", "propagatorCount", len(current), "cost", time.Since(start))
	return errors.Join(propErrs...)
}
