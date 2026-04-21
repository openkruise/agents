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

	"k8s.io/client-go/kubernetes"
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
	K8sClient          kubernetes.Interface
}

// NewKeyStorage returns a KeyStorage implementation for the given config.
func NewKeyStorage(cfg Config) (KeyStorage, error) {
	switch cfg.Mode {
	case "", StorageModeSecret:
		if cfg.K8sClient == nil {
			return nil, errors.New("secret key storage requires a Kubernetes client")
		}
		return NewSecretKeyStorage(cfg.K8sClient, cfg.Namespace, cfg.AdminKey), nil
	case StorageModeMySQL:
		if cfg.DSN == "" {
			return nil, errors.New("mysql key storage requires a DSN")
		}
		return newMySQLKeyStorage(mysqlConfig{
			DSN:                cfg.DSN,
			AdminKey:           cfg.AdminKey,
			Pepper:             cfg.Pepper,
			DisableAutoMigrate: cfg.DisableAutoMigrate,
		}), nil
	default:
		return nil, fmt.Errorf("unknown key storage mode: %q", cfg.Mode)
	}
}
