package registry

import (
	"sync"
	"testing"
)

func TestGetUpdateDelete(t *testing.T) {
	// Clean state
	mu.Lock()
	entries = make(map[string]string)
	mu.Unlock()

	// Get missing key
	if _, ok := Get("default--app1"); ok {
		t.Fatal("expected not found for missing key")
	}

	// Update and Get
	Update("default--app1", "10.0.0.1")
	ip, ok := Get("default--app1")
	if !ok || ip != "10.0.0.1" {
		t.Fatalf("expected 10.0.0.1, got %q (ok=%v)", ip, ok)
	}

	// Overwrite
	Update("default--app1", "10.0.0.2")
	ip, ok = Get("default--app1")
	if !ok || ip != "10.0.0.2" {
		t.Fatalf("expected 10.0.0.2, got %q", ip)
	}

	// Delete
	Delete("default--app1")
	if _, ok := Get("default--app1"); ok {
		t.Fatal("expected not found after delete")
	}

	// Delete non-existent key should not panic
	Delete("nonexistent--key")
}

func TestConcurrentAccess(t *testing.T) {
	mu.Lock()
	entries = make(map[string]string)
	mu.Unlock()

	var wg sync.WaitGroup
	// Concurrent writers
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "ns--app"
			Update(key, "10.0.0.1")
			Get(key)
			Delete(key)
		}(i)
	}
	wg.Wait()
}
