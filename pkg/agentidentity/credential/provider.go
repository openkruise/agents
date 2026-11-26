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

// Package credential resolves CredentialProviders into OAuth2 clients and
// keeps the delegations made through them usable.
//
// Section 10 names this the credential module: the provider registry, the
// operator-defined OAuth endpoints, and the delegations they produce. The
// records themselves are stored by pkg/identity/delegation; this package is
// what acts on them against a provider's token endpoint.
package credential

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/oauth2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
)

var (
	// ErrProviderNotFound means no CredentialProvider of that name exists in
	// the namespace. Section 7.6 reports it as PROVIDER_NOT_FOUND, which a
	// caller should not retry.
	ErrProviderNotFound = errors.New("credential: provider not found")

	// ErrProviderNotReady means the provider exists but cannot be used now: it
	// is being deleted, its controller has not reported it Ready for the
	// current generation, or its client secret has gone. Section 7.6 reports it
	// as PROVIDER_NOT_READY, which waits for the configuration to recover.
	ErrProviderNotReady = errors.New("credential: provider is not ready")
)

// Provider is a CredentialProvider resolved into an OAuth2 client for its
// token endpoint.
//
// It holds the plaintext client secret, which section 9 forbids from reaching
// a log, status or event, so nothing in it is exported and String and GoString
// name only which provider it is. Without them fmt's %v would walk the
// unexported fields and print the secret anyway.
type Provider struct {
	key    client.ObjectKey
	config oauth2.Config
}

// String identifies the provider without its client.
func (p Provider) String() string {
	return "credential.Provider{" + p.key.String() + "}"
}

// GoString redacts the provider for %#v, which does not call String.
func (p Provider) GoString() string {
	return p.String()
}

// LoadProvider reads the CredentialProvider at key and the client secret it
// references, and builds the OAuth2 client the two describe.
//
// It requires the provider to be Ready for its current generation rather than
// repeating the controller's checks, because readiness is the controller's
// verdict on exactly those checks. The client secret is still read here, since
// it is the one value the controller never hands out, and a Secret deleted
// after the controller last looked leaves the provider unusable whatever its
// status still says (section 5.7).
//
// reader should be informer-backed, the same client the controllers read
// through.
func LoadProvider(ctx context.Context, reader client.Reader, key client.ObjectKey) (*Provider, error) {
	provider := &securityv1alpha1.CredentialProvider{}
	if err := reader.Get(ctx, key, provider); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrProviderNotFound, key)
		}
		return nil, fmt.Errorf("credential: read provider %s: %w", key, err)
	}
	if !provider.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("%w: %s is being deleted", ErrProviderNotReady, key)
	}
	if !readyForGeneration(provider) {
		return nil, fmt.Errorf("%w: %s is not Ready for generation %d",
			ErrProviderNotReady, key, provider.Generation)
	}
	spec := provider.Spec.OAuth2
	if spec == nil {
		// The controller never reports such a provider Ready, so whatever wrote
		// that status was not the controller. Fail closed regardless.
		return nil, fmt.Errorf("%w: %s has no oauth2 configuration", ErrProviderNotReady, key)
	}

	selector := spec.Client.ClientSecret.SecretKeyRef
	secret := &corev1.Secret{}
	secretKey := client.ObjectKey{Namespace: provider.Namespace, Name: selector.Name}
	if err := reader.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: client secret %s does not exist", ErrProviderNotReady, secretKey)
		}
		return nil, fmt.Errorf("credential: read client secret %s: %w", secretKey, err)
	}
	clientSecret := secret.Data[selector.Key]
	if len(clientSecret) == 0 {
		return nil, fmt.Errorf("%w: client secret %s has no non-empty key %q",
			ErrProviderNotReady, secretKey, selector.Key)
	}

	return &Provider{
		key: key,
		config: oauth2.Config{
			ClientID:     spec.Client.ClientID,
			ClientSecret: string(clientSecret),
			Endpoint: oauth2.Endpoint{
				AuthURL:  spec.Discovery.AuthorizationEndpoint,
				TokenURL: spec.Discovery.TokenEndpoint,
				// A Provider is built per call, so x/oauth2's per-Config cache of
				// the auth style it probed for never fills, and left to detect
				// it every request would try the token endpoint twice. GitHub,
				// the provider this proposal is written against, takes the
				// client credentials as POST parameters, which RFC 6749 section
				// 2.3.1 also allows, so that style is fixed instead.
				AuthStyle: oauth2.AuthStyleInParams,
			},
		},
	}, nil
}

// readyForGeneration reports whether the controller has judged the provider
// Ready for the spec it has now, not for an earlier one.
func readyForGeneration(provider *securityv1alpha1.CredentialProvider) bool {
	if provider.Status.ObservedGeneration != provider.Generation {
		return false
	}
	ready := apiMeta.FindStatusCondition(provider.Status.Conditions, securityv1alpha1.ConditionReady)
	return ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == provider.Generation
}

// refresh redeems refreshToken at the provider's token endpoint.
//
// The HTTP client comes from ctx through oauth2.HTTPClient. Without one,
// x/oauth2 falls back to http.DefaultClient, which has no timeout.
func (p *Provider) refresh(ctx context.Context, refreshToken string) (*oauth2.Token, error) {
	// With no access token the source treats the token as expired, so Token
	// goes straight to the refresh grant.
	return p.config.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken}).Token()
}
