package keys

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNewKeyStorage(t *testing.T) {
	t.Run("default secret mode requires client", func(t *testing.T) {
		_, err := NewKeyStorage(Config{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "requires a Kubernetes client")
	})

	t.Run("secret mode success", func(t *testing.T) {
		got, err := NewKeyStorage(Config{
			Mode:      StorageModeSecret,
			Namespace: "default",
			AdminKey:  "admin",
			K8sClient: fake.NewClientset(),
		})
		require.NoError(t, err)
		require.IsType(t, &secretKeyStorage{}, got)
	})

	t.Run("mysql mode requires dsn", func(t *testing.T) {
		_, err := NewKeyStorage(Config{Mode: StorageModeMySQL})
		require.Error(t, err)
		require.Contains(t, err.Error(), "requires a DSN")
	})

	t.Run("mysql mode success", func(t *testing.T) {
		got, err := NewKeyStorage(Config{
			Mode:     StorageModeMySQL,
			DSN:      "user:pass@tcp(localhost:3306)/db",
			AdminKey: "admin",
			Pepper:   "pepper",
		})
		require.NoError(t, err)
		require.IsType(t, &mysqlKeyStorage{}, got)
	})

	t.Run("unknown mode", func(t *testing.T) {
		_, err := NewKeyStorage(Config{
			Mode:      StorageMode("unknown"),
			K8sClient: fake.NewClientset(),
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown key storage mode")
	})
}
