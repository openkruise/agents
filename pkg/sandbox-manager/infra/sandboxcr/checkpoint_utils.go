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

package sandboxcr

import (
	"strings"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
)

// necessaryAnnotationKeys defines the exact annotation keys that need to be
// preserved when converting between Sandbox and Checkpoint.
var necessaryAnnotationKeys = []string{
	v1alpha1.AnnotationCSIVolumeConfig,
}

// necessaryAnnotationKeyPrefixes defines annotation key prefixes whose matching
// keys need to be preserved when converting between Sandbox and Checkpoint.
// All security-related annotations (identity provider integration) share the
// SecurityMetadataPrefix and must travel together with the checkpoint.
var necessaryAnnotationKeyPrefixes = []string{
	identity.SecurityMetadataPrefix,
}

// shouldPreserveAnnotation reports whether the given annotation key should be
// preserved, either because it exactly matches one of necessaryAnnotationKeys
// or because it carries one of necessaryAnnotationKeyPrefixes.
func shouldPreserveAnnotation(key string) bool {
	for _, k := range necessaryAnnotationKeys {
		if key == k {
			return true
		}
	}
	for _, prefix := range necessaryAnnotationKeyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// copyPreservedAnnotations copies every preserved annotation with a non-empty
// value from src into dst, allocating dst when it is nil, and returns dst.
func copyPreservedAnnotations(src, dst map[string]string) map[string]string {
	if dst == nil {
		dst = make(map[string]string, len(necessaryAnnotationKeys))
	}
	for key, val := range src {
		if val != "" && shouldPreserveAnnotation(key) {
			dst[key] = val
		}
	}
	return dst
}

func PropagateAnnotationsToCheckpoint(sbx *v1alpha1.Sandbox, cp *v1alpha1.Checkpoint) {
	sbxAnnotations := sbx.GetAnnotations()
	if sbxAnnotations == nil {
		return
	}
	cp.SetAnnotations(copyPreservedAnnotations(sbxAnnotations, cp.GetAnnotations()))
}

// RestoreAnnotationsFromCheckpoint copies necessary annotations from a Checkpoint back to a Sandbox.
// This is the reverse of propagateAnnotationsToCheckpoint, used during clone to restore
// annotations that were previously propagated from the original sandbox to the checkpoint.
func RestoreAnnotationsFromCheckpoint(cp *v1alpha1.Checkpoint, sbx *v1alpha1.Sandbox) {
	cpAnnotations := cp.GetAnnotations()
	if cpAnnotations == nil {
		return
	}
	sbx.SetAnnotations(copyPreservedAnnotations(cpAnnotations, sbx.GetAnnotations()))
}
