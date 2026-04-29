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

package registry

import (
	"strconv"
	"sync"
	"testing"

	"github.com/openkruise/agents/pkg/proxy"
)

func TestGetUpdateDelete(t *testing.T) {
	r := GetRegistry()
	defer r.Clear()

	// Get missing key
	if _, ok := r.Get("default--app1"); ok {
		t.Fatal("expected not found for missing key")
	}

	// Update with version and Get
	r.Update("default--app1", proxy.Route{IP: "10.0.0.1", ResourceVersion: "1000"})
	route, ok := r.Get("default--app1")
	if !ok || route.IP != "10.0.0.1" {
		t.Fatalf("expected 10.0.0.1, got %q (ok=%v)", route.IP, ok)
	}

	// Get should return resourceVersion
	if route.ResourceVersion != "1000" {
		t.Fatalf("expected resourceVersion 1000, got %q", route.ResourceVersion)
	}

	// Overwrite with newer version
	r.Update("default--app1", proxy.Route{IP: "10.0.0.2", ResourceVersion: "1001"})
	route, ok = r.Get("default--app1")
	if !ok || route.IP != "10.0.0.2" {
		t.Fatalf("expected 10.0.0.2, got %q", route.IP)
	}

	// Update with older version should be skipped
	if r.Update("default--app1", proxy.Route{IP: "10.0.0.3", ResourceVersion: "999"}) {
		t.Fatal("expected update with older version to be skipped")
	}
	route, ok = r.Get("default--app1")
	if !ok || route.IP != "10.0.0.2" {
		t.Fatalf("expected 10.0.0.2 after skipped update, got %q", route.IP)
	}

	// Delete
	r.Delete("default--app1")
	if _, ok := r.Get("default--app1"); ok {
		t.Fatal("expected not found after delete")
	}

	// Delete non-existent key should not panic
	r.Delete("nonexistent--key")
}

func TestUpdate(t *testing.T) {
	r := GetRegistry()
	defer r.Clear()

	// First write should succeed
	if !r.Update("ns--app", proxy.Route{IP: "10.0.0.1", ResourceVersion: "100"}) {
		t.Fatal("expected first write to succeed")
	}

	// Same version should succeed (>= check)
	if !r.Update("ns--app", proxy.Route{IP: "10.0.0.2", ResourceVersion: "100"}) {
		t.Fatal("expected update with same version to succeed")
	}

	// Newer version should succeed
	if !r.Update("ns--app", proxy.Route{IP: "10.0.0.3", ResourceVersion: "101"}) {
		t.Fatal("expected update with newer version to succeed")
	}

	// Older version should be skipped
	if r.Update("ns--app", proxy.Route{IP: "10.0.0.4", ResourceVersion: "99"}) {
		t.Fatal("expected update with older version to be skipped")
	}

	// Verify final value
	route, ok := r.Get("ns--app")
	if !ok || route.IP != "10.0.0.3" {
		t.Fatalf("expected 10.0.0.3, got %q", route.IP)
	}
}

func TestList(t *testing.T) {
	r := GetRegistry()
	defer r.Clear()

	// Add routes
	r.Update("ns1--app1", proxy.Route{IP: "10.0.0.1", ResourceVersion: "100"})
	r.Update("ns2--app2", proxy.Route{IP: "10.0.0.2", ResourceVersion: "200"})

	// List should return all routes
	list := r.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(list))
	}
	if route, ok := list["ns1--app1"]; !ok || route.IP != "10.0.0.1" {
		t.Fatal("expected ns1--app1 with IP 10.0.0.1")
	}
	if route, ok := list["ns2--app2"]; !ok || route.IP != "10.0.0.2" {
		t.Fatal("expected ns2--app2 with IP 10.0.0.2")
	}
}

func TestConcurrentAccess(t *testing.T) {
	r := GetRegistry()
	defer r.Clear()

	var wg sync.WaitGroup
	// Concurrent writers with versioned updates
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "ns--app"
			r.Update(key, proxy.Route{IP: "10.0.0.1", ResourceVersion: strconv.Itoa(i)})
			r.Get(key)
			r.Delete(key)
		}(i)
	}
	wg.Wait()
}
