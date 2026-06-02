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

// Package keystest exposes test-only constructors for keys.SystemKeyStore.
// Importers must restrict usage to _test.go files; production code must keep
// using keys.NewSystemKeyStore + EnsureKeys against the real API server.
package keystest

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openkruise/agents/pkg/servers/e2b/keys"
)

// testNamespace is an arbitrary namespace used only by the in-memory fake
// client; it never reaches a real apiserver.
const testNamespace = "sandbox-system"

// NewConnectStore returns a fully-initialised SystemKeyStore whose lookup map
// resolves the given plaintext to the connect-scope system key def. It drives
// the public EnsureKeys path against a fake apiserver so test behaviour stays
// aligned with production semantics rather than poking unexported state.
func NewConnectStore(t testing.TB, value string) *keys.SystemKeyStore {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace,
			Name:      keys.ConnectSystemKeySecretName,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{keys.SystemKeyDataKey: []byte(value)},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	store := keys.NewSystemKeyStore(fc, fc, testNamespace)
	if err := store.EnsureKeys(context.Background()); err != nil {
		t.Fatalf("EnsureKeys: %v", err)
	}
	return store
}
