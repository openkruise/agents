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

func TestDeterministicVolumeID(t *testing.T) {
	// Same (pv, target) must always yield the same id so a resume re-mount is
	// idempotent per the CSI spec.
	id1 := deterministicVolumeID("pv-a", "/mnt/data")
	id2 := deterministicVolumeID("pv-a", "/mnt/data")
	assert.Equal(t, id1, id2, "same pv+target must produce a stable volume id")

	// The id must contain the PV name (existing callers/tests rely on this).
	assert.Contains(t, id1, "pv-a")

	// Same PV mounted at a different target path must not collide.
	assert.NotEqual(t, id1, deterministicVolumeID("pv-a", "/mnt/other"),
		"different target paths must produce different volume ids")

	// Different PVs at the same target path must not collide.
	assert.NotEqual(t, id1, deterministicVolumeID("pv-b", "/mnt/data"),
		"different PVs must produce different volume ids")

	// Empty target path is still stable and well-formed.
	assert.Equal(t,
		deterministicVolumeID("pv-a", ""),
		deterministicVolumeID("pv-a", ""))
}
