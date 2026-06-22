/*
Copyright 2025 The Kruise Authors.

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

package identity

import (
	"sync"

	"k8s.io/klog/v2"
)

// caBundleRegistry stores all registered CABundleSpec entries.
//
// Community baseline registers the gateway CA spec via init() in ca_cert_injector.go.
// Enterprise deployments may register additional specs via init() in inner_*.go files.
//
// All accesses MUST go through the package-level functions below to ensure
// thread-safety. Direct access is intentionally not exposed.
var caBundleRegistry = struct {
	sync.RWMutex
	specs []CABundleSpec
}{}

// RegisterCABundleSpec registers a CABundleSpec into the global registry.
// If a spec with the same Name already exists, the previous entry is replaced
// in-place and an info-level message is logged so the override is observable
// in startup logs. This is the primary extension point for enterprise builds:
// the inner package re-registers the gateway spec with the same Name to swap
// in its own ContainerSelector / EnvVars / Secret references in one shot,
// without touching community code.
func RegisterCABundleSpec(spec CABundleSpec) {
	caBundleRegistry.Lock()
	defer caBundleRegistry.Unlock()

	for i := range caBundleRegistry.specs {
		if caBundleRegistry.specs[i].Name == spec.Name {
			klog.InfoS("CABundleSpec overridden",
				"name", spec.Name,
				"oldSecretName", caBundleRegistry.specs[i].SecretName,
				"newSecretName", spec.SecretName)
			caBundleRegistry.specs[i] = spec
			return
		}
	}
	caBundleRegistry.specs = append(caBundleRegistry.specs, spec)
}

// ListCABundleSpecs returns a snapshot of all registered CABundleSpec entries.
// The returned slice is a copy; callers may iterate freely without holding any lock.
func ListCABundleSpecs() []CABundleSpec {
	caBundleRegistry.RLock()
	defer caBundleRegistry.RUnlock()

	out := make([]CABundleSpec, len(caBundleRegistry.specs))
	copy(out, caBundleRegistry.specs)
	return out
}

// BindCAEnabledFor sets the EnabledFor predicate of the CABundleSpec identified
// by name. It is intended to be called once during controller startup to inject
// runtime-specific gating logic (e.g. sidecarutils.IsRuntimeEnabled) without
// introducing a reverse dependency from the identity package.
//
// If no spec with the given name is registered, this call is a no-op and an
// info-level message is logged so that misconfigurations are visible.
func BindCAEnabledFor(name string, fn SandboxPredicate) {
	caBundleRegistry.Lock()
	defer caBundleRegistry.Unlock()

	for i := range caBundleRegistry.specs {
		if caBundleRegistry.specs[i].Name == name {
			caBundleRegistry.specs[i].EnabledFor = fn
			klog.V(5).InfoS("CABundleSpec EnabledFor bound", "name", name)
			return
		}
	}
	klog.InfoS("BindCAEnabledFor: no CABundleSpec found with the given name; binding skipped",
		"name", name)
}

// resetCABundleRegistryForTest clears all registered specs. It is intended
// solely for unit tests to provide a clean baseline. Production code MUST NOT
// call this function.
func resetCABundleRegistryForTest() {
	caBundleRegistry.Lock()
	defer caBundleRegistry.Unlock()
	caBundleRegistry.specs = nil
}
