/*
Copyright 2025.

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
	"errors"
	"fmt"

	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// StorageMode selects the KeyStorage backend implementation.
type StorageMode string

const (
	StorageModeSecret StorageMode = "secret"
	StorageModeMySQL  StorageMode = "mysql"
)

// Config describes parameters for constructing a KeyStorage.
type Config struct {
	Mode               StorageMode
	Namespace          string // secret mode
	AdminKey           string // both modes
	DSN                string // mysql mode
	Pepper             string // mysql mode (HMAC pepper)
	DisableAutoMigrate bool   // mysql mode
	Client             client.Client
	APIReader          client.Reader
	Cache              ctrlcache.Cache // required for secret mode; ignored for mysql mode
}

// validate reports whether the operator-supplied fields are usable. The controller-runtime
// dependencies are injected only once the shared cache exists, so NewKeyStorage checks
// those separately.
func (cfg Config) validate() error {
	switch cfg.Mode {
	case "", StorageModeSecret:
	case StorageModeMySQL:
		if cfg.DSN == "" {
			return errors.New("mysql key storage requires a DSN")
		}
		// The pepper is the only secret component of the stored HMAC key hashes.
		if cfg.Pepper == "" {
			return errors.New("mysql key storage requires a non-empty pepper")
		}
	default:
		return fmt.Errorf("unknown key storage mode: %q, must be %q or %q", cfg.Mode, StorageModeSecret, StorageModeMySQL)
	}
	// An empty admin key would let an empty API-key header authenticate as admin.
	if cfg.AdminKey == "" {
		return errors.New("key storage requires a non-empty admin key")
	}
	return nil
}

// NewKeyStorage returns a KeyStorage implementation for the given config.
func NewKeyStorage(cfg Config) (KeyStorage, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Mode == StorageModeMySQL {
		return newMySQLKeyStorage(mysqlConfig{
			DSN:                cfg.DSN,
			AdminKey:           cfg.AdminKey,
			Pepper:             cfg.Pepper,
			DisableAutoMigrate: cfg.DisableAutoMigrate,
		}), nil
	}
	// validate accepted the mode, so only secret mode remains.
	if cfg.Client == nil || cfg.APIReader == nil || cfg.Cache == nil {
		return nil, errors.New("secret key storage requires controller-runtime client, api-reader, and cache")
	}
	return NewSecretKeyStorage(cfg.Client, cfg.APIReader, cfg.Cache, cfg.Namespace, cfg.AdminKey), nil
}
