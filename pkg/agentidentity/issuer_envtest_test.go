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
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
	"github.com/openkruise/agents/pkg/identity/jwtissuer"
)

// TestEndpointsServeOnANonLeader is the reason unconditionalRunnable exists.
//
// A bare manager.RunnableFunc does not implement LeaderElectionRunnable, and
// the manager's default for anything that does not is the leader-election
// group. Discovery and JWKS are read-only, every replica binds the port at
// startup, and /readyz answers before the lease is held, so a non-leader that
// did not serve them would join Service endpoints and then accept connections
// it never answers.
//
// Nothing could check that without a real lease. The lease here is taken by
// somebody else first and held for the whole test, so this manager starts with
// leader election enabled and never becomes the leader.
func TestEndpointsServeOnANonLeader(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run under make test or point it at envtest binaries")
	}
	ctrl.SetLogger(klog.Background())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	scheme := apiruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("register the core scheme: %v", err)
	}
	if err := securityv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("register the security scheme: %v", err)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		Scheme:                scheme,
	}
	restConfig, err := env.Start()
	if err != nil {
		t.Fatalf("start the API server: %v", err)
	}
	t.Cleanup(func() { stopEnvironment(t, env) })

	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("build a client: %v", err)
	}

	const (
		namespace = "kube-system"
		leaseName = "agent-identity-provider"
	)

	// Another replica takes the lease first and holds it. A long duration with a
	// fresh acquire time means this manager cannot time it out and take over.
	holder := "the-other-replica"
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: namespace},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holder,
			LeaseDurationSeconds: ptr.To[int32](3600),
			AcquireTime:          &metav1.MicroTime{Time: time.Now()},
			RenewTime:            &metav1.MicroTime{Time: time.Now()},
		},
	}
	if err := c.Create(ctx, lease); err != nil {
		t.Fatalf("take the lease on behalf of another replica: %v", err)
	}

	dir := t.TempDir()
	signingKeyFile, keyID := writeLocalSigningKey(t, dir)
	certFile, keyFile, certPEM := writeLocalServingCertificate(t, dir)

	port := freePort(t)
	issuerURL := fmt.Sprintf("https://127.0.0.1:%d", port)

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       ctrlmetrics.Options{BindAddress: "0"},
		LeaderElection:                true,
		LeaderElectionID:              leaseName,
		LeaderElectionNamespace:       namespace,
		LeaderElectionReleaseOnCancel: true,
		Controller:                    ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}

	issuer, err := AddIssuer(mgr, IssuerConfig{
		IssuerURL:      issuerURL,
		SigningKeyFile: signingKeyFile,
		TLSCertFile:    certFile,
		TLSKeyFile:     keyFile,
		BindAddress:    fmt.Sprintf("127.0.0.1:%d", port),
	})
	if err != nil {
		t.Fatalf("add the issuer: %v", err)
	}

	// A leader-only runnable, so the test can prove this manager really is not
	// the leader rather than assuming the lease held.
	leaderOnly := make(chan struct{})
	if err := mgr.Add(&leaderGatedRunnable{started: leaderOnly}); err != nil {
		t.Fatalf("add the leader-only runnable: %v", err)
	}

	go func() { _ = mgr.Start(ctx) }()

	fixture := &chain{issuer: issuer, issuerURL: issuerURL, keyID: keyID, trusting: trustingClient(certPEM)}

	// The endpoints must come up without the lease.
	waitForEndpoint(t, fixture, issuerURL+jwtissuer.DiscoveryPath)

	var document struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	fetchJSONOver(t, fixture, issuerURL+jwtissuer.DiscoveryPath, &document)
	if document.Issuer != issuerURL {
		t.Errorf("iss is %q, want %q", document.Issuer, issuerURL)
	}

	var keySet jose.JSONWebKeySet
	fetchJSONOver(t, fixture, issuerURL+jwtissuer.JWKSPath, &keySet)
	if len(keySet.Keys) != 1 {
		t.Fatalf("the non-leader published %d keys, want 1", len(keySet.Keys))
	}
	if keySet.Keys[0].KeyID != keyID {
		t.Errorf("kid is %q, want %q", keySet.Keys[0].KeyID, keyID)
	}

	// Confirm the premise: a leader-gated runnable must not have started, or
	// this replica was the leader after all and the test proved nothing.
	select {
	case <-leaderOnly:
		t.Fatal("this replica acquired the lease, so the non-leader path was never exercised")
	default:
	}

	// And the lease is still somebody else's.
	observed := &coordinationv1.Lease{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(lease), observed); err != nil {
		t.Fatalf("re-read the lease: %v", err)
	}
	if observed.Spec.HolderIdentity == nil || *observed.Spec.HolderIdentity != holder {
		t.Fatalf("the lease holder is %v, want %q", observed.Spec.HolderIdentity, holder)
	}
}

// leaderGatedRunnable signals when it starts and needs the lease to do so.
type leaderGatedRunnable struct {
	started chan struct{}
}

func (r *leaderGatedRunnable) Start(ctx context.Context) error {
	close(r.started)
	<-ctx.Done()
	return nil
}

func (r *leaderGatedRunnable) NeedLeaderElection() bool { return true }

// TestServingCertificateReloadsWithoutARestart covers the reason AddIssuer uses
// certwatcher rather than passing the file paths to ServeTLS.
//
// cert-manager rotates a serving certificate in place. If the running process
// kept the pair it read at startup, the endpoints would keep presenting an
// expired certificate until something restarted the pod, and every verifier
// bootstrapping from the JWKS would fail in the meantime.
func TestServingCertificateReloadsWithoutARestart(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run under make test or point it at envtest binaries")
	}
	ctrl.SetLogger(klog.Background())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	scheme := apiruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("register the core scheme: %v", err)
	}

	env := &envtest.Environment{Scheme: scheme}
	restConfig, err := env.Start()
	if err != nil {
		t.Fatalf("start the API server: %v", err)
	}
	t.Cleanup(func() { stopEnvironment(t, env) })

	dir := t.TempDir()
	signingKeyFile, _ := writeLocalSigningKey(t, dir)

	// The first pair is what the process starts with.
	certFile, keyFile, firstPEM := writeLocalServingCertificate(t, dir)
	firstSerial := serialOf(t, certFile)

	port := freePort(t)
	issuerURL := fmt.Sprintf("https://127.0.0.1:%d", port)

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:     scheme,
		Metrics:    ctrlmetrics.Options{BindAddress: "0"},
		Controller: ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}
	if _, err := AddIssuer(mgr, IssuerConfig{
		IssuerURL:      issuerURL,
		SigningKeyFile: signingKeyFile,
		TLSCertFile:    certFile,
		TLSKeyFile:     keyFile,
		BindAddress:    fmt.Sprintf("127.0.0.1:%d", port),
	}); err != nil {
		t.Fatalf("add the issuer: %v", err)
	}

	go func() { _ = mgr.Start(ctx) }()

	address := issuerURL + jwtissuer.DiscoveryPath
	first := &chain{issuerURL: issuerURL, trusting: trustingClient(firstPEM)}
	waitForEndpoint(t, first, address)

	if served := servedSerial(t, first, address); served != firstSerial {
		t.Fatalf("the endpoint presented serial %s, want the first certificate %s", served, firstSerial)
	}

	// Rotate in place, the way cert-manager does: same paths, new material.
	rotatedDir := t.TempDir()
	rotatedCert, rotatedKey, secondPEM := writeLocalServingCertificate(t, rotatedDir)
	secondSerial := serialOf(t, rotatedCert)
	if secondSerial == firstSerial {
		t.Fatal("the two generated certificates share a serial, so a reload would be invisible")
	}
	replaceFile(t, rotatedCert, certFile)
	replaceFile(t, rotatedKey, keyFile)

	// A client that trusts only the new certificate, so a stale pair fails the
	// handshake rather than quietly succeeding.
	second := &chain{issuerURL: issuerURL, trusting: trustingClient(secondPEM)}

	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		response, err := second.trusting.Get(address)
		if err == nil {
			_ = response.Body.Close()
			if served := servedSerial(t, second, address); served == secondSerial {
				return
			}
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("the endpoint never presented the rotated certificate %s: %v", secondSerial, lastErr)
}

// servedSerial reports the serial of the certificate the endpoint presents.
func servedSerial(t *testing.T, c *chain, address string) string {
	t.Helper()
	response, err := c.trusting.Get(address)
	if err != nil {
		t.Fatalf("GET %s: %v", address, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.TLS == nil || len(response.TLS.PeerCertificates) == 0 {
		t.Fatalf("GET %s returned no peer certificate", address)
	}
	return response.TLS.PeerCertificates[0].SerialNumber.String()
}

// serialOf reads the serial out of a PEM certificate on disk.
func serialOf(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block, _ := pem.Decode(contents)
	if block == nil {
		t.Fatalf("%s holds no PEM block", path)
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return parsed.SerialNumber.String()
}

// replaceFile overwrites destination with the contents of source, which is what
// a rotation looks like to a process watching the path.
func replaceFile(t *testing.T, source, destination string) {
	t.Helper()
	contents, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %s: %v", source, err)
	}
	if err := os.WriteFile(destination, contents, 0o600); err != nil {
		t.Fatalf("write %s: %v", destination, err)
	}
}
