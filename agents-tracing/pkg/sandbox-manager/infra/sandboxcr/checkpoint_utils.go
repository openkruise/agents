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
	"github.com/openkruise/agents/api/v1alpha1"
)

// necessaryAnnotationKeys defines the list of annotation keys that need to be
// preserved when converting between Sandbox and SandboxTemplate.
var necessaryAnnotationKeys = []string{
	v1alpha1.AnnotationCSIVolumeConfig,
}

func PropagateAnnotationsToCheckpoint(sbx *v1alpha1.Sandbox, cp *v1alpha1.Checkpoint) {
	sbxAnnotations := sbx.GetAnnotations()
	if sbxAnnotations == nil {
		return
	}
	cpAnnotations := cp.GetAnnotations()
	if cpAnnotations == nil {
		cpAnnotations = make(map[string]string, len(necessaryAnnotationKeys))
	}
	for _, key := range necessaryAnnotationKeys {
		if val, ok := sbxAnnotations[key]; ok && val != "" {
			cpAnnotations[key] = val
		}
	}
	cp.SetAnnotations(cpAnnotations)
}

// RestoreAnnotationsFromCheckpoint copies necessary annotations from a Checkpoint back to a Sandbox.
// This is the reverse of propagateAnnotationsToCheckpoint, used during clone to restore
// annotations that were previously propagated from the original sandbox to the checkpoint.
func RestoreAnnotationsFromCheckpoint(cp *v1alpha1.Checkpoint, sbx *v1alpha1.Sandbox) {
	cpAnnotations := cp.GetAnnotations()
	if cpAnnotations == nil {
		return
	}
	sbxAnnotations := sbx.GetAnnotations()
	if sbxAnnotations == nil {
		sbxAnnotations = make(map[string]string, len(necessaryAnnotationKeys))
	}
	for _, key := range necessaryAnnotationKeys {
		if val, ok := cpAnnotations[key]; ok && val != "" {
			sbxAnnotations[key] = val
		}
	}
	sbx.SetAnnotations(sbxAnnotations)
}
