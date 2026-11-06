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
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/configreload"
)

func TestParseSecretConfig(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pepper     []byte
		omitPepper bool
		empty      bool
		missing    string
		want       string
	}{
		{name: "explicit", pepper: []byte(" p \n"), want: "p"},
		{name: "default-missing", omitPepper: true, want: "default"},
		{name: "default-whitespace", pepper: []byte(" \n"), want: "default"},
		{name: "empty-values", empty: true, want: "default"},
		{name: "missing-admin", missing: configreload.AdminKey},
		{name: "missing-dsn", missing: configreload.DSN},
		{name: "missing-username", missing: configreload.RedisUsername},
		{name: "missing-password", missing: configreload.RedisPassword},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := map[string][]byte{configreload.AdminKey: []byte(" key "), configreload.DSN: []byte(" dsn \n"), configreload.Pepper: tc.pepper, configreload.RedisUsername: []byte(" user "), configreload.RedisPassword: []byte(" secret-sentinel "), "unrelated": []byte("ignored")}
			if tc.omitPepper {
				delete(data, configreload.Pepper)
			}
			if tc.empty {
				for key := range data {
					data[key] = nil
				}
			}
			if tc.missing != "" {
				for key := range data {
					data[key] = []byte("secret-sentinel")
				}
				delete(data, tc.missing)
			}
			cfg, err := parseSecretConfig(data)
			if tc.missing != "" {
				require.ErrorContains(t, err, tc.missing)
				require.NotContains(t, err.Error(), "secret-sentinel")
				return
			}
			require.NoError(t, err)
			if tc.empty {
				require.Equal(t, secretConfig{}, cfg)
			} else {
				require.Equal(t, " key ", cfg.AdminKey)
				require.Equal(t, "dsn", cfg.KeyStorageDSN)
				require.Equal(t, "user", cfg.RedisUsername)
				require.Equal(t, "secret-sentinel", cfg.RedisPassword)
			}
			require.Equal(t, tc.want, cfg.values("default")[configreload.Pepper])
		})
	}
}

func TestSecretConfigSourceRefValidation(t *testing.T) {
	_, err := secretConfigSource(nil, "cfg", "default", secretConfig{})
	require.Error(t, err, "an invalid reference must fail instead of being ignored")
	// Remaining invalid forms are pinned by utils.TestParseSecretRef.
}

// TestSecretConfigSourceWithoutSecret runs the flag/env path end to end: with no
// --secret-config, inputs must still reach components through Reload.
func TestSecretConfigSourceWithoutSecret(t *testing.T) {
	source, err := secretConfigSource(nil, "", "default-pepper", secretConfig{AdminKey: "flag-admin", KeyStorageDSN: "flag-dsn", RedisUsername: "flag-user", RedisPassword: "flag-password"})
	require.NoError(t, err)
	applied := make(chan map[string]string, 1)
	d := configreload.NewDispatcher([]configreload.Component{{
		Name: "flags",
		Keys: []string{configreload.AdminKey, configreload.DSN, configreload.Pepper, configreload.RedisUsername, configreload.RedisPassword},
		Reload: func(_ context.Context, changes map[string]string) (configreload.Result, error) {
			applied <- changes
			return configreload.Accepted, nil
		},
	}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- source(ctx, d) }()
	<-d.Initialized()
	require.Equal(t, map[string]string{configreload.AdminKey: "flag-admin", configreload.DSN: "flag-dsn", configreload.Pepper: "default-pepper", configreload.RedisUsername: "flag-user", configreload.RedisPassword: "flag-password"}, <-applied)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}
