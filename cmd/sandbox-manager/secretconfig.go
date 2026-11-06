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
	"fmt"
	"strings"

	"k8s.io/client-go/rest"

	"github.com/openkruise/agents/pkg/configreload"
	"github.com/openkruise/agents/pkg/utils"
)

// secretConfig holds normalized flag/env or Secret inputs. Business validation
// belongs to each component's Reload, after applying the pepper default.
type secretConfig struct {
	AdminKey      string
	KeyStorageDSN string
	KeyHashPepper string
	RedisUsername string
	RedisPassword string
}

// parseSecretConfig requires the four non-pepper keys and returns effective inputs.
// Errors mention only key names, never their values. The admin key is used
// verbatim (matching the flag semantics); the other four are whitespace-trimmed
// (matching the env-var semantics) so an accidental trailing newline in Secret
// data does not leak in.
func parseSecretConfig(data map[string][]byte) (secretConfig, error) {
	for _, key := range []string{
		configreload.AdminKey,
		configreload.DSN,
		configreload.RedisUsername,
		configreload.RedisPassword,
	} {
		if _, ok := data[key]; !ok {
			return secretConfig{}, fmt.Errorf("missing required key %q", key)
		}
	}
	return secretConfig{
		AdminKey:      string(data[configreload.AdminKey]),
		KeyStorageDSN: strings.TrimSpace(string(data[configreload.DSN])),
		KeyHashPepper: strings.TrimSpace(string(data[configreload.Pepper])),
		RedisUsername: strings.TrimSpace(string(data[configreload.RedisUsername])),
		RedisPassword: strings.TrimSpace(string(data[configreload.RedisPassword])),
	}, nil
}

func (s secretConfig) values(defaultPepper string) map[string]string {
	pepper := s.KeyHashPepper
	if pepper == "" {
		pepper = defaultPepper
	}
	return map[string]string{
		configreload.AdminKey:      s.AdminKey,
		configreload.DSN:           s.KeyStorageDSN,
		configreload.Pepper:        pepper,
		configreload.RedisUsername: s.RedisUsername,
		configreload.RedisPassword: s.RedisPassword,
	}
}

func secretConfigSource(restConfig *rest.Config, ref, defaultPepper string, initial secretConfig) (func(context.Context, *configreload.Dispatcher) error, error) {
	if ref == "" {
		return func(ctx context.Context, d *configreload.Dispatcher) error {
			return configreload.RunStatic(ctx, d, initial.values(defaultPepper))
		}, nil
	}
	secretRef, err := utils.ParseSecretRef(ref)
	if err != nil {
		return nil, fmt.Errorf("--secret-config: %w", err)
	}
	parse := func(data map[string][]byte) (map[string]string, error) {
		cfg, err := parseSecretConfig(data)
		if err != nil {
			return nil, err
		}
		return cfg.values(defaultPepper), nil
	}
	return func(ctx context.Context, d *configreload.Dispatcher) error {
		return configreload.RunWatcher(ctx, restConfig, secretRef, parse, d)
	}, nil
}
