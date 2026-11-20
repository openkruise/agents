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

// Package secureprovider implements an identity.IdentityProvider that issues sandbox
// tokens by calling a remote identity provider service over HTTPS and propagates the
// issued Agent Token into the sandbox runtime.
//
// The package registers nothing on import except its own token propagator. A binary
// opts in explicitly after flag parsing:
//   - agent-sandbox-controller calls InstallFileBackedProviderFromEnv when the
//     SecurityIdentityProvider feature gate is enabled.
//   - sandbox-manager calls InstallProviderFromEnv from its startup hook, selected by
//     IDENTITY_PROVIDER_CREDENTIAL_SOURCE.
//
// The package does not read feature gates itself, so it can be linked into
// sandbox-manager without depending on the controller-only pkg/features.
package secureprovider

import (
	"context"
	"os"
	"strings"

	"k8s.io/client-go/rest"

	"github.com/openkruise/agents/pkg/identity"
)

// InstallFileBackedProviderFromEnv registers the secure identity provider configured
// from volume-mounted files and environment variables (see identityProviderOptionsFromEnv).
//
// It must be called after flag parsing and before any component that issues tokens is
// started, because pkg/identity holds the provider in an unsynchronized package
// variable. On misconfiguration it registers a failingProvider that surfaces the error
// on every call instead of silently issuing UUID tokens.
func InstallFileBackedProviderFromEnv() {
	identity.RegisterProvider(buildSecureProvider())
}

// InstallProviderFromEnv installs the secure identity provider for sandbox-manager
// according to IDENTITY_PROVIDER_CREDENTIAL_SOURCE:
//   - "" (default): no-op, the default UUID provider stays in place.
//   - "file":       InstallFileBackedProviderFromEnv.
//   - "secret":     InstallSecretBackedProviderFromEnv, using cfg to reach the cluster
//     that holds the client certificate Secret and the ServiceAccount.
//
// Any other value is rejected so a typo cannot silently disable secure mode.
func InstallProviderFromEnv(ctx context.Context, cfg *rest.Config) error {
	source := strings.TrimSpace(os.Getenv(EnvIdentityProviderCredentialSource))
	switch {
	case source == "":
		return nil
	case strings.EqualFold(source, CredentialSourceFile):
		InstallFileBackedProviderFromEnv()
		return nil
	default:
		return InstallSecretBackedProviderFromEnv(ctx, cfg)
	}
}
