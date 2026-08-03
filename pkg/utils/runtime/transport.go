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
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

const (
	// RuntimeTLSPort is the well-known HTTPS/TLS port exposed by the agent-runtime
	// sidecar. Plaintext HTTP stays on utils.RuntimePort; this mirrors the
	// agent-runtime -tls-port default so a client can reach the HTTPS server
	// without extra configuration.
	RuntimeTLSPort = 49984

	// RuntimeServerSNI is the canonical TLS authority (SNI + certificate
	// verification hostname) for the agent-runtime HTTPS server. The server
	// certificate SAN covers the wildcard *.sandbox.agents.kruise.io, of which
	// this name is the well-known instance also used by the sandbox-gateway when
	// it re-encrypts traffic to the runtime. Callers dial the sandbox Pod IP but
	// verify the certificate against this name (see newPinnedTransport).
	RuntimeServerSNI = "agentruntime.sandbox.agents.kruise.io"

	// pinnedDialTimeout bounds a single TCP dial to the sandbox Pod IP.
	pinnedDialTimeout = 5 * time.Second

	// Well-known names of the runtime client certificate material. They are
	// both the file names inside a mounted certificate directory (see
	// NewTLSBundle) and the data keys of the certificate Secret itself (see
	// NewTLSBundleFromSecret).
	clientCAFile   = "ca.crt"
	clientCertFile = "client.crt"
	clientKeyFile  = "client.key"
)

// TLSBundle carries the client-side certificate material used to speak
// HTTPS/mTLS to the agent-runtime.
//
// Only CABundle is required: it verifies the server certificate presented by
// the runtime. ClientCertPEM/ClientKeyPEM are optional because the runtime
// server is configured with tls.VerifyClientCertIfGiven — presenting a client
// certificate upgrades the connection to mutual TLS, but omitting it still
// yields a valid server-authenticated TLS connection. Provide both the client
// certificate and key together, or neither.
type TLSBundle struct {
	// CABundle is the PEM-encoded CA certificate(s) that issued the runtime
	// server certificate. Required.
	CABundle []byte
	// ClientCertPEM is the optional PEM-encoded client certificate for mutual TLS.
	ClientCertPEM []byte
	// ClientKeyPEM is the optional PEM-encoded client private key for mutual TLS.
	ClientKeyPEM []byte

	// parsed caches the decoded form of the PEM blocks above. It is populated by
	// the loaders (NewTLSBundle, NewTLSBundleFromSecret), which must decode the
	// material anyway to fail fast on a broken bundle, so every runtime client
	// then only assembles a tls.Config around it instead of re-parsing the PEM.
	// A zero-valued TLSBundle (built directly by a caller) leaves it nil and is
	// decoded on demand.
	parsed *parsedTLSBundle
}

// parsedTLSBundle holds the decoded, authority-independent part of a TLSBundle.
// Only tls.Config.ServerName varies between clients (see WithAuthority), so the
// trust anchors and the client certificate can be decoded once and shared; both
// are read-only afterwards and safe for concurrent use.
type parsedTLSBundle struct {
	rootCAs *x509.CertPool
	// clientCert is nil unless the bundle carries a client certificate/key pair.
	clientCert *tls.Certificate
}

// parseTLSBundle decodes the PEM blocks of m, rejecting a bundle that cannot
// produce a usable TLS configuration.
func parseTLSBundle(m TLSBundle) (*parsedTLSBundle, error) {
	if len(m.CABundle) == 0 {
		return nil, fmt.Errorf("runtime TLS CA bundle is required")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(m.CABundle) {
		return nil, fmt.Errorf("failed to parse runtime TLS CA bundle")
	}
	parsed := &parsedTLSBundle{rootCAs: pool}
	// Client certificate is optional (server uses VerifyClientCertIfGiven).
	if len(m.ClientCertPEM) > 0 || len(m.ClientKeyPEM) > 0 {
		cert, err := tls.X509KeyPair(m.ClientCertPEM, m.ClientKeyPEM)
		if err != nil {
			return nil, fmt.Errorf("failed to load runtime client certificate/key pair: %w", err)
		}
		parsed.clientCert = &cert
	}
	return parsed, nil
}

// buildClientTLSConfig assembles the *tls.Config used by the runtime client.
//
// serverName pins the SNI and certificate-verification hostname (typically
// RuntimeServerSNI) so verification succeeds against the wildcard SAN even
// though the underlying connection dials a bare Pod IP. The decoded material is
// reused from m when a loader already cached it, and decoded on demand
// otherwise; only serverName differs per client, so nothing else is rebuilt.
func buildClientTLSConfig(m TLSBundle, serverName string) (*tls.Config, error) {
	parsed := m.parsed
	if parsed == nil {
		var err error
		if parsed, err = parseTLSBundle(m); err != nil {
			return nil, err
		}
	}
	cfg := &tls.Config{
		RootCAs:    parsed.rootCAs,
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
	}
	if parsed.clientCert != nil {
		cfg.Certificates = []tls.Certificate{*parsed.clientCert}
	}
	return cfg, nil
}

// NewTLSBundle loads the client TLS bundle from dir, the mount point of the
// client certificate Secret carrying ca.crt (required) plus
// client.crt/client.key (optional, but only as a pair). It is the single place
// that touches certificate files for runtime clients; both the sandbox
// controller and the sandbox manager are expected to use it.
//
// Semantics are strict by design: an empty dir means TLS is not configured and
// yields (nil, nil), which callers treat as "this process speaks plain HTTP".
// A non-empty dir declares the intent to speak TLS, so any problem (missing
// directory, missing ca.crt, unparsable material, an unpaired client
// certificate) is an error the caller should surface at startup instead of
// silently degrading to plain HTTP.
//
// The bundle is a snapshot: callers load it once during startup and hold the
// value, so replacing the certificate material requires restarting the process.
// That mirrors how the sandbox-gateway consumes the same runtime mTLS Secret
// (loaded once by its cert-init container) and keeps the long-lived runtime
// certificates free of any reload machinery.
func NewTLSBundle(dir string) (*TLSBundle, error) {
	if dir == "" {
		return nil, nil
	}
	caBundle, err := os.ReadFile(filepath.Join(dir, clientCAFile)) // #nosec G304 -- operator-configured certificate directory
	if err != nil {
		return nil, fmt.Errorf("failed to read runtime client CA bundle %s: %w", filepath.Join(dir, clientCAFile), err)
	}

	certPEM, certErr := os.ReadFile(filepath.Join(dir, clientCertFile)) // #nosec G304 -- operator-configured certificate directory
	keyPEM, keyErr := os.ReadFile(filepath.Join(dir, clientKeyFile))    // #nosec G304 -- operator-configured certificate directory
	certMissing, keyMissing := os.IsNotExist(certErr), os.IsNotExist(keyErr)
	switch {
	case certMissing && keyMissing:
		// Server-authenticated TLS only; the runtime server accepts it
		// (VerifyClientCertIfGiven).
		certPEM, keyPEM = nil, nil
	case certErr != nil:
		return nil, fmt.Errorf("failed to read runtime client certificate %s: %w", filepath.Join(dir, clientCertFile), certErr)
	case keyErr != nil:
		return nil, fmt.Errorf("failed to read runtime client key %s: %w", filepath.Join(dir, clientKeyFile), keyErr)
	}

	m := &TLSBundle{CABundle: caBundle, ClientCertPEM: certPEM, ClientKeyPEM: keyPEM}
	// Decode eagerly so a broken mount fails fast at startup instead of on the
	// first runtime call, and keep the result so runtime clients reuse it.
	parsed, err := parseTLSBundle(*m)
	if err != nil {
		return nil, fmt.Errorf("invalid runtime client TLS bundle in %s: %w", dir, err)
	}
	m.parsed = parsed
	return m, nil
}

// NewTLSBundleFromSecret loads the client TLS bundle from the Kubernetes Secret
// namespace/name through reader. It is the Secret-backed counterpart of
// NewTLSBundle for components that cannot volume-mount the certificate Secret —
// e.g. the sandbox-manager, whose certificate Secret lives in a namespace it
// does not mount — and reads the very same ca.crt / client.crt / client.key
// keys.
//
// Semantics mirror NewTLSBundle: an empty name means TLS is not configured and
// yields (nil, nil), while a non-empty name declares the intent to speak TLS,
// so any problem (missing Secret, missing ca.crt, unparsable material, an
// unpaired client certificate) is an error the caller should surface at startup
// instead of silently degrading to plain HTTP.
//
// The returned bundle is likewise a snapshot: the caller reads it once during
// startup and holds the value, so replacing the certificate material requires
// restarting the process.
func NewTLSBundleFromSecret(ctx context.Context, reader ctrlclient.Reader, namespace, name string) (*TLSBundle, error) {
	if name == "" {
		return nil, nil
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret); err != nil {
		return nil, fmt.Errorf("failed to get runtime client certificate secret %s/%s: %w", namespace, name, err)
	}

	caBundle := secret.Data[clientCAFile]
	if len(caBundle) == 0 {
		return nil, fmt.Errorf("runtime client certificate secret %s/%s is missing the %q data key", namespace, name, clientCAFile)
	}
	// Unlike the directory layout, a Secret cannot distinguish "absent" from
	// "empty", so an unpaired client certificate is rejected explicitly rather
	// than silently downgraded to server-authenticated TLS.
	certPEM, keyPEM := secret.Data[clientCertFile], secret.Data[clientKeyFile]
	if (len(certPEM) == 0) != (len(keyPEM) == 0) {
		return nil, fmt.Errorf("runtime client certificate secret %s/%s carries an unpaired %q/%q data key (set both or neither)",
			namespace, name, clientCertFile, clientKeyFile)
	}

	m := &TLSBundle{CABundle: caBundle, ClientCertPEM: certPEM, ClientKeyPEM: keyPEM}
	// Decode eagerly so a broken Secret fails fast at startup instead of on the
	// first runtime call, and keep the result so runtime clients reuse it.
	parsed, err := parseTLSBundle(*m)
	if err != nil {
		return nil, fmt.Errorf("invalid runtime client TLS bundle in secret %s/%s: %w", namespace, name, err)
	}
	m.parsed = parsed
	return m, nil
}

// TransportOptionsFor resolves the transport Options for sbx from its
// advertised runtime capability, implementing the dual-switch decision:
//
//   - the sandbox carries no AnnotationRuntimeTLSPort -> nil options, plain
//     HTTP (legacy sandboxes are untouched);
//   - the annotation is present and a bundle is supplied -> WithTLS +
//     WithTLSPort, i.e. HTTPS with forced resolution to the sandbox Pod IP;
//   - the annotation is present but the caller supplies no TLS bundle ->
//     error. A sandbox that declares the TLS capability must not be silently
//     downgraded to plaintext by a caller that lacks certificates; surfacing
//     the misconfiguration is preferred over a quiet fallback.
//
// An explicitly present but unparsable annotation is likewise an error: it
// indicates a broken injection template.
func TransportOptionsFor(sbx *agentsv1alpha1.Sandbox, m *TLSBundle) ([]Option, error) {
	if sbx == nil {
		return nil, nil
	}
	raw := sbx.GetAnnotations()[agentsv1alpha1.AnnotationRuntimeTLSPort]
	if raw == "" {
		return nil, nil
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("invalid runtime TLS port annotation %q on sandbox %s/%s", raw, sbx.Namespace, sbx.Name)
	}
	if m == nil {
		return nil, fmt.Errorf("sandbox %s/%s advertises runtime TLS port %d but no client TLS bundle is configured",
			sbx.Namespace, sbx.Name, port)
	}
	return []Option{WithTLS(*m), WithTLSPort(port)}, nil
}

// newPinnedTransport builds an *http.Transport that reproduces the behaviour of
// `curl --resolve <host>:<port>:<ip>`: every dial is forced to dialIP:port
// regardless of the host in the request URL, while the TLS handshake still uses
// the request URL host (and tlsCfg.ServerName) for SNI and certificate
// verification.
//
// This lets a caller address the runtime by its certificate hostname
// (RuntimeServerSNI) — so the wildcard SAN validates — while physically
// connecting to the sandbox Pod IP, which has no DNS record.
//
// The transport is one-shot: the caller builds a fresh one per attempt (the
// Pod IP may change between retries) and discards it afterwards. Keep-alives
// are therefore disabled — a kept-alive connection parked in a discarded
// transport (whose zero IdleConnTimeout never expires) would never be reused
// and would leak one TCP+TLS connection per attempt until the remote end
// closes it.
//
// TODO: once all process/filesystem APIs are migrated to the TLS transport,
// switch to a properly shared transport with connection reuse; repeating the
// TLS handshake on every call is too costly at that call frequency.
func newPinnedTransport(dialIP string, port int, tlsCfg *tls.Config) *http.Transport {
	dialer := &net.Dialer{Timeout: pinnedDialTimeout}
	target := net.JoinHostPort(dialIP, strconv.Itoa(port))
	return &http.Transport{
		TLSClientConfig:   tlsCfg,
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			// Ignore the address derived from the request URL host and dial the
			// real sandbox Pod IP instead. TLS is still performed by net/http
			// using the URL host / tlsCfg.ServerName for SNI and verification.
			return dialer.DialContext(ctx, network, target)
		},
	}
}
