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
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
)

// recordingCache records IndexField calls; any other ctrlcache.Cache method
// falls through to the embedded nil interface and panics, which is the
// desired loud-failure behavior for paths the tests should not reach.
type recordingCache struct {
	ctrlcache.Cache
	fields []string
}

func (r *recordingCache) IndexField(_ context.Context, _ client.Object, field string, _ client.IndexerFunc) error {
	r.fields = append(r.fields, field)
	return nil
}

func containsField(fields []string, field string) bool {
	for _, f := range fields {
		if f == field {
			return true
		}
	}
	return false
}

func TestGetIndexFuncs_OptionalGVK(t *testing.T) {
	for _, idx := range GetIndexFuncs() {
		_, isTrafficPolicy := idx.Obj.(*agentsv1alpha1.TrafficPolicy)
		wantGVK := schema.GroupVersionKind{}
		if isTrafficPolicy {
			wantGVK = trafficPolicyKind
		}
		if idx.OptionalGVK != wantGVK {
			t.Errorf("index %q OptionalGVK = %v, want %v", idx.FieldName, idx.OptionalGVK, wantGVK)
		}
	}
}

func TestAddIndexesToCache(t *testing.T) {
	allFields := make([]string, 0)
	for _, idx := range GetIndexFuncs() {
		allFields = append(allFields, idx.FieldName)
	}
	withoutTrafficPolicy := make([]string, 0, len(allFields)-1)
	for _, f := range allFields {
		if f != IndexTrafficPolicySandboxID {
			withoutTrafficPolicy = append(withoutTrafficPolicy, f)
		}
	}

	tests := []struct {
		name       string
		absent     bool
		wantFields []string
	}{
		{
			name:       "TrafficPolicy CRD present registers every index",
			absent:     false,
			wantFields: allFields,
		},
		{
			name:       "TrafficPolicy CRD absent skips its index",
			absent:     true,
			wantFields: withoutTrafficPolicy,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := discoverGVK
			discoverGVK = func(gvk schema.GroupVersionKind) bool {
				return !(tt.absent && gvk == trafficPolicyKind)
			}
			t.Cleanup(func() { discoverGVK = old })

			c := &recordingCache{}
			if err := AddIndexesToCache(c); err != nil {
				t.Fatalf("AddIndexesToCache() error = %v", err)
			}
			if len(c.fields) != len(tt.wantFields) {
				t.Fatalf("registered fields = %v, want %v", c.fields, tt.wantFields)
			}
			for _, want := range tt.wantFields {
				if !containsField(c.fields, want) {
					t.Errorf("registered fields = %v, missing %q", c.fields, want)
				}
			}
		})
	}
}

func TestAddIndexesToCache_NilCache(t *testing.T) {
	if err := AddIndexesToCache(nil); err != nil {
		t.Errorf("AddIndexesToCache(nil) error = %v, want nil", err)
	}
}

func TestBuildCacheConfig_SkipsTrafficPolicyWhenCRDAbsent(t *testing.T) {
	tests := []struct {
		name              string
		absent            bool
		wantTrafficPolicy bool
	}{
		{
			name:              "TrafficPolicy CRD present keeps informer config",
			absent:            false,
			wantTrafficPolicy: true,
		},
		{
			name:              "TrafficPolicy CRD absent skips informer config",
			absent:            true,
			wantTrafficPolicy: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := discoverGVK
			discoverGVK = func(gvk schema.GroupVersionKind) bool {
				return !(tt.absent && gvk == trafficPolicyKind)
			}
			t.Cleanup(func() { discoverGVK = old })

			byObject, err := BuildCacheConfig(config.SandboxManagerOptions{})
			if err != nil {
				t.Fatalf("BuildCacheConfig() error = %v", err)
			}
			var ok bool
			for obj := range byObject {
				if _, isTrafficPolicy := obj.(*agentsv1alpha1.TrafficPolicy); isTrafficPolicy {
					ok = true
					break
				}
			}
			if ok != tt.wantTrafficPolicy {
				t.Errorf("TrafficPolicy in byObject = %v, want %v", ok, tt.wantTrafficPolicy)
			}
		})
	}
}
