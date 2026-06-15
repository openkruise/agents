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

package keys

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const testSystemNamespace = "sandbox-system"

func newStoreSchemeForTest(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	return scheme
}

func connectSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ConnectSystemKeySecretName, Namespace: testSystemNamespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

// withCatalog temporarily swaps the package catalog for a test and restores it.
func withCatalog(t *testing.T, c []SystemKeyDef) {
	t.Helper()
	orig := systemKeyCatalog
	systemKeyCatalog = c
	t.Cleanup(func() { systemKeyCatalog = orig })
}

func TestSystemKeyStore_Lookup(t *testing.T) {
	def := &SystemKeyDef{Name: "system"}
	s := &SystemKeyStore{byValue: map[string]*SystemKeyDef{"abc": def}}
	tests := []struct {
		name      string
		presented string
		wantOK    bool
	}{
		{name: "hit", presented: "abc", wantOK: true},
		{name: "miss", presented: "xyz", wantOK: false},
		{name: "empty", presented: "", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := s.Lookup(tt.presented)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Same(t, def, got)
			}
		})
	}
}

func TestSystemKeyStore_EnsureOne_GeneratesWhenEmpty(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(newStoreSchemeForTest(t)).
		WithObjects(connectSecret(map[string][]byte{})).Build()
	s := NewSystemKeyStore(fc, fc, testSystemNamespace)

	value, err := s.ensureOne(context.Background(), &systemKeyCatalog[0])
	require.NoError(t, err)
	require.NotEmpty(t, value)

	var stored corev1.Secret
	require.NoError(t, fc.Get(context.Background(), client.ObjectKey{Namespace: testSystemNamespace, Name: ConnectSystemKeySecretName}, &stored))
	assert.Equal(t, value, string(stored.Data[SystemKeyDataKey]))
}

func TestSystemKeyStore_EnsureOne_LoadsVerbatimAndDoesNotUpdate(t *testing.T) {
	tests := []struct {
		name   string
		preset string
	}{
		{name: "plain key", preset: "preset-system-key"},
		{name: "trailing newline preserved", preset: "abc\n"},
		{name: "surrounding spaces preserved", preset: " spaced "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var updates atomic.Int32
			fc := fake.NewClientBuilder().WithScheme(newStoreSchemeForTest(t)).
				WithObjects(connectSecret(map[string][]byte{SystemKeyDataKey: []byte(tt.preset)})).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						updates.Add(1)
						return c.Update(ctx, obj, opts...)
					},
				}).Build()
			s := NewSystemKeyStore(fc, fc, testSystemNamespace)

			value, err := s.ensureOne(context.Background(), &systemKeyCatalog[0])
			require.NoError(t, err)
			assert.Equal(t, tt.preset, value)
			assert.Equal(t, int32(0), updates.Load())
		})
	}
}

func TestSystemKeyStore_EnsureOne_FailsAfterRetryCap(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(newStoreSchemeForTest(t)).Build() // no Secret
	s := NewSystemKeyStore(fc, fc, testSystemNamespace)
	s.retryInterval = time.Millisecond
	s.retryTimeout = 5 * time.Millisecond

	_, err := s.ensureOne(context.Background(), &systemKeyCatalog[0])
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ensure system key")
}

func TestSystemKeyStore_EnsureOne_HonorsContextCancelDuringRetry(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(newStoreSchemeForTest(t)).Build()
	s := NewSystemKeyStore(fc, fc, testSystemNamespace)
	s.retryInterval = time.Hour
	s.retryTimeout = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.ensureOne(ctx, &systemKeyCatalog[0])

	assert.ErrorIs(t, err, context.Canceled)
}

func TestSystemKeyStore_EnsureOne_RetriesOnConflictThenConverges(t *testing.T) {
	gvr := schema.GroupResource{Resource: "secrets"}
	var updates atomic.Int32
	fc := fake.NewClientBuilder().WithScheme(newStoreSchemeForTest(t)).
		WithObjects(connectSecret(map[string][]byte{})).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if updates.Add(1) == 1 {
					winner := obj.DeepCopyObject().(*corev1.Secret)
					if winner.Data == nil {
						winner.Data = map[string][]byte{}
					}
					winner.Data[SystemKeyDataKey] = []byte("winner-system-key")
					require.NoError(t, c.Update(ctx, winner, opts...))
					return apierrors.NewConflict(gvr, ConnectSystemKeySecretName, errors.New("simulated conflict"))
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
	s := NewSystemKeyStore(fc, fc, testSystemNamespace)
	s.retryInterval = time.Millisecond

	value, err := s.ensureOne(context.Background(), &systemKeyCatalog[0])
	require.NoError(t, err)
	assert.Equal(t, "winner-system-key", value)
	assert.Equal(t, int32(1), updates.Load())
}

func TestSystemKeyStore_EnsureKeys_ReturnsEnsureOneError(t *testing.T) {
	freshID := uuid.MustParse("00000000-0000-0000-0000-0000000000bb")
	withCatalog(t, []SystemKeyDef{
		{Name: "ready", ID: SystemKeyIDConnect, SecretName: "ready-secret", Scopes: []SystemAuth{SystemAuthConnect}, CrossOwner: true},
		{Name: "missing", ID: freshID, SecretName: "missing-secret", Scopes: []SystemAuth{SystemAuthConnect}, CrossOwner: true},
	})
	fc := fake.NewClientBuilder().WithScheme(newStoreSchemeForTest(t)).
		WithObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "ready-secret", Namespace: testSystemNamespace},
			Data:       map[string][]byte{SystemKeyDataKey: []byte("ready-system-key")},
		}).Build()
	s := NewSystemKeyStore(fc, fc, testSystemNamespace)
	s.retryInterval = time.Millisecond
	s.retryTimeout = 5 * time.Millisecond

	err := s.EnsureKeys(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ensure system key")
	assert.Contains(t, err.Error(), "missing")
}

func TestSystemKeyStore_EnsureKeys_BuildsMap(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(newStoreSchemeForTest(t)).
		WithObjects(connectSecret(map[string][]byte{SystemKeyDataKey: []byte("the-key")})).Build()
	s := NewSystemKeyStore(fc, fc, testSystemNamespace)

	require.NoError(t, s.EnsureKeys(context.Background()))
	def, ok := s.Lookup("the-key")
	require.True(t, ok)
	assert.Equal(t, SystemKeyNameConnect, def.Name)
	assert.Equal(t, SystemKeyIDConnect, def.ID)
}

func TestSystemKeyStore_EnsureKeys_FailsClosed(t *testing.T) {
	freshID := uuid.MustParse("00000000-0000-0000-0000-0000000000aa")
	tests := []struct {
		name    string
		catalog []SystemKeyDef
		objects []client.Object
		wantErr string
	}{
		{
			name: "duplicate name",
			catalog: []SystemKeyDef{
				{Name: "dup", ID: SystemKeyIDConnect, SecretName: "s1", Scopes: []SystemAuth{SystemAuthConnect}, CrossOwner: true},
				{Name: "dup", ID: freshID, SecretName: "s2", Scopes: []SystemAuth{SystemAuthConnect}, CrossOwner: true},
			},
			wantErr: "duplicate system key name",
		},
		{
			name: "duplicate id",
			catalog: []SystemKeyDef{
				{Name: "a", ID: SystemKeyIDConnect, SecretName: "s1", Scopes: []SystemAuth{SystemAuthConnect}, CrossOwner: true},
				{Name: "b", ID: SystemKeyIDConnect, SecretName: "s2", Scopes: []SystemAuth{SystemAuthConnect}, CrossOwner: true},
			},
			wantErr: "duplicate system key id",
		},
		{
			name: "duplicate secret",
			catalog: []SystemKeyDef{
				{Name: "a", ID: SystemKeyIDConnect, SecretName: "s1", Scopes: []SystemAuth{SystemAuthConnect}, CrossOwner: true},
				{Name: "b", ID: freshID, SecretName: "s1", Scopes: []SystemAuth{SystemAuthConnect}, CrossOwner: true},
			},
			wantErr: "duplicate system key secret",
		},
		{
			name: "cross owner false rejected",
			catalog: []SystemKeyDef{
				{Name: "n", ID: SystemKeyIDConnect, SecretName: "s1", Scopes: []SystemAuth{SystemAuthConnect}, CrossOwner: false},
			},
			wantErr: "CrossOwner=false",
		},
		{
			name: "duplicate value across secrets",
			catalog: []SystemKeyDef{
				{Name: "a", ID: SystemKeyIDConnect, SecretName: "s1", Scopes: []SystemAuth{SystemAuthConnect}, CrossOwner: true},
				{Name: "b", ID: freshID, SecretName: "s2", Scopes: []SystemAuth{SystemAuthConnect}, CrossOwner: true},
			},
			objects: []client.Object{
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: testSystemNamespace}, Data: map[string][]byte{SystemKeyDataKey: []byte("same")}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s2", Namespace: testSystemNamespace}, Data: map[string][]byte{SystemKeyDataKey: []byte("same")}},
			},
			wantErr: "resolved to the same Secret value",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withCatalog(t, tt.catalog)
			fc := fake.NewClientBuilder().WithScheme(newStoreSchemeForTest(t)).WithObjects(tt.objects...).Build()
			s := NewSystemKeyStore(fc, fc, testSystemNamespace)
			s.retryInterval = time.Millisecond
			s.retryTimeout = 5 * time.Millisecond

			err := s.EnsureKeys(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
