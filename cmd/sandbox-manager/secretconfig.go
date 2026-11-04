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
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/configreload"
	"github.com/openkruise/agents/pkg/utils"
)

// secretConfig holds normalized flag/env or Secret inputs. Business validation
// belongs to each component's Reload, after applying the pepper default.
type secretConfig struct {
	E2BAdminKey      string
	E2BKeyStorageDSN string
	E2BKeyHashPepper string
	RedisUsername    string
	RedisPassword    string
}

// parseSecretConfig requires the four non-pepper keys and returns effective inputs.
// Errors mention only key names, never their values. The admin key is used
// verbatim (matching the flag semantics); the other four are whitespace-trimmed
// (matching the env-var semantics) so an accidental trailing newline in Secret
// data does not leak in.
func parseSecretConfig(data map[string][]byte) (secretConfig, error) {
	for _, key := range []string{
		E2BAdminKeySecretKey,
		E2BKeyStorageDSNSecretKey,
		QuotaRedisUsernameSecretKey,
		QuotaRedisPasswordSecretKey,
	} {
		if _, ok := data[key]; !ok {
			return secretConfig{}, fmt.Errorf("missing required key %q", key)
		}
	}
	return secretConfig{
		E2BAdminKey:      string(data[E2BAdminKeySecretKey]),
		E2BKeyStorageDSN: strings.TrimSpace(string(data[E2BKeyStorageDSNSecretKey])),
		E2BKeyHashPepper: strings.TrimSpace(string(data[E2BKeyHashPepperSecretKey])),
		RedisUsername:    strings.TrimSpace(string(data[QuotaRedisUsernameSecretKey])),
		RedisPassword:    strings.TrimSpace(string(data[QuotaRedisPasswordSecretKey])),
	}, nil
}

func (s secretConfig) values(defaultPepper string) map[string]string {
	pepper := s.E2BKeyHashPepper
	if pepper == "" {
		pepper = defaultPepper
	}
	return map[string]string{configreload.AdminKey: s.E2BAdminKey, configreload.DSN: s.E2BKeyStorageDSN, configreload.Pepper: pepper, configreload.RedisUsername: s.RedisUsername, configreload.RedisPassword: s.RedisPassword}
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
		for ctx.Err() == nil {
			watcher, err := configreload.NewWatcher(ctx, restConfig, secretRef, parse, d)
			if err == nil {
				err = watcher.Start(ctx)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Initial list/watch outages are recoverable, including cache sync timeout.
			klog.FromContext(ctx).Info("configuration observer reconnecting", "secret", secretRef)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
		return ctx.Err()
	}, nil
}
