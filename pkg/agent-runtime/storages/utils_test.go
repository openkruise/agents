package storages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

func TestIsPureReadOnly(t *testing.T) {
	tests := []struct {
		name        string
		accessModes []corev1.PersistentVolumeAccessMode
		expected    bool
	}{
		{
			name:        "empty access modes",
			accessModes: []corev1.PersistentVolumeAccessMode{},
			expected:    false,
		},
		{
			name:        "read only many",
			accessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany},
			expected:    true,
		},
		{
			name:        "read write once",
			accessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			expected:    false,
		},
		{
			name:        "read write many",
			accessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			expected:    false,
		},
		{
			name:        "read write once pod",
			accessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod},
			expected:    false,
		},
		{
			name:        "mixed access modes with read write",
			accessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany, corev1.ReadWriteOnce},
			expected:    false,
		},
		{
			name:        "multiple read only many",
			accessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany, corev1.ReadOnlyMany},
			expected:    true,
		},
		{
			name:        "single read only many mixed with others",
			accessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany},
			expected:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsPureReadOnly(tt.accessModes)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGenerateRandomString(t *testing.T) {
	tests := []struct {
		name     string
		length   int
		expected int
	}{
		{
			name:     "zero length",
			length:   0,
			expected: 0,
		},
		{
			name:     "positive length",
			length:   5,
			expected: 5,
		},
		{
			name:     "large length",
			length:   10,
			expected: 10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := generateRandomString(tt.length)
			assert.Len(t, result, tt.expected)

			if tt.length > 0 {
				// Verify that the string contains only characters from the charset
				for _, char := range result {
					assert.Contains(t, charset, string(char))
				}
			}
		})
	}
}
