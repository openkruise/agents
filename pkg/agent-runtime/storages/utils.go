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
