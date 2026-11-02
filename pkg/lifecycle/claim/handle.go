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

package claim

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/pkg/utils/timeout"
)

// Handle is the narrow, backend-neutral view of a sandbox that the claim
// engine needs to stamp claim metadata onto a picked candidate. It is a subset
// of the fat infra.Sandbox interface: a concrete backend sandbox satisfies it
// implicitly, so the engine can operate on claims without depending on the
// full lifecycle surface.
type Handle interface {
	metav1.Object
	GetPodLabels() map[string]string
	SetPodLabels(map[string]string)
	GetPodAnnotations() map[string]string
	SetPodAnnotations(map[string]string)
	SetTimeout(timeout.Options)
}

// MergePodLabels merges the given labels into the handle's pod template labels.
// Existing labels with the same key are overwritten. The pod template labels
// map is initialized if necessary; creating a missing pod template remains the
// responsibility of the Handle implementation.
func MergePodLabels(h Handle, labels map[string]string) {
	if len(labels) == 0 {
		return
	}
	existing := h.GetPodLabels()
	if existing == nil {
		existing = make(map[string]string, len(labels))
	}
	for k, v := range labels {
		existing[k] = v
	}
	h.SetPodLabels(existing)
}

// MergePodAnnotations merges the given annotations into the handle's pod
// template annotations. Existing annotations with the same key are overwritten.
// The annotations map is initialized if necessary; creating a missing pod
// template remains the responsibility of the Handle implementation.
func MergePodAnnotations(h Handle, annotations map[string]string) {
	if len(annotations) == 0 {
		return
	}
	existing := h.GetPodAnnotations()
	if existing == nil {
		existing = make(map[string]string, len(annotations))
	}
	for k, v := range annotations {
		existing[k] = v
	}
	h.SetPodAnnotations(existing)
}
