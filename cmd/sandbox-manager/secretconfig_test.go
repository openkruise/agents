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

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func fullSecretData() map[string][]byte {
	return map[string][]byte{
		E2BAdminKeySecretKey:        []byte("admin"),
		E2BKeyStorageDSNSecretKey:   []byte("dsn"),
		E2BKeyHashPepperSecretKey:   []byte("pepper"),
		QuotaRedisUsernameSecretKey: []byte("user"),
		QuotaRedisPasswordSecretKey: []byte("pass"),
	}
}

func secretWith(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "cfg"},
		Data:       data,
	}
}

func TestParseSecretConfig(t *testing.T) {
	missing := fullSecretData()
	delete(missing, E2BAdminKeySecretKey)

	cases := []struct {
		name        string
		data        map[string][]byte
		errContains string
		want        secretConfig
	}{
		{
			name: "all-empty-ok",
			data: map[string][]byte{E2BAdminKeySecretKey: {}, E2BKeyStorageDSNSecretKey: {}, E2BKeyHashPepperSecretKey: {}, QuotaRedisUsernameSecretKey: {}, QuotaRedisPasswordSecretKey: {}},
		},
		{
			name:        "missing-key",
			data:        missing,
			errContains: E2BAdminKeySecretKey,
		},
		{
			name: "values-present",
			data: fullSecretData(),
			want: secretConfig{AdminKey: "admin", KeyStorageDSN: "dsn", KeyHashPepper: "pepper", RedisUsername: "user", RedisPassword: "pass"},
		},
		{
			name: "admin-not-trimmed-others-trimmed",
			data: map[string][]byte{E2BAdminKeySecretKey: []byte(" x "), E2BKeyStorageDSNSecretKey: []byte("  d  "), E2BKeyHashPepperSecretKey: []byte("\tp\n"), QuotaRedisUsernameSecretKey: []byte(" u "), QuotaRedisPasswordSecretKey: []byte(" w ")},
			want: secretConfig{AdminKey: " x ", KeyStorageDSN: "d", KeyHashPepper: "p", RedisUsername: "u", RedisPassword: "w"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseSecretConfig(tc.data)
			if tc.errContains != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errContains)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg)
		})
	}
}

func TestLoadSecretConfig(t *testing.T) {
	t.Run("ref", func(t *testing.T) {
		c := fake.NewClientBuilder().WithObjects(secretWith(fullSecretData())).Build()
		cases := []struct {
			name      string
			ref       string
			defaultNs string
			wantErr   string
		}{
			{name: "ns-name", ref: "ns/cfg", defaultNs: "sys"},
			{name: "name-only-uses-default-ns", ref: "cfg", defaultNs: "ns"},
			{name: "empty-namespace-uses-default-ns", ref: "/cfg", defaultNs: "ns"},
			{name: "empty-name", ref: "ns/", defaultNs: "sys", wantErr: "Secret name or namespace/name"},
			{name: "empty", ref: "", defaultNs: "sys", wantErr: "Secret name or namespace/name"},
			{name: "extra-slash", ref: "ns/cfg/extra", defaultNs: "sys", wantErr: "Secret name or namespace/name"},
			{name: "name-only-empty-default-ns", ref: "cfg", defaultNs: "", wantErr: "Secret name or namespace/name"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				cfg, err := loadSecretConfig(c, tc.ref, tc.defaultNs)
				if tc.wantErr != "" {
					require.Error(t, err)
					assert.Contains(t, err.Error(), tc.wantErr)
					return
				}
				require.NoError(t, err)
				assert.Equal(t, "admin", cfg.AdminKey)
			})
		}
	})

	t.Run("not-found", func(t *testing.T) {
		c := fake.NewClientBuilder().Build()
		_, err := loadSecretConfig(c, "ns/cfg", "sys")
		require.Error(t, err)
		assert.True(t, apierrors.IsNotFound(err))
		assert.Contains(t, err.Error(), "ns/cfg")
	})
	t.Run("missing-key-wrapped-with-ref", func(t *testing.T) {
		data := fullSecretData()
		delete(data, E2BKeyHashPepperSecretKey)
		c := fake.NewClientBuilder().WithObjects(secretWith(data)).Build()
		_, err := loadSecretConfig(c, "ns/cfg", "sys")
		require.Error(t, err)
		assert.Contains(t, err.Error(), E2BKeyHashPepperSecretKey)
		assert.Contains(t, err.Error(), "ns/cfg")
	})
}

func TestSecretConfigErrorsDoNotLeakValues(t *testing.T) {
	const sentinel = "SUPER_SECRET_SENTINEL"
	data := fullSecretData()
	for k := range data {
		data[k] = []byte(sentinel)
	}
	delete(data, E2BKeyStorageDSNSecretKey)
	_, err := parseSecretConfig(data)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), sentinel)
}

func TestResolveSecretSettings(t *testing.T) {
	current := secretConfig{
		AdminKey:      "flag-admin",
		KeyStorageDSN: "flag-dsn",
		KeyHashPepper: "flag-pepper",
		RedisUsername: "flag-user",
		RedisPassword: "flag-pass",
	}

	t.Run("empty-ref-passthrough", func(t *testing.T) {
		got, err := resolveSecretSettings(nil, "", "sys", current)
		require.NoError(t, err)
		assert.Equal(t, current, got)
	})

	t.Run("secret-overlays-current", func(t *testing.T) {
		c := fake.NewClientBuilder().WithObjects(secretWith(fullSecretData())).Build()
		got, err := resolveSecretSettings(c, "ns/cfg", "sys", current)
		require.NoError(t, err)
		assert.Equal(t, secretConfig{
			AdminKey:      "admin",
			KeyStorageDSN: "dsn",
			KeyHashPepper: "pepper",
			RedisUsername: "user",
			RedisPassword: "pass",
		}, got)
	})

	t.Run("empty-secret-values-overlay-current", func(t *testing.T) {
		c := fake.NewClientBuilder().WithObjects(secretWith(map[string][]byte{
			E2BAdminKeySecretKey:        {},
			E2BKeyStorageDSNSecretKey:   {},
			E2BKeyHashPepperSecretKey:   {},
			QuotaRedisUsernameSecretKey: {},
			QuotaRedisPasswordSecretKey: {},
		})).Build()
		got, err := resolveSecretSettings(c, "ns/cfg", "sys", current)
		require.NoError(t, err)
		assert.Equal(t, secretConfig{}, got)
	})

	t.Run("load-error-wraps-ref", func(t *testing.T) {
		c := fake.NewClientBuilder().Build()
		_, err := resolveSecretSettings(c, "ns/cfg", "sys", current)
		require.Error(t, err)
		assert.True(t, apierrors.IsNotFound(err))
	})
}
