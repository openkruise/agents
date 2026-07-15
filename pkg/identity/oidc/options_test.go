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

package oidc

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOptionsFromEnvironment(t *testing.T) {
	tests := []struct {
		name        string
		environment map[string]string
		assertions  func(*testing.T, Options)
		expectError string
	}{
		{
			name: "defaults",
			assertions: func(t *testing.T, opts Options) {
				assert.Equal(t, DefaultDiscoveryURL, opts.DiscoveryURL)
				assert.Equal(t, DefaultCAConfigMapNamespace, opts.CAConfigMapNamespace)
				assert.Equal(t, DefaultCAConfigMapName, opts.CAConfigMapName)
				assert.Equal(t, DefaultCAConfigMapKey, opts.CAConfigMapKey)
				assert.Equal(t, DefaultClockSkew, opts.ClockSkew)
				assert.Equal(t, DefaultHTTPTimeout, opts.HTTPTimeout)
				assert.Equal(t, DefaultMaxResponseSize, opts.MaxResponseSize)
				assert.Equal(t, DefaultMaxTokenSize, opts.MaxTokenSize)
			},
		},
		{
			name: "overrides",
			environment: map[string]string{
				envDiscoveryURL:         "https://issuer.example/discovery",
				envCAConfigMapNamespace: "identity-system",
				envCAConfigMapName:      "oidc-ca",
				envCAConfigMapKey:       "root.pem",
				envClockSkew:            "15s",
			},
			assertions: func(t *testing.T, opts Options) {
				assert.Equal(t, "https://issuer.example/discovery", opts.DiscoveryURL)
				assert.Equal(t, "identity-system", opts.CAConfigMapNamespace)
				assert.Equal(t, "oidc-ca", opts.CAConfigMapName)
				assert.Equal(t, "root.pem", opts.CAConfigMapKey)
				assert.Equal(t, 15*time.Second, opts.ClockSkew)
			},
		},
		{
			name: "invalid duration",
			environment: map[string]string{
				envClockSkew: "later",
			},
			expectError: envClockSkew,
		},
		{
			name: "negative duration",
			environment: map[string]string{
				envClockSkew: "-1s",
			},
			expectError: "must not be negative",
		},
		{
			name: "HTTP discovery URL",
			environment: map[string]string{
				envDiscoveryURL: "http://issuer.example/discovery",
			},
			expectError: "absolute HTTPS URL",
		},
		{
			name: "relative discovery URL",
			environment: map[string]string{
				envDiscoveryURL: "/discovery",
			},
			expectError: "absolute HTTPS URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, name := range []string{
				envDiscoveryURL,
				envCAConfigMapNamespace,
				envCAConfigMapName,
				envCAConfigMapKey,
				envClockSkew,
			} {
				t.Setenv(name, "")
			}
			for name, value := range tt.environment {
				t.Setenv(name, value)
			}

			opts, err := OptionsFromEnvironment()
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			if tt.assertions != nil {
				tt.assertions(t, opts)
			}
		})
	}
}

func TestValidateOptions(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*Options)
		expectError string
	}{
		{name: "valid"},
		{name: "negative clock skew", mutate: func(opts *Options) { opts.ClockSkew = -time.Second }, expectError: "clock skew"},
		{name: "negative timeout", mutate: func(opts *Options) { opts.HTTPTimeout = -time.Second }, expectError: "HTTP timeout"},
		{name: "negative response size", mutate: func(opts *Options) { opts.MaxResponseSize = -1 }, expectError: "response size"},
		{name: "negative token size", mutate: func(opts *Options) { opts.MaxTokenSize = -1 }, expectError: "token size"},
		{name: "empty namespace", mutate: func(opts *Options) { opts.CAConfigMapNamespace = "" }, expectError: "namespace"},
		{name: "empty name", mutate: func(opts *Options) { opts.CAConfigMapName = "" }, expectError: "name"},
		{name: "empty key", mutate: func(opts *Options) { opts.CAConfigMapKey = "" }, expectError: "key"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := defaultOptions()
			if tt.mutate != nil {
				tt.mutate(&opts)
			}
			err := validateOptions(opts)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
		})
	}
}
