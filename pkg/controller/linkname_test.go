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

package controller

import (
	"strings"
	"sync"
	_ "unsafe" // required for go:linkname

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/testing"

	"github.com/openkruise/agents/client"
	// import for side-effect: ensures the linkname target package is loaded.
	_ "sigs.k8s.io/controller-runtime/pkg/controller"
)

// controllerRuntimeUsedNames is a linkname alias for the package-private
// controllerRuntimeUsedNames is the process-global usedNames set in
// sigs.k8s.io/controller-runtime/pkg/controller. The set rejects duplicate
// Named(...) registrations across all managers in the process.
//
// We access it via go:linkname because controller-runtime provides NO public
// API to reset or query this registry, yet our tests need to deterministically
// trigger the "controller name already exists" error path for each
// sub-controller registered in SetupWithManager. Without this, the only way
// to cover those error branches would be multiple separate test binaries or
// unreliable process-level isolation.
//
//go:linkname controllerRuntimeUsedNames sigs.k8s.io/controller-runtime/pkg/controller.usedNames
var controllerRuntimeUsedNames sets.Set[string]

// controllerRuntimeNameLock is a linkname alias for the corresponding
// nameLock that guards usedNames in controller-runtime.
//
//go:linkname controllerRuntimeNameLock sigs.k8s.io/controller-runtime/pkg/controller.nameLock
var controllerRuntimeNameLock sync.Mutex

// clientDefaultGenericClient is a linkname alias for the package-private
// defaultGenericClient in github.com/openkruise/agents/client. Our discovery
// helper (pkg/discovery.DiscoverGVK) calls client.GetGenericClient() and
// returns false immediately when it is nil. By injecting a GenericClientset
// backed by a FakeDiscovery here, we can flip the discovery guard to true
// without standing up a real apiserver, which in turn lets the test exercise
// each sub-controller's post-discovery registration path.
//
//go:linkname clientDefaultGenericClient github.com/openkruise/agents/client.defaultGenericClient
var clientDefaultGenericClient *client.GenericClientset

// resetControllerRuntimeUsedNames clears the global controller-name registry
// so a follow-up SetupWithManager call sees a clean slate. Only intended for
// tests; this is a deliberate end-run around controller-runtime's
// process-level uniqueness guard so multiple registration attempts can be
// exercised in a single test binary.
func resetControllerRuntimeUsedNames() {
	controllerRuntimeNameLock.Lock()
	defer controllerRuntimeNameLock.Unlock()
	controllerRuntimeUsedNames = sets.Set[string]{}
}

// seedControllerName inserts name into controller-runtime's usedNames set so
// the next attempt to register a controller with that name fails with the
// canonical "controller with name X already exists" error. This is the lever
// each error-path subtest pulls to force a specific sub-controller's
// SetupWithManager to fail on demand.
func seedControllerName(name string) {
	controllerRuntimeNameLock.Lock()
	defer controllerRuntimeNameLock.Unlock()
	if controllerRuntimeUsedNames == nil {
		controllerRuntimeUsedNames = sets.Set[string]{}
	}
	controllerRuntimeUsedNames.Insert(name)
}

// allAgentsKinds lists every Kind under agents.kruise.io/v1alpha1 that
// pkg/controller's sub-controllers consult via discovery.DiscoverGVK. The
// fake discovery client built by installFakeGenericClient reports each kind
// in this slice as discoverable, so every guarded sub-controller proceeds
// past its discovery check.
var allAgentsKinds = []string{"Sandbox", "SandboxSet", "SandboxClaim", "SandboxUpdateOps", "SandboxTemplate", "Checkpoint"}

// installFakeGenericClient stuffs a GenericClientset whose DiscoveryClient
// is a FakeDiscovery returning an APIResourceList for agents.kruise.io/v1alpha1
// containing every Kind in allAgentsKinds into the package-private
// client.defaultGenericClient slot. After this call,
// pkg/discovery.DiscoverGVK returns true for those kinds and false for any
// other Kind/group.
//
// The previous value is returned so callers can restore it on teardown,
// keeping unrelated tests in the same binary from observing a polluted
// global. A nil return value indicates no client was previously installed.
func installFakeGenericClient() *client.GenericClientset {
	prev := clientDefaultGenericClient
	resources := make([]metav1.APIResource, 0, len(allAgentsKinds))
	for _, k := range allAgentsKinds {
		resources = append(resources, metav1.APIResource{
			Name:       strings.ToLower(k) + "s",
			Kind:       k,
			Namespaced: true,
		})
	}
	list := &metav1.APIResourceList{
		GroupVersion: "agents.kruise.io/v1alpha1",
		APIResources: resources,
	}
	fake := &discoveryfake.FakeDiscovery{
		Fake: &testing.Fake{
			Resources: []*metav1.APIResourceList{list},
		},
	}
	clientDefaultGenericClient = &client.GenericClientset{DiscoveryClient: fake}
	return prev
}

// restoreGenericClient puts back whatever client was installed before
// installFakeGenericClient. Callers usually defer this immediately after the
// install call.
func restoreGenericClient(prev *client.GenericClientset) {
	clientDefaultGenericClient = prev
}
