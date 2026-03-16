package filter

import (
	"testing"

	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
)

func TestDecodeHeadersMissingSandboxID(t *testing.T) {
	// When sandbox-id header is missing, the filter should return Continue (pass-through).
	// We can't easily test the full Envoy filter interface without the Envoy runtime,
	// but we can test the registry logic that the filter depends on.
	registry.Update("default--app1", "10.0.0.1")
	defer registry.Delete("default--app1")

	ip, ok := registry.Get("default--app1")
	if !ok || ip != "10.0.0.1" {
		t.Fatalf("expected 10.0.0.1, got %q", ip)
	}

	// Missing key returns not found
	_, ok = registry.Get("default--nonexistent")
	if ok {
		t.Fatal("expected not found for missing sandbox")
	}
}
