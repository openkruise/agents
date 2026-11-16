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
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openkruise/agents/pkg/identity/jwtissuer"
	"github.com/openkruise/agents/pkg/identity/oidc"
)

func TestVerifySessionServiceToken(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, otherKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	mux := http.NewServeMux()
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	issuer, err := jwtissuer.New(server.URL, jwtissuer.SigningKey{KeyID: "serving-test", PrivateKey: key})
	require.NoError(t, err)
	handler, err := issuer.Handler()
	require.NoError(t, err)
	mux.Handle("/", handler)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	ca := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "oidc-ca"},
		Data:       map[string]string{"ca.crt": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ca).Build()
	opts := oidc.Options{DiscoveryURL: server.URL + jwtissuer.DiscoveryPath, CAConfigMapNamespace: ca.Namespace, CAConfigMapName: ca.Name}
	verifier, err := oidc.NewVerifier(t.Context(), reader, opts)
	require.NoError(t, err)

	for _, tt := range []struct {
		name                   string
		mutate                 func(map[string]any)
		id, uid                string
		rawToken               string
		missingToken, otherKey bool
		wantError              string
	}{
		{name: "matching SessionService"},
		{name: "informational metadata", mutate: func(c map[string]any) {
			c["metadata"] = map[string]string{"sessionServiceId": "informational-id", "sessionServiceUid": "informational-uid", "servingNamespace": "team-a", "servingName": "demo"}
		}},
		{name: "different SessionService", id: "serving-b", wantError: "does not match SessionService"},
		{name: "recreated SessionService", uid: "uid-b", wantError: "does not match SessionService"},
		{name: "missing binding", mutate: func(c map[string]any) { delete(c, "sessionService") }, wantError: "does not match SessionService"},
		{name: "null binding", mutate: func(c map[string]any) { c["sessionService"] = nil }, wantError: "does not match SessionService"},
		{name: "empty binding", mutate: func(c map[string]any) { c["sessionService"] = map[string]string{} }, wantError: "does not match SessionService"},
		{name: "missing binding ID", mutate: func(c map[string]any) { c["sessionService"] = map[string]string{"sessionServiceUid": "uid-a"} }, wantError: "does not match SessionService"},
		{name: "missing binding UID", mutate: func(c map[string]any) { c["sessionService"] = map[string]string{"sessionServiceId": "serving-a"} }, wantError: "does not match SessionService"},
		{name: "empty binding ID", mutate: func(c map[string]any) {
			c["sessionService"] = map[string]string{"sessionServiceId": "", "sessionServiceUid": "uid-a"}
		}, wantError: "does not match SessionService"},
		{name: "empty binding UID", mutate: func(c map[string]any) {
			c["sessionService"] = map[string]string{"sessionServiceId": "serving-a", "sessionServiceUid": ""}
		}, wantError: "does not match SessionService"},
		{name: "wrong binding ID", mutate: func(c map[string]any) {
			c["sessionService"] = map[string]string{"sessionServiceId": "other", "sessionServiceUid": "uid-a"}
		}, wantError: "does not match SessionService"},
		{name: "wrong binding UID", mutate: func(c map[string]any) {
			c["sessionService"] = map[string]string{"sessionServiceId": "serving-a", "sessionServiceUid": "other"}
		}, wantError: "does not match SessionService"},
		{name: "non-string binding", mutate: func(c map[string]any) {
			c["sessionService"] = map[string]any{"sessionServiceId": 1, "sessionServiceUid": "uid-a"}
		}, wantError: "decode claims"},
		{name: "metadata binding only", mutate: func(c map[string]any) { c["metadata"] = c["sessionService"]; delete(c, "sessionService") }, wantError: "does not match SessionService"},
		{name: "old subject only", mutate: func(c map[string]any) { c["sub"] = "serving:serving-a:uid-a"; delete(c, "sessionService") }, wantError: "does not match SessionService"},
		{name: "empty resource subject", id: "", uid: "", mutate: func(c map[string]any) { c["sub"] = "serving::" }, wantError: "does not match SessionService"},
		{name: "legacy prefix", mutate: func(c map[string]any) { c["sub"] = "e2b:serving:serving-a:uid-a" }, wantError: "does not match SessionService"},
		{name: "Sandbox token", mutate: func(c map[string]any) {
			c["sub"] = "e2b:controlplane:team--sandbox-a"
			c["sandbox"] = map[string]string{"sandboxId": "team--sandbox-a", "sandboxUid": "sandbox-uid"}
		}, wantError: "does not match SessionService"},
		{name: "mixed Sandbox binding", mutate: func(c map[string]any) {
			c["sandbox"] = map[string]string{"sandboxId": "team--sandbox-a", "sandboxUid": "sandbox-uid"}
		}, wantError: "does not match SessionService"},
		{name: "empty Sandbox binding", mutate: func(c map[string]any) { c["sandbox"] = map[string]string{} }, wantError: "does not match SessionService"},
		{name: "expired", mutate: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }, wantError: "validate token claims"},
		{name: "wrong issuer", mutate: func(c map[string]any) { c["iss"] = "https://other.example" }, wantError: "does not match expected issuer"},
		{name: "wrong signature", otherKey: true, wantError: "verify token signature"},
		{name: "UUID placeholder", rawToken: "58cb544a-4a4d-40ec-91c5-093d6a91b539", wantError: "parse signed token"},
		{name: "missing token", missingToken: true, wantError: "token must not be empty"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id, uid := "serving-a", "uid-a"
			if tt.id != "" {
				id = tt.id
			}
			if tt.uid != "" {
				uid = tt.uid
			}
			claims := map[string]any{
				"iss": server.URL, "sub": "sessionservice:controlplane:serving-a",
				"sessionService": map[string]string{"sessionServiceId": "serving-a", "sessionServiceUid": "uid-a"},
				"iat":            time.Now().Add(-time.Minute).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(time.Hour).Unix(),
			}
			if tt.mutate != nil {
				tt.mutate(claims)
			}
			raw := tt.rawToken
			if raw == "" && !tt.missingToken {
				privateKey := key
				if tt.otherKey {
					privateKey = otherKey
				}
				signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: privateKey}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "serving-test"))
				require.NoError(t, err)
				raw, err = jwt.Signed(signer).Claims(claims).Serialize()
				require.NoError(t, err)
			}
			err := VerifySessionServiceToken(verifier, raw, id, uid)
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
				return
			}
			require.NoError(t, err)
		})
	}

	for _, ids := range [][2]string{{"", "uid-a"}, {"serving-a", ""}} {
		t.Run("missing trusted identity/"+ids[0]+"/"+ids[1], func(t *testing.T) {
			require.ErrorContains(t, VerifySessionServiceToken(verifier, "not-a-token", ids[0], ids[1]), "required")
		})
	}

	t.Run("verifier unavailable", func(t *testing.T) {
		require.ErrorIs(t, VerifySessionServiceToken(nil, "not-a-token", "serving-a", "uid-a"), ErrVerifierUnavailable)
	})
}
