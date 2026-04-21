package keys

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNewKeyStorage(t *testing.T) {
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
			errContains: "requires a Kubernetes client",
		},
		{
			name: "secret mode success",
			config: Config{
				Mode:      StorageModeSecret,
				Namespace: "default",
				AdminKey:  "admin",
				K8sClient: fake.NewClientset(),
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
				Mode:      StorageMode("unknown"),
				K8sClient: fake.NewClientset(),
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
