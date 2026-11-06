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

// Command agent-identity-provider runs the controllers and, in later phases,
// the services that own agent identity.
//
// It is a separate process from agent-sandbox-controller on purpose: identity is
// a distinct failure domain with its own RBAC and its own release cadence, and a
// cluster that has not opted into the identity path should not be running this
// code at all. Keeping it out of the shared controller manager is what makes
// that possible.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
	"github.com/openkruise/agents/client"
	"github.com/openkruise/agents/pkg/agentidentity"
	"github.com/openkruise/agents/pkg/identity/aead"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(securityv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr, probeAddr string
	var enableLeaderElection bool
	var issuerCfg agentidentity.IssuerConfig
	var encryptionKeyDir string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election, ensuring only one active instance reconciles.")
	flag.StringVar(&issuerCfg.IssuerURL, "issuer-url", "",
		"Absolute HTTPS URL this issuer is reachable at. Becomes the iss claim.")
	flag.StringVar(&issuerCfg.SigningKeyFile, "signing-key-file", "",
		"PEM private key to sign tokens with. Empty disables token issuance.")
	flag.StringVar(&issuerCfg.TLSCertFile, "tls-cert-file", "",
		"Certificate serving the discovery and JWKS endpoints. Required with --signing-key-file.")
	flag.StringVar(&issuerCfg.TLSKeyFile, "tls-key-file", "",
		"Private key for --tls-cert-file. Required with --signing-key-file.")
	flag.StringVar(&issuerCfg.BindAddress, "issuer-bind-address", ":8443",
		"The address the discovery and JWKS endpoints bind to.")
	flag.StringVar(&encryptionKeyDir, "encryption-key-dir", "",
		"Directory of root keys sealing persisted credential records, one file per key ID "+
			"alongside an \"active\" file naming which of them seals. Empty disables credential storage.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	cfg := ctrl.GetConfigOrDie()

	// The generic client backs discovery, which the controller uses to decide
	// whether the AgentIdentity CRD is installed before it registers anything.
	if err := client.NewRegistry(cfg); err != nil {
		setupLog.Error(err, "unable to initialise the client registry")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "agent-identity-provider.agents.kruise.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := agentidentity.Add(mgr); err != nil {
		setupLog.Error(err, "unable to add the AgentIdentity controller")
		os.Exit(1)
	}

	// Without this nothing ever sets an AgentAuthenticationConfig Ready, and
	// since authenticationRefs is required and every reference must be Ready, no
	// AgentIdentity could reach Ready either.
	if err := agentidentity.AddAuthConfig(mgr); err != nil {
		setupLog.Error(err, "unable to add the AgentAuthenticationConfig controller")
		os.Exit(1)
	}

	// The returned Issuer is what will mint agent tokens once the issuance RPC
	// exists. For now it only serves discovery and JWKS, so a verifier can
	// already fetch the keys it will need.
	if _, err := agentidentity.AddIssuer(mgr, issuerCfg); err != nil {
		setupLog.Error(err, "unable to add the token issuer")
		os.Exit(1)
	}

	// Loaded at startup so a missing or malformed key directory stops the
	// process here, rather than surfacing on the first delegation a user tries
	// to establish. Like the Issuer above, this is what will seal credential
	// records once the credential API exists; nothing consumes it yet.
	credentialKeys, err := loadEncryptionKeys(encryptionKeyDir)
	if err != nil {
		setupLog.Error(err, "unable to load the credential encryption keys")
		os.Exit(1)
	}
	if credentialKeys != nil {
		// The active key ID, never key material. It is the same value a sealed
		// record carries, so an operator can tell which key sealed what.
		setupLog.Info("credential record encryption is enabled",
			"activeKeyID", credentialKeys.ActiveKeyID())
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up the health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up the ready check")
		os.Exit(1)
	}

	setupLog.Info("starting agent-identity-provider")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// loadEncryptionKeys builds the keyring that seals persisted credential
// records, or reports that credential storage is disabled.
//
// Encryption is opt-in configuration rather than a feature gate, the same shape
// as the signing key: an empty directory means the process runs without
// credential storage, so a deployment that has not reached the credential flow
// does not have to invent a key to start.
//
// Where the keys come from is not optional. They are read from a file mount and
// never from the API server, because session and delegation records live in
// Secrets in sandbox-system, and a root key held in a Secret there would be
// reachable by the same RBAC grant that reaches the ciphertext it protects.
func loadEncryptionKeys(dir string) (*aead.Keyring, error) {
	if dir == "" {
		setupLog.Info("credential record encryption is disabled: no key directory configured")
		return nil, nil
	}
	return aead.LoadKeyringFromDir(dir)
}
