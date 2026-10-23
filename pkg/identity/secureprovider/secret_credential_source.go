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
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/identity"
)

// Environment variable keys and defaults for the Secret-backed credential source.
//
// The sandbox-manager may be deployed away from the cluster that hosts the remote
// identity provider, so it cannot rely on the same volume-mounted certificate/token
// layout used by the in-cluster sandbox-controller. Instead it fetches its mTLS client
// certificate from a user cluster Secret and obtains the ServiceAccount token either from
// a kubelet-projected file or from the user cluster's token subresource.
//
// Every read goes through the user cluster rest.Config handed to the startup hook. The
// sandbox-manager must never present the identity of the cluster it runs in to the identity
// provider, so this file resolves no in-cluster configuration of its own.
const (
	// EnvIdentityProviderCredentialSource selects where the sandbox-manager's remote
	// identity provider client obtains its transport credentials. Values are compared
	// case-insensitively.
	//   "" (default):  the secure provider is not installed and the default UUID
	//                  provider stays in place.
	//   "file":        volume-mounted files, the same layout the controller uses.
	//   "secret":      Secret-backed mTLS client certificate + ServiceAccount token
	//                  from a projected file or the user cluster's token subresource.
	EnvIdentityProviderCredentialSource = "IDENTITY_PROVIDER_CREDENTIAL_SOURCE"

	// EnvIdentityProviderClientCertSecret overrides the name of the Secret that
	// holds the sandbox-manager mTLS client certificate.
	EnvIdentityProviderClientCertSecret = "IDENTITY_PROVIDER_CLIENT_CERT_SECRET"

	// EnvIdentityProviderClientCertNamespace overrides the namespace of the Secret
	// that holds the sandbox-manager mTLS client certificate.
	EnvIdentityProviderClientCertNamespace = "IDENTITY_PROVIDER_CLIENT_CERT_NAMESPACE"

	// EnvIdentityProviderTokenFile overrides the path of the projected
	// ServiceAccount token read on every request.
	EnvIdentityProviderTokenFile = "IDENTITY_PROVIDER_TOKEN_FILE"
)

// Credential source selector values for EnvIdentityProviderCredentialSource.
const (
	CredentialSourceFile   = "file"
	CredentialSourceSecret = "secret"
)

// Defaults for the Secret-backed credential source.
const (
	// DefaultClientCertSecretName is the Secret that holds the sandbox-manager mTLS
	// client certificate. RBAC to read it is granted externally by the identity
	// provider side; the sandbox-manager itself adds no Secret RBAC.
	DefaultClientCertSecretName = "agent-identity-provider-sandbox-manager-client-cert"

	// DefaultClientCertSecretNamespace is the namespace of the client certificate
	// Secret.
	DefaultClientCertSecretNamespace = "sandbox-system"

	// DefaultProjectedTokenFile is the projected ServiceAccount token path read on
	// every request so short-lived, auto-rotated tokens are always current.
	DefaultProjectedTokenFile = "/var/run/secrets/agent-identity-provider/token" //nolint:gosec // G101 false positive: a file path, not a credential
)

// Secret data keys for the mTLS client certificate material. They match the
// conventional file names used by the controller's volume-mounted deployment
// (ca.crt/client.crt/client.key) so both deployments share one certificate layout.
const (
	SecretDataKeyCACert     = "ca.crt"
	SecretDataKeyClientCert = "client.crt"
	SecretDataKeyClientKey  = "client.key"
)

// certReadTimeout bounds the single client certificate Secret Get performed at startup.
const certReadTimeout = 30 * time.Second

// userClusterClientBuilder builds the Kubernetes client used for the client certificate
// Secret Get and, in TokenRequest mode, the ServiceAccount token subresource. Both go
// through the user cluster configuration handed to the startup hook. It is a package
// variable so unit tests can inject a fake clientset.
var userClusterClientBuilder = func(cfg *rest.Config) (kubernetes.Interface, error) {
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create user cluster client for identity provider credentials: %w", err)
	}
	return clientset, nil
}

// InstallSecretBackedProviderFromEnv installs the Secret-backed secure identity
// provider when IDENTITY_PROVIDER_CREDENTIAL_SOURCE=secret. It is reached from the
// sandbox-manager startup hook through InstallProviderFromEnv, so no controller-only
// feature gate is involved in enabling it.
//
// cfg is the user cluster configuration established by the binary. ctx is the hook
// context: it bounds the startup certificate read and the first TokenRequest, and it is
// the cancellation boundary of the background refresh started in TokenRequest mode.
//
// When the credential source is not "secret" it is a no-op and returns nil, leaving the
// currently registered provider in place. When it is "secret" it
// loads the mTLS client certificate and, in TokenRequest mode, acquires the first
// ServiceAccount token before returning; any failure is returned so the caller can fail
// fast, ensuring the manager never silently degrades to UUID tokens after secure mode was
// explicitly requested.
func InstallSecretBackedProviderFromEnv(ctx context.Context, cfg *rest.Config) error {
	source := strings.TrimSpace(os.Getenv(EnvIdentityProviderCredentialSource))
	if source == "" || strings.EqualFold(source, CredentialSourceFile) {
		return nil
	}
	if !strings.EqualFold(source, CredentialSourceSecret) {
		return fmt.Errorf("unknown %s value %q (expected %q or %q)",
			EnvIdentityProviderCredentialSource, source, CredentialSourceFile, CredentialSourceSecret)
	}
	return installSecretBackedProvider(ctx, cfg)
}

func installSecretBackedProvider(ctx context.Context, cfg *rest.Config) error {
	endpoint := os.Getenv(EnvIdentityProviderEndpoint)
	if endpoint == "" {
		return fmt.Errorf("%s not set while %s=%s", EnvIdentityProviderEndpoint,
			EnvIdentityProviderCredentialSource, CredentialSourceSecret)
	}

	clientset, err := userClusterClientBuilder(cfg)
	if err != nil {
		return err
	}

	opts, tokenSource, err := secretBackedOptionsFromEnv(ctx, clientset, endpoint)
	if err != nil {
		return err
	}

	tokenProvider, err := NewIdentityProviderTokenProvider(opts)
	if err != nil {
		return fmt.Errorf("failed to create secret-backed identity provider token provider: %w", err)
	}

	// Started only once the transport is known to be valid, so a local HTTPS
	// misconfiguration fails startup without contacting the user cluster API and without
	// leaving a refresh goroutine behind. Startup must not open the business endpoints
	// before a usable credential exists, so a failed first request aborts here.
	if tokenSource != nil {
		if err := tokenSource.start(ctx); err != nil {
			return fmt.Errorf("failed to acquire the initial identity provider service account token: %w", err)
		}
	}

	// Registering here rather than in init() is safe because the hook runs on the main
	// goroutine before the binary starts any controller, listener or other goroutine that
	// could call into pkg/identity, so the write happens-before every reader. Keeping that
	// ordering is a requirement: pkg/identity holds the provider in a plain package
	// variable and does not synchronize it.
	identity.RegisterProvider(newSecureIdentityProvider(tokenProvider))

	tokenServiceAccount := ""
	if tokenSource != nil {
		tokenServiceAccount = tokenSource.saName
	}
	klog.InfoS("identity provider installed in secret-backed secure mode",
		"endpoint", loggableEndpoint(endpoint),
		"clientCertSecret", envOrDefault(EnvIdentityProviderClientCertSecret, DefaultClientCertSecretName),
		"clientCertNamespace", envOrDefault(EnvIdentityProviderClientCertNamespace, DefaultClientCertSecretNamespace),
		"tokenServiceAccount", tokenServiceAccount)
	return nil
}

// secretBackedOptionsFromEnv assembles IdentityProviderOptions for the sandbox-manager
// deployment. The mTLS material is fetched once at startup (fail-fast on error); the
// endpoint reuses the shared IDENTITY_PROVIDER_ENDPOINT so both deployments are configured
// with a single, consistent variable.
//
// The Bearer token source depends on IDENTITY_PROVIDER_TOKEN_SERVICE_ACCOUNT:
//   - empty: the kubelet-projected token file is re-read on every request and the
//     returned source is nil. Startup performs no probe, so an unreadable or empty file
//     surfaces as a credential error on the business call that needs it.
//   - set:   the returned source holds an in-memory credential. The caller starts it once
//     the transport is known to be valid.
func secretBackedOptionsFromEnv(ctx context.Context, clientset kubernetes.Interface, endpoint string) (IdentityProviderOptions, *saTokenSource, error) {
	opts := IdentityProviderOptions{
		Endpoint: endpoint,
		// Both sandbox-manager Secret modes carry a tenant client certificate and a
		// ServiceAccount token, so they refuse HTTP, skipped verification and redirects.
		StrictTransport: true,
	}

	// Optional knobs shared with the file-based path.
	if timeoutStr := os.Getenv(EnvIdentityProviderTimeout); timeoutStr != "" {
		if d, perr := time.ParseDuration(timeoutStr); perr == nil {
			opts.Timeout = d
		} else {
			klog.ErrorS(perr, "invalid identity provider timeout value, falling back to default",
				"envVar", EnvIdentityProviderTimeout, "value", timeoutStr)
		}
	}
	// Parsed rather than ignored so that an operator who enables it gets an explicit
	// startup failure from validateStrictTransport instead of a silently different
	// transport than the one requested.
	if skipVerifyStr := os.Getenv(EnvIdentityProviderInsecureSkipVerify); skipVerifyStr != "" {
		if v, perr := strconv.ParseBool(skipVerifyStr); perr == nil {
			opts.InsecureSkipVerify = v
		}
	}

	// The strict transport rules depend only on local configuration, so they are enforced
	// before the certificate Secret Get: a misconfigured endpoint or TLS setup must fail
	// startup without contacting the user cluster API.
	if err := validateStrictTransport(opts); err != nil {
		return IdentityProviderOptions{}, nil, err
	}

	caPEM, certPEM, keyPEM, err := fetchClientCertSecret(ctx, clientset)
	if err != nil {
		return IdentityProviderOptions{}, nil, err
	}
	opts.CAData = caPEM
	opts.CertData = certPEM
	opts.KeyData = keyPEM

	var tokenSource *saTokenSource
	if saName := strings.TrimSpace(os.Getenv(EnvIdentityProviderTokenServiceAccount)); saName != "" {
		tokenSource = &saTokenSource{
			clientset: clientset,
			saName:    saName,
			audience:  envOrDefault(EnvIdentityProviderTokenAudience, DefaultTokenAudience),
		}
		opts.TokenFunc = tokenSource.token
	} else {
		tokenFile := envOrDefault(EnvIdentityProviderTokenFile, DefaultProjectedTokenFile)
		opts.TokenFunc = func() (string, error) {
			data, readErr := os.ReadFile(tokenFile) //nolint:gosec // G304: operator-configured projected token path, not user input
			if readErr != nil {
				return "", fmt.Errorf("failed to read identity provider token file %s: %w", tokenFile, readErr)
			}
			// An empty projection must not fall through to an anonymous identity
			// provider call: kubelet can transiently truncate the file while rotating it.
			token := strings.TrimSpace(string(data))
			if token == "" {
				return "", fmt.Errorf("identity provider token file %s is empty", tokenFile)
			}
			return token, nil
		}
	}

	return opts, tokenSource, nil
}

// fetchClientCertSecret reads the mTLS client certificate material from the user cluster
// with a single bounded Secret Get. No informer, List or APIReader is involved: the
// certificate is loaded once at startup and a replacement is picked up by a process
// restart.
//
// client.crt and client.key are required; ca.crt is optional and the system root pool is
// used when absent.
func fetchClientCertSecret(ctx context.Context, clientset kubernetes.Interface) (caPEM, certPEM, keyPEM []byte, err error) {
	secretName := envOrDefault(EnvIdentityProviderClientCertSecret, DefaultClientCertSecretName)
	secretNamespace := envOrDefault(EnvIdentityProviderClientCertNamespace, DefaultClientCertSecretNamespace)

	getCtx, cancel := context.WithTimeout(quietContext(ctx), certReadTimeout)
	defer cancel()

	secret, err := clientset.CoreV1().Secrets(secretNamespace).Get(getCtx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to get identity provider client certificate secret %s/%s: %s",
			secretNamespace, secretName, describeRemoteFailure(err, 0))
	}

	certPEM = secret.Data[SecretDataKeyClientCert]
	keyPEM = secret.Data[SecretDataKeyClientKey]
	caPEM = secret.Data[SecretDataKeyCACert]
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return nil, nil, nil, fmt.Errorf("identity provider client certificate secret %s/%s missing %q or %q data key",
			secretNamespace, secretName, SecretDataKeyClientCert, SecretDataKeyClientKey)
	}

	klog.InfoS("identity provider client certificate loaded from secret",
		"secret", secretNamespace+"/"+secretName,
		"caBytes", len(caPEM), "certBytes", len(certPEM), "keyBytes", len(keyPEM))
	return caPEM, certPEM, keyPEM, nil
}

// envOrDefault returns the trimmed value of the environment variable when set to a
// non-empty value, otherwise the provided default.
func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
