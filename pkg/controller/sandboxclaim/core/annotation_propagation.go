/*
Copyright 2025.

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

package core

import (
	"strings"
	"sync"
)

// Pod-template annotation propagation allowlist.
//
// By default, annotations declared on a SandboxClaim (claim.Spec.Annotations)
// are propagated only onto the Sandbox object itself, not onto the pod template.
// Some designated annotations, however, must also reach the pod template so the
// resulting Pod carries them.
//
// This allowlist keeps the community default empty (no claim annotation is
// propagated to the pod template) while exposing a registration hook so that
// internal/enterprise code can extend it via init() without modifying the
// propagation logic here. Both exact-key and prefix matching are supported.
var (
	propagationMu sync.RWMutex

	// podPropagatableAnnotationKeys holds exact-match annotation keys allowed to
	// propagate from a SandboxClaim onto the pod template.
	podPropagatableAnnotationKeys = map[string]struct{}{}

	// podPropagatableAnnotationPrefixes holds annotation key prefixes allowed to
	// propagate onto the pod template.
	podPropagatableAnnotationPrefixes []string
)

// RegisterPodPropagatableAnnotationKeys registers exact annotation keys that
// should be propagated from a SandboxClaim onto the pod template. It is intended
// to be called from an init() in internal/enterprise packages. Calling it
// multiple times is safe; duplicate keys are ignored.
func RegisterPodPropagatableAnnotationKeys(keys ...string) {
	propagationMu.Lock()
	defer propagationMu.Unlock()
	for _, k := range keys {
		if k == "" {
			continue
		}
		podPropagatableAnnotationKeys[k] = struct{}{}
	}
}

// RegisterPodPropagatableAnnotationPrefixes registers annotation key prefixes
// that should be propagated from a SandboxClaim onto the pod template. It is
// intended to be called from an init() in internal/enterprise packages.
func RegisterPodPropagatableAnnotationPrefixes(prefixes ...string) {
	propagationMu.Lock()
	defer propagationMu.Unlock()
	for _, p := range prefixes {
		if p == "" {
			continue
		}
		podPropagatableAnnotationPrefixes = append(podPropagatableAnnotationPrefixes, p)
	}
}

// shouldPropagateAnnotationToPod reports whether the given annotation key is
// allowed to propagate from the claim onto the pod template, matching either an
// exact registered key or a registered prefix.
func shouldPropagateAnnotationToPod(key string) bool {
	propagationMu.RLock()
	defer propagationMu.RUnlock()
	if _, ok := podPropagatableAnnotationKeys[key]; ok {
		return true
	}
	for _, p := range podPropagatableAnnotationPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// filterPodPropagatableAnnotations returns the subset of the given annotations
// whose keys are allowed to propagate onto the pod template. It returns nil when
// no annotation is eligible.
func filterPodPropagatableAnnotations(annotations map[string]string) map[string]string {
	if len(annotations) == 0 {
		return nil
	}
	var filtered map[string]string
	for k, v := range annotations {
		if !shouldPropagateAnnotationToPod(k) {
			continue
		}
		if filtered == nil {
			filtered = make(map[string]string)
		}
		filtered[k] = v
	}
	return filtered
}
