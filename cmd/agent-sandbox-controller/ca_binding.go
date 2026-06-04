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

package main

import (
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/identity"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
	"github.com/openkruise/agents/pkg/utils/sidecarutils"
)

// caBindingFuncs holds registered CA binding callbacks. Each callback is
// expected to call identity.BindCAEnabledFor for one or more CA specs.
//
// Community baseline registers the gateway CA binding in this file's init().
// Enterprise deployments append additional bindings via init() in
// inner_ca_binding.go (or similar inner_*.go files), which only exist in the
// enterprise repository and thus never conflict with community code.
//
// All callbacks are executed during controller startup (after flag parsing) via
// executeCABindings(). This guarantees that feature-gate state is available.
var caBindingFuncs []func()

// registerCABinding appends a CA binding callback to the global list.
// It MUST be called during init() only.
func registerCABinding(fn func()) {
	caBindingFuncs = append(caBindingFuncs, fn)
}

// executeCABindings runs all registered CA binding callbacks. It should be
// called exactly once during controller startup, after feature gates and
// controller setup are complete.
func executeCABindings() {
	for _, fn := range caBindingFuncs {
		fn()
	}
}

func init() {
	// Community baseline: bind the gateway CA spec's EnabledFor predicate to
	// the traffic-proxy runtime check, gated by SecurityIdentityProviderGate.
	//
	// When the gate is off, the entire CA injection pipeline is disabled at the
	// caller side (shouldInjectCABundles), so we skip binding altogether to
	// keep startup logs clean and make the gate dependency explicit.
	registerCABinding(func() {
		if utilfeature.DefaultFeatureGate.Enabled(features.SecurityIdentityProviderGate) {
			identity.BindCAEnabledFor(identity.GatewayCABundleName, func(sbx *agentsv1alpha1.Sandbox) bool {
				return sidecarutils.IsRuntimeEnabled(sbx, agentsv1alpha1.RuntimeConfigForInjectTrafficProxy)
			})
		} else {
			setupLog.Info("SecurityIdentityProviderGate disabled, skip CABundleSpec EnabledFor binding",
				"name", identity.GatewayCABundleName)
		}
	})
}
