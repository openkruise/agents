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

package cache

import (
	"testing"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	agents "github.com/openkruise/agents/api/v1alpha1"
)

func TestServingCacheOptionalNamespaceScope(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "installed"}[installed], func(t *testing.T) {
			previous := discoverGVK
			discoverGVK = func(gvk schema.GroupVersionKind) bool { return gvk != servingKind || installed }
			t.Cleanup(func() { discoverGVK = previous })
			config, err := BuildCacheConfig(CacheScope{SandboxNamespace: "team", SandboxLabelSelector: "member=true"})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for obj, scope := range config {
				if _, ok := obj.(*agents.SandboxServing); ok {
					found = true
					if scope.Label == nil || !scope.Label.Matches(labels.Set{"member": "true"}) || scope.Label.Matches(labels.Set{"member": "false"}) {
						t.Fatal("Serving did not inherit the Sandbox label selector")
					}
					if _, ok := scope.Namespaces["team"]; !ok {
						t.Fatal("SandboxServing object lost namespace scope")
					}
				}
			}
			if found != installed {
				t.Fatal("optional Serving informer config did not follow discovery")
			}
			recorded := &recordingCache{}
			if err := AddIndexesToCache(recorded, false); err != nil {
				t.Fatal(err)
			}
			if containsField(recorded.fields, IndexServingID) != installed {
				t.Fatal("optional Serving ID index did not follow discovery")
			}
		})
	}
}

func TestServingCacheHealthGate(t *testing.T) {
	health := NewInformerHealth()
	c := &Cache{servingAvailable: true, health: health}
	if c.ServingInformerHealthy() {
		t.Fatal("Serving cache healthy before initial sync")
	}
	health.MarkSynced()
	if !c.ServingInformerHealthy() {
		t.Fatal("Serving cache not healthy after sync")
	}
	c.servingAvailable = false
	if c.ServingInformerHealthy() {
		t.Fatal("missing CRD became a healthy Serving capability")
	}
}
