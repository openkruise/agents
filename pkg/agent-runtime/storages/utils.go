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
	"math/rand/v2"
	"slices"

	corev1 "k8s.io/api/core/v1"
)

const charset = "abcdefghijklmnopqrstuvwxyz0123456789"

func generateRandomString(length int) string {
	// math/rand/v2's global source is seeded automatically and is safe
	// for concurrent use; the deprecated rand.Seed re-seed per call
	// could produce identical sequences when calls land in the same
	// nanosecond.
	b := make([]byte, length)
	for i := range b {
		b[i] = charset[rand.IntN(len(charset))] // #nosec G404 -- non-security random for temp names
	}
	return string(b)
}

// IsPureReadOnly reports whether the volume is read-only by its access
// modes. Any writable mode (ReadWriteOnce/Many/OncePod) makes the volume
// writable; otherwise a non-empty mode list is treated as read-only so that
// an unknown future mode defaults to the conservative direction instead of
// silently weakening to writable. An empty mode list (no declaration) stays
// writable, matching the mount request's own readOnly flag as the only
// signal.
func IsPureReadOnly(accessModes []corev1.PersistentVolumeAccessMode) bool {
	if len(accessModes) == 0 {
		return false
	}
	for _, mode := range accessModes {
		switch mode {
		case corev1.ReadWriteOnce, corev1.ReadWriteMany, corev1.ReadWriteOncePod:
			return false
		case corev1.ReadOnlyMany:
			// known read-only mode
		default:
			// Unknown mode: keep the conservative read-only default.
		}
	}
	return true
}

// hasAccessMode reports whether the PV declares the given access mode.
func hasAccessMode(pv *corev1.PersistentVolume, mode corev1.PersistentVolumeAccessMode) bool {
	return slices.Contains(pv.Spec.AccessModes, mode)
}
