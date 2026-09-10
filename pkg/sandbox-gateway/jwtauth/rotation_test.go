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

package jwtauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openkruise/agents/pkg/identity/oidc"
)

const (
	rotationIssuer      = "https://issuer.example"
	rotationCANamespace = "identity-system"
	rotationCAName      = "oidc-ca"
)

// TestManagerKeyRotation exercises the real OIDC loader against a JWKS endpoint
// that rotates its signing key, which is the outage the refresh loop prevents:
// without refresh, every token signed with the rotated key stays rejected as an
// unknown kid until the process restarts.
func TestManagerKeyRotation(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "rotated signing key is accepted without a restart"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldKey, oldJWK := rotationKey(t, "key-old")
			newKey, newJWK := rotationKey(t, "key-new")

			var mu sync.Mutex
			published := []jose.JSONWebKey{oldJWK}
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/discovery":
					_, _ = fmt.Fprintf(response, `{"issuer":%q,"jwks_uri":%q}`,
						rotationIssuer, "https://"+request.Host+"/jwks")
				case "/jwks":
					mu.Lock()
					keys := published
					mu.Unlock()
					_ = json.NewEncoder(response).Encode(jose.JSONWebKeySet{Keys: keys})
				default:
					response.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			options := oidc.Options{
				DiscoveryURL:         server.URL + "/discovery",
				CAConfigMapNamespace: rotationCANamespace,
				CAConfigMapName:      rotationCAName,
			}
			manager := newManager(func() (oidc.Options, error) {
				return options, nil
			}, oidc.NewVerifier, time.Millisecond, 5*time.Millisecond, 10*time.Millisecond)
			require.NoError(t, manager.Configure(true))
			require.NoError(t, manager.SetReader(rotationReader(t, server)))
			cancel, done := startManager(t, manager)

			waitForState(t, manager, Ready)
			oldToken := rotationToken(t, oldKey, "key-old")
			newToken := rotationToken(t, newKey, "key-new")

			// Before rotation the provider's current key verifies and the
			// not-yet-published key is rejected as an unknown kid.
			_, err := manager.Current().Verify(oldToken)
			require.NoError(t, err)
			_, err = manager.Current().Verify(newToken)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "unknown kid")

			mu.Lock()
			published = []jose.JSONWebKey{newJWK}
			mu.Unlock()

			require.Eventually(t, func() bool {
				_, err := manager.Current().Verify(newToken)
				return err == nil
			}, testTimeout, time.Millisecond, "verifier never picked up the rotated signing key")
			stopManager(t, cancel, done)
		})
	}
}

func rotationKey(t *testing.T, keyID string) (*ecdsa.PrivateKey, jose.JSONWebKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return key, jose.JSONWebKey{
		Key:       &key.PublicKey,
		KeyID:     keyID,
		Use:       "sig",
		Algorithm: string(jose.ES256),
	}
}

func rotationToken(t *testing.T, key *ecdsa.PrivateKey, keyID string) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: key},
		(&jose.SignerOptions{}).WithHeader("kid", keyID),
	)
	require.NoError(t, err)
	now := time.Now()
	rawJWT, err := jwt.Signed(signer).Claims(map[string]interface{}{
		"iss": rotationIssuer,
		"sub": "sandbox-1",
		"iat": now.Add(-time.Minute).Unix(),
		"nbf": now.Add(-time.Minute).Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}).Serialize()
	require.NoError(t, err)
	return rawJWT
}

func rotationReader(t *testing.T, server *httptest.Server) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: rotationCANamespace, Name: rotationCAName},
		Data:       map[string]string{oidc.DefaultCAConfigMapKey: string(caPEM)},
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build()
}
