package sandboxutils

import (
	"fmt"
	"testing"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestGlobalCacheBehavior(t *testing.T) {
	// 1. Enable cache for this test
	skipCacheInTests = false
	defer func() { skipCacheInTests = true }() // Restore default

	// 2. Setup Test Data
	uid := types.UID("test-cache-uid")
	ns, name := "default", "cache-test-sbx"
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       ns,
			Name:            name,
			UID:             uid,
			ResourceVersion: "100",
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			// Fix: Let Go infer the type instead of guessing the struct name
			PodInfo: agentsv1alpha1.PodInfo{
				PodIP: "1.2.3.4",
			},
		},
	}
	// Add ready condition
	readyCond := metav1.Condition{
		Type:   string(agentsv1alpha1.SandboxConditionReady),
		Status: metav1.ConditionTrue,
	}
	sbx.Status.Conditions = append(sbx.Status.Conditions, readyCond)

	// 3. First Call (Cache Miss -> Compute -> Store)
	// Fix: Use '_' to ignore the unused 'reason' variable
	state1, _ := GetSandboxState(sbx)
	if state1 != agentsv1alpha1.SandboxStateRunning {
		t.Errorf("Expected Running, got %s", state1)
	}

	// 4. Verify it's in the cache
	key := types.NamespacedName{Namespace: ns, Name: name}
	cacheLock.RLock()
	item, found := sandboxStateCache[key]
	cacheLock.RUnlock()

	if !found {
		t.Fatal("Item should be in cache after first call")
	}
	if item.UID != uid {
		t.Errorf("Cached UID mismatch")
	}

	// 5. Simulate a "Second Call" (Cache Hit)
	// We change the object logic state but KEEP the same ResourceVersion.
	// If cache works, it should return the OLD state (from cache), ignoring the change.
	sbx.Status.Phase = agentsv1alpha1.SandboxFailed // Should be ignored by cache
	state2, _ := GetSandboxState(sbx)

	if state2 != agentsv1alpha1.SandboxStateRunning {
		t.Errorf("Cache failed: expected to return cached 'Running' state, but got '%s'", state2)
	}

	// 6. Test Invalidation
	DeleteSandboxStateCache(ns, name)

	cacheLock.RLock()
	_, foundAfterDelete := sandboxStateCache[key]
	cacheLock.RUnlock()

	if foundAfterDelete {
		t.Error("Cache should be empty after delete")
	}
}
func BenchmarkGetSandboxState(b *testing.B) {
	// 1. Setup Data for the benchmark
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "bench-ns",
			Name:            "bench-sbx",
			UID:             types.UID("bench-uid"),
			ResourceVersion: "100",
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
		},
	}

	// 2. Enable Cache for Benchmark
	// We temporarily turn off the "skipCacheInTests" flag so the cache actually runs
	originalSkip := skipCacheInTests
	skipCacheInTests = false
	defer func() { skipCacheInTests = originalSkip }()

	// Populate the cache once so we measure HIT performance
	GetSandboxState(sbx)

	b.ResetTimer()

	// SCENARIO A: Cache Hit (The Fast Path)
	// This measures how fast it is when the data is already in the map.
	b.Run("CacheHit", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			GetSandboxState(sbx)
		}
	})

	// SCENARIO B: Cache Miss (The Slow Path)
	// This simulates the old behavior by forcing a re-calculation every time.
	b.Run("CacheMiss", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			// Changing ResourceVersion forces the code to re-compute the state
			sbx.ResourceVersion = fmt.Sprintf("%d", i)
			GetSandboxState(sbx)
		}
	})
}
func TestCacheSkipInTests(t *testing.T) {
	// 1. Setup Data with enough detail to be considered "Running"
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "test-ns",
			Name:            "test-skip",
			UID:             types.UID("test-uid"),
			ResourceVersion: "100",
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			PodInfo: agentsv1alpha1.PodInfo{
				PodIP: "1.2.3.4", // Required for "Running" state
			},
		},
	}
	// Add Ready condition
	sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
		Type:   string(agentsv1alpha1.SandboxConditionReady),
		Status: metav1.ConditionTrue,
	})

	// 2. Ensure we are in "Default Test Mode"
	if !skipCacheInTests {
		t.Fatal("skipCacheInTests should be true by default in tests")
	}

	// 3. Call GetSandboxState
	state, _ := GetSandboxState(sbx)

	// 4. Verify Result
	if state != agentsv1alpha1.SandboxStateRunning {
		t.Errorf("Expected Running, got %s", state)
	}
}
