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

package agentidentity

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/openkruise/agents/pkg/identity/agenttoken"
	"github.com/openkruise/agents/pkg/identity/jwtissuer"
)

// readHeaderTimeout bounds how long a client may take to send its headers,
// which is what stops an idle connection holding a slot open indefinitely.
const readHeaderTimeout = 5 * time.Second

// IssuerConfig is the operator-supplied configuration for token issuance.
//
// Every field names a path or URL the operator chose. None of it is derived
// from a request.
type IssuerConfig struct {
	// IssuerURL is the absolute HTTPS URL this issuer is reachable at. It
	// becomes the iss claim, and a verifier compares it against the issuer
	// advertised by discovery, so it must match how callers actually reach the
	// service rather than how the pod binds.
	IssuerURL string

	// SigningKeyFile is the PEM private key to sign with, normally a Secret
	// projected as a volume. Empty disables issuance entirely.
	SigningKeyFile string

	// TLSCertFile and TLSKeyFile serve the discovery and JWKS endpoints.
	// Required whenever SigningKeyFile is set.
	TLSCertFile string
	TLSKeyFile  string

	// BindAddress is the address the endpoints listen on.
	BindAddress string

	// ClientCAFile is the CA that verifies control-plane client certificates.
	// Setting it enables agent token issuance on the same listener: callers must
	// present a certificate chaining to this CA. The discovery and JWKS
	// endpoints stay reachable without one, because a verifier holds no
	// control-plane credential.
	ClientCAFile string
}

// AddIssuer builds an Issuer from cfg and registers a listener serving its
// discovery and JWKS endpoints with the manager.
//
// Issuance is opt-in configuration rather than a feature gate: an empty
// SigningKeyFile returns a nil Issuer and registers nothing, so the process runs
// the AgentIdentity controller alone until an operator supplies a key. That is
// the same shape as the controller itself, which does not start unless the CRD
// is installed.
//
// Once a key is configured TLS is mandatory. The JWKS tells every verifier
// which keys to trust, so serving it over plaintext would let anyone on the
// path substitute their own and have forged tokens accepted. There is no flag
// to turn that off.
func AddIssuer(mgr ctrl.Manager, cfg IssuerConfig) (*jwtissuer.Issuer, error) {
	if cfg.SigningKeyFile == "" {
		if cfg.ClientCAFile != "" {
			return nil, errors.New("a client CA is configured but no signing key is: " +
				"agent token issuance needs the key it would sign with")
		}
		klog.InfoS("Token issuance is disabled: no signing key configured")
		return nil, nil
	}
	if cfg.IssuerURL == "" {
		return nil, errors.New("issuer URL is required when a signing key is configured")
	}
	if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
		return nil, errors.New("TLS certificate and key are required when a signing key is configured: " +
			"the JWKS determines which keys verifiers trust and must not be served over plaintext")
	}

	clientCAs, err := loadClientCAs(cfg.ClientCAFile)
	if err != nil {
		return nil, err
	}

	signingKey, err := jwtissuer.LoadSigningKeyFromFile(cfg.SigningKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load signing key: %w", err)
	}

	issuer, err := jwtissuer.New(cfg.IssuerURL, signingKey)
	if err != nil {
		return nil, fmt.Errorf("build issuer: %w", err)
	}

	issuerHandler, err := issuer.Handler()
	if err != nil {
		return nil, fmt.Errorf("build issuer endpoints: %w", err)
	}
	handler := issuerServingMux(issuerHandler, issuer, mgr.GetClient(), clientCAs != nil)

	// certwatcher caches the parsed pair and refreshes it from fsnotify events
	// plus a poll, so a cert-manager renewal is still picked up without a
	// restart but a handshake does not read the files.
	watcher, err := certwatcher.New(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("watch issuer serving certificate: %w", err)
	}

	tlsConfig := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: watcher.GetCertificate,
	}
	if clientCAs != nil {
		// Verify rather than require: discovery and JWKS must keep accepting
		// verifiers that present no certificate, and the issuance handler rejects
		// a request without a verified chain.
		tlsConfig.ClientCAs = clientCAs
		tlsConfig.ClientAuth = tls.VerifyClientCertIfGiven
	}

	server := &http.Server{
		Handler:           handler,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	// Bound here rather than inside the Runnable so a port already in use fails
	// startup, instead of surfacing later from a goroutine nobody is watching.
	listener, err := net.Listen("tcp", cfg.BindAddress)
	if err != nil {
		return nil, fmt.Errorf("listen for issuer endpoints on %s: %w", cfg.BindAddress, err)
	}
	closeListener := func(err error) error {
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			return errors.Join(err, closeErr)
		}
		return err
	}

	// Both of these must run on every replica, not just the leader. Discovery
	// and JWKS are read-only, every replica has already bound the port, and
	// /readyz answers before the lease is held, so a non-leader would enter
	// Service endpoints and then accept connections it never answers.
	if err := mgr.Add(everyReplica(func(ctx context.Context) error {
		return watcher.Start(ctx)
	})); err != nil {
		return nil, closeListener(fmt.Errorf("register the certificate watcher: %w", err))
	}
	if err := mgr.Add(everyReplica(func(ctx context.Context) error {
		return serve(ctx, server, listener)
	})); err != nil {
		return nil, closeListener(fmt.Errorf("register issuer endpoints: %w", err))
	}

	klog.InfoS("Token issuance is enabled", "issuerURL", cfg.IssuerURL,
		"keyID", signingKey.KeyID, "bindAddress", cfg.BindAddress,
		"agentTokenIssuance", clientCAs != nil)
	return issuer, nil
}

// issuerServingMux mounts agent token issuance next to discovery and JWKS.
// Without a client CA the issuer handler is served alone, so a deployment that
// only publishes verification keys exposes no way to mint anything.
func issuerServingMux(issuerHandler http.Handler, issuer *jwtissuer.Issuer,
	reader client.Reader, issueAgentTokens bool) http.Handler {
	if !issueAgentTokens {
		return issuerHandler
	}
	mux := http.NewServeMux()
	mux.Handle("/", issuerHandler)
	mux.Handle(agenttoken.Path, newAgentTokenHandler(issuer, reader))
	return mux
}

// loadClientCAs reads the CA that control-plane callers must chain to. An empty
// path disables client certificate verification and, with it, issuance.
func loadClientCAs(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	pemData, err := os.ReadFile(path) // #nosec G304 -- operator-configured CA mount path
	if err != nil {
		return nil, fmt.Errorf("read client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemData) {
		return nil, fmt.Errorf("client CA %s contains no certificates", path)
	}
	return pool, nil
}

// unconditionalRunnable is a Runnable that runs whether or not this replica
// holds the leader lease.
//
// A bare manager.RunnableFunc does not implement LeaderElectionRunnable, and
// the manager's default for anything that does not is the leader-election
// group. Serving read-only endpoints from the leader alone is wrong here: every
// replica binds the port at startup and answers /readyz before the lease is
// held, so a non-leader joins Service endpoints and then accepts connections it
// never answers.
type unconditionalRunnable func(context.Context) error

func (r unconditionalRunnable) Start(ctx context.Context) error { return r(ctx) }

// NeedLeaderElection puts this runnable in the manager's "others" group.
func (r unconditionalRunnable) NeedLeaderElection() bool { return false }

func everyReplica(run func(context.Context) error) manager.Runnable {
	return unconditionalRunnable(run)
}

// serve runs server on listener until ctx is cancelled, then drains it.
func serve(ctx context.Context, server *http.Server, listener net.Listener) error {
	errs := make(chan error, 1)
	go func() {
		klog.InfoS("Serving issuer discovery and JWKS", "address", listener.Addr())
		if err := server.ServeTLS(listener, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	// A fresh context: ctx is already cancelled, so passing it would skip the
	// drain entirely.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down issuer endpoints: %w", err)
	}
	return nil
}
