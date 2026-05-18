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
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// deterministicVolumeID returns a stable CSI volume id for a (PV, target path)
// pair. The CSI spec requires NodePublishVolume to be idempotent keyed on
// (volume_id, target_path): a previously random id meant every resume re-mount
// looked like a brand-new publish to the driver, stacking mounts. A stable id
// lets a spec-compliant driver short-circuit an already-published volume.
// The target path is hashed (it contains '/'), keeping the id bounded while
// remaining unique per target so the same PV mounted at different paths does
// not collide.
func deterministicVolumeID(pvName, targetPath string) string {
	sum := sha256.Sum256([]byte(targetPath))
	return fmt.Sprintf("%s-%s", pvName, hex.EncodeToString(sum[:])[:12])
}

func IsPureReadOnly(accessModes []corev1.PersistentVolumeAccessMode) bool {
	for _, mode := range accessModes {
		if mode == corev1.ReadWriteOnce || mode == corev1.ReadWriteMany || mode == corev1.ReadWriteOncePod {
			return false
		}
	}
	for _, mode := range accessModes {
		if mode == corev1.ReadOnlyMany {
			return true
		}
	}
	return false
}
