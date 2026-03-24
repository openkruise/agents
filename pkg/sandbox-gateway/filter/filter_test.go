package filter

import (
	"testing"

	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
)

func TestDecodeHeadersMissingSandboxID(t *testing.T) {
	// When sandbox-id header is missing, the filter should return Continue (pass-through).
	// We can't easily test the full Envoy filter interface without the Envoy runtime,
	// but we can test the registry logic that the filter depends on.
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--app1", proxy.Route{IP: "10.0.0.1", ResourceVersion: "1"})

	route, ok := r.Get("default--app1")
	if !ok || route.IP != "10.0.0.1" {
		t.Fatalf("expected 10.0.0.1, got %q", route.IP)
	}

	// Missing key returns not found
	_, ok = r.Get("default--nonexistent")
	if ok {
		t.Fatal("expected not found for missing sandbox")
	}
}
