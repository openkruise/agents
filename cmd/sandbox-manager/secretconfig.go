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

	corev1 "k8s.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/pkg/utils"
)

// secretConfig holds the five secret values loaded from --secret-config.
// It does not validate them against auth or key-storage mode; the caller overlays
// these onto the corresponding process settings. Emptiness against auth and
// key-storage mode is checked later by keys.Config.validate when key storage is constructed.
type secretConfig struct {
	AdminKey      string
	KeyStorageDSN string
	KeyHashPepper string
	RedisUsername string
	RedisPassword string
}

// secretConfigLoadTimeout bounds the single startup Get so a broken reference
// or unreachable API server fails fast instead of hanging boot.
const secretConfigLoadTimeout = 30 * time.Second

// parseSecretConfig requires all five keys to be present and returns the values.
// Errors mention only key names, never their values. The admin key is used
// verbatim (matching the flag semantics); the other four are whitespace-trimmed
// (matching the env-var semantics) so an accidental trailing newline in Secret
// data does not leak in.
func parseSecretConfig(data map[string][]byte) (secretConfig, error) {
	for _, key := range []string{
		E2BAdminKeySecretKey,
		E2BKeyStorageDSNSecretKey,
		E2BKeyHashPepperSecretKey,
		QuotaRedisUsernameSecretKey,
		QuotaRedisPasswordSecretKey,
	} {
		if _, ok := data[key]; !ok {
			return secretConfig{}, fmt.Errorf("missing required key %q", key)
		}
	}
	return secretConfig{
		AdminKey:      string(data[E2BAdminKeySecretKey]),
		KeyStorageDSN: strings.TrimSpace(string(data[E2BKeyStorageDSNSecretKey])),
		KeyHashPepper: strings.TrimSpace(string(data[E2BKeyHashPepperSecretKey])),
		RedisUsername: strings.TrimSpace(string(data[QuotaRedisUsernameSecretKey])),
		RedisPassword: strings.TrimSpace(string(data[QuotaRedisPasswordSecretKey])),
	}, nil
}

// loadSecretConfig parses ref, then performs exactly one precise Get. It never
// lists, watches, caches, or falls back to any other source. An empty ref is
// rejected here even though resolveSecretSettings filters it out before calling.
func loadSecretConfig(reader ctrlclient.Reader, ref string) (secretConfig, error) {
	ctx, cancel := context.WithTimeout(context.Background(), secretConfigLoadTimeout)
	defer cancel()
	secretRef, err := utils.ParseSecretRef(ref)
	if ref == "" {
		return secretConfig{}, fmt.Errorf("--secret-config must be in namespace/name form, got %q", ref)
	}
	if err != nil {
		return secretConfig{}, fmt.Errorf("--secret-config: %w", err)
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, secretRef, secret); err != nil {
		return secretConfig{}, fmt.Errorf("failed to get secret config %s/%s: %w", secretRef.Namespace, secretRef.Name, err)
	}
	cfg, err := parseSecretConfig(secret.Data)
	if err != nil {
		return secretConfig{}, fmt.Errorf("invalid secret config %s/%s: %w", secretRef.Namespace, secretRef.Name, err)
	}
	return cfg, nil
}
