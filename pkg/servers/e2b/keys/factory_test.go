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
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNewKeyStorage(t *testing.T) {
	fc := fake.NewFakeClient()
	tests := []struct {
		name             string
		config           Config
		wantErr          bool
		errContains      string
		wantType         interface{}
		additionalChecks func(t *testing.T, storage KeyStorage)
	}{
		{
			name:        "default secret mode requires client",
			config:      Config{},
			wantErr:     true,
			errContains: "secret key storage requires",
		},
		{
			name: "secret mode success",
			config: Config{
				Mode:      StorageModeSecret,
				Namespace: "default",
				AdminKey:  "admin",
				Client:    fc,
				APIReader: fc,
			},
			wantErr:  false,
			wantType: &secretKeyStorage{},
		},
		{
			name:        "mysql mode requires dsn",
			config:      Config{Mode: StorageModeMySQL},
			wantErr:     true,
			errContains: "requires a DSN",
		},
		{
			name: "mysql mode success",
			config: Config{
				Mode:               StorageModeMySQL,
				DSN:                "user:pass@tcp(localhost:3306)/db",
				AdminKey:           "admin",
				Pepper:             "pepper",
				DisableAutoMigrate: true,
			},
			wantErr:  false,
			wantType: &mysqlKeyStorage{},
			additionalChecks: func(t *testing.T, storage KeyStorage) {
				mysqlStorage, ok := storage.(*mysqlKeyStorage)
				require.True(t, ok, "expected *mysqlKeyStorage")
				require.True(t, mysqlStorage.cfg.DisableAutoMigrate, "DisableAutoMigrate should be true")
			},
		},
		{
			name: "unknown mode",
			config: Config{
				Mode: StorageMode("unknown"),
			},
			wantErr:     true,
			errContains: "unknown key storage mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewKeyStorage(tt.config)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					require.Contains(t, err.Error(), tt.errContains)
				}
				return
			}
			require.NoError(t, err)
			if tt.wantType != nil {
				require.IsType(t, tt.wantType, got)
			}
			if tt.additionalChecks != nil {
				tt.additionalChecks(t, got)
			}
		})
	}
}
