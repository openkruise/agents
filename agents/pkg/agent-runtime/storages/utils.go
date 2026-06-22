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
	"math/rand"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const charset = "abcdefghijklmnopqrstuvwxyz0123456789"

func generateRandomString(length int) string {
	rand.Seed(time.Now().UnixNano())
	b := make([]byte, length)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	return string(b)
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
