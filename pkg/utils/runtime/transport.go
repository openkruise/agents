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
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/peersecurity"
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
)

// TLSBundle is the shared certificate material used by runtime clients.
// CA-only bundles support server-authenticated TLS; a runtime configured with a
// client CA still requires a client certificate and rejects its absence during
// the handshake. Certificate/key material must be supplied together or omitted.
//
// known-limit: this alias and the loader forwarders retain the existing runtime
// API while callers migrate to peersecurity; they own no loading or parsing.
// TODO: migrate callers and remove the alias and thin forwarders in a follow-up PR.
type TLSBundle = peersecurity.TLSBundle

// buildClientTLSConfig assembles the *tls.Config used by the runtime client.
//
// serverName pins the SNI and certificate-verification hostname (typically
// RuntimeServerSNI) so verification succeeds against the wildcard SAN even
// though the underlying connection dials a bare Pod IP. The decoded material is
// reused from m when a loader already cached it, and decoded on demand
// otherwise; only serverName differs per client, so nothing else is rebuilt.
func buildClientTLSConfig(m TLSBundle, serverName string) (*tls.Config, error) {
	rootCAs, clientCert, err := m.Parsed()
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		RootCAs:    rootCAs,
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
	}
	if clientCert != nil {
		cfg.Certificates = []tls.Certificate{*clientCert}
	}
	return cfg, nil
}

// NewTLSBundle forwards directory loading to peersecurity. Empty dir disables
// TLS; a configured directory must contain a valid CA and an optional key pair.
func NewTLSBundle(dir string) (*TLSBundle, error) {
	return peersecurity.NewTLSBundleFromDir(dir)
}

// NewTLSBundleFromSecret forwards Secret loading to peersecurity, preserving
// the runtime entrypoint while callers migrate to the shared loader.
func NewTLSBundleFromSecret(ctx context.Context, reader ctrlclient.Reader, namespace, name string) (*TLSBundle, error) {
	return peersecurity.NewTLSBundleFromSecret(ctx, reader, namespace, name)
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
