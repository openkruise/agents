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
	"fmt"
	"net/url"
	"os"
	"time"
)

// Default verifier configuration.
const (
	DefaultDiscoveryURL         = "https://id-provider.ack-agent-identity.svc:8445/.well-known/openid-configuration"
	DefaultCAConfigMapNamespace = "ack-agent-identity"
	DefaultCAConfigMapName      = "ack-agent-identity-root-ca.crt"
	DefaultCAConfigMapKey       = "ca.crt"
	DefaultClockSkew            = time.Minute
	DefaultHTTPTimeout          = 10 * time.Second
	DefaultMaxResponseSize      = int64(1 << 20)
	DefaultMaxTokenSize         = 64 << 10
)

const (
	envDiscoveryURL         = "OIDC_DISCOVERY_URL"
	envCAConfigMapNamespace = "OIDC_CA_CONFIGMAP_NAMESPACE"
	envCAConfigMapName      = "OIDC_CA_CONFIGMAP_NAME"
	envCAConfigMapKey       = "OIDC_CA_CONFIGMAP_KEY"
	envClockSkew            = "OIDC_CLOCK_SKEW"
)

// Options configures verifier initialization and local token validation.
type Options struct {
	DiscoveryURL         string
	CAConfigMapNamespace string
	CAConfigMapName      string
	CAConfigMapKey       string
	ClockSkew            time.Duration
	HTTPTimeout          time.Duration
	MaxResponseSize      int64
	MaxTokenSize         int
}

// OptionsFromEnvironment returns default options with supported environment overrides.
func OptionsFromEnvironment() (Options, error) {
	opts := defaultOptions()
	if value := os.Getenv(envDiscoveryURL); value != "" {
		opts.DiscoveryURL = value
	}
	if value := os.Getenv(envCAConfigMapNamespace); value != "" {
		opts.CAConfigMapNamespace = value
	}
	if value := os.Getenv(envCAConfigMapName); value != "" {
		opts.CAConfigMapName = value
	}
	if value := os.Getenv(envCAConfigMapKey); value != "" {
		opts.CAConfigMapKey = value
	}
	if value := os.Getenv(envClockSkew); value != "" {
		clockSkew, err := time.ParseDuration(value)
		if err != nil {
			return Options{}, fmt.Errorf("parse %s: %w", envClockSkew, err)
		}
		opts.ClockSkew = clockSkew
	}

	if err := validateOptions(opts); err != nil {
		return Options{}, err
	}
	return opts, nil
}

func defaultOptions() Options {
	return Options{
		DiscoveryURL:         DefaultDiscoveryURL,
		CAConfigMapNamespace: DefaultCAConfigMapNamespace,
		CAConfigMapName:      DefaultCAConfigMapName,
		CAConfigMapKey:       DefaultCAConfigMapKey,
		ClockSkew:            DefaultClockSkew,
		HTTPTimeout:          DefaultHTTPTimeout,
		MaxResponseSize:      DefaultMaxResponseSize,
		MaxTokenSize:         DefaultMaxTokenSize,
	}
}

func withDefaults(opts Options) Options {
	defaults := defaultOptions()
	if opts.DiscoveryURL == "" {
		opts.DiscoveryURL = defaults.DiscoveryURL
	}
	if opts.CAConfigMapNamespace == "" {
		opts.CAConfigMapNamespace = defaults.CAConfigMapNamespace
	}
	if opts.CAConfigMapName == "" {
		opts.CAConfigMapName = defaults.CAConfigMapName
	}
	if opts.CAConfigMapKey == "" {
		opts.CAConfigMapKey = defaults.CAConfigMapKey
	}
	if opts.ClockSkew == 0 {
		opts.ClockSkew = defaults.ClockSkew
	}
	if opts.HTTPTimeout == 0 {
		opts.HTTPTimeout = defaults.HTTPTimeout
	}
	if opts.MaxResponseSize == 0 {
		opts.MaxResponseSize = defaults.MaxResponseSize
	}
	if opts.MaxTokenSize == 0 {
		opts.MaxTokenSize = defaults.MaxTokenSize
	}
	return opts
}

func validateOptions(opts Options) error {
	discoveryURL, err := url.Parse(opts.DiscoveryURL)
	if err != nil || !discoveryURL.IsAbs() || discoveryURL.Scheme != "https" || discoveryURL.Host == "" {
		return fmt.Errorf("discovery URL must be an absolute HTTPS URL")
	}
	if opts.CAConfigMapNamespace == "" {
		return fmt.Errorf("CA ConfigMap namespace must not be empty")
	}
	if opts.CAConfigMapName == "" {
		return fmt.Errorf("CA ConfigMap name must not be empty")
	}
	if opts.CAConfigMapKey == "" {
		return fmt.Errorf("CA ConfigMap key must not be empty")
	}
	if opts.ClockSkew < 0 {
		return fmt.Errorf("clock skew must not be negative")
	}
	if opts.HTTPTimeout <= 0 {
		return fmt.Errorf("HTTP timeout must be positive")
	}
	if opts.MaxResponseSize <= 0 {
		return fmt.Errorf("maximum response size must be positive")
	}
	if opts.MaxTokenSize <= 0 {
		return fmt.Errorf("maximum token size must be positive")
	}
	return nil
}
