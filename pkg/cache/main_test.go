/*
Copyright 2026 The Kruise Authors.

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

package cache

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client"
)

// TestMain initializes the generic client registry against a fake discovery
// server that reports all agents.kruise.io kinds as installed. Production
// code paths under test (BuildCacheConfig, AddIndexesToCache) use
// discovery.DiscoverGVK, which treats an uninitialized registry as "CRD
// absent"; seeding the registry keeps the default test environment in the
// "CRD installed" state. Tests that need an absent CRD stub discoverGVK.
func TestMain(m *testing.M) {
	kinds := []string{"Sandbox", "SandboxSet", "SandboxTemplate", "SandboxClaim",
		"Checkpoint", "Commit", "SandboxUpdateOps", "TrafficPolicy", "GlobalTrafficPolicy"}
	resources := make([]metav1.APIResource, 0, len(kinds))
	for _, kind := range kinds {
		resources = append(resources, metav1.APIResource{Name: kind, Kind: kind})
	}
	list := metav1.APIResourceList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "APIResourceList",
			APIVersion: "v1",
		},
		GroupVersion: v1alpha1.GroupVersion.String(),
		APIResources: resources,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&list)
	}))
	defer server.Close()

	if err := client.NewRegistry(&rest.Config{Host: server.URL}); err != nil {
		panic("failed to initialize generic client registry for tests: " + err.Error())
	}
	os.Exit(m.Run())
}
