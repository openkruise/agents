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

package configstore

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/traffic-extension/model"
)

func newTestProfile(name, namespace string, matchLabels map[string]string) *v1alpha1.SecurityProfile {
	return &v1alpha1.SecurityProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1alpha1.SecurityProfileSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: matchLabels,
			},
		},
	}
}

func TestProfileSetAndGet(t *testing.T) {
	store := NewStore()

	nn := types.NamespacedName{Name: "test-profile", Namespace: "default"}
	profile := newTestProfile("test-profile", "default", map[string]string{"app": "test"})

	store.ProfileSet(profile)

	got, ok := store.ProfileGet(nn)
	if !ok {
		t.Fatal("expected to find profile, but got not ok")
	}
	if got.Profile.Name != "test-profile" {
		t.Errorf("expected profile name 'test-profile', got %q", got.Profile.Name)
	}

	list := store.ProfileList()
	if len(list) != 1 {
		t.Fatalf("expected 1 profile in list, got %d", len(list))
	}
}

func TestProfileDelete(t *testing.T) {
	store := NewStore()

	nn := types.NamespacedName{Name: "test-profile", Namespace: "default"}
	profile := newTestProfile("test-profile", "default", map[string]string{"app": "test"})

	store.ProfileSet(profile)
	store.ProfileDelete(nn)

	_, ok := store.ProfileGet(nn)
	if ok {
		t.Error("expected profile to be deleted, but it was found")
	}

	if len(store.ProfileList()) != 0 {
		t.Errorf("expected 0 profiles after deletion, got %d", len(store.ProfileList()))
	}
}

func TestFindProfilesForLabels(t *testing.T) {
	store := NewStore()

	profile := newTestProfile("test-profile", "default", map[string]string{"app": "test"})
	store.ProfileSet(profile)

	matched := store.FindProfilesForLabels("default", map[string]string{"app": "test"})
	if len(matched) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(matched))
	}

	matched2 := store.FindProfilesForLabels("default", map[string]string{"app": "other"})
	if len(matched2) != 0 {
		t.Errorf("expected 0 profiles for non-matching labels, got %d", len(matched2))
	}

	matched3 := store.FindProfilesForLabels("other-ns", map[string]string{"app": "test"})
	if len(matched3) != 0 {
		t.Errorf("expected 0 profiles for wrong namespace, got %d", len(matched3))
	}
}

func TestFindProfilesForLabels_MultipleProfiles(t *testing.T) {
	store := NewStore()

	now := time.Now()
	profile1 := newTestProfile("beta-profile", "default", map[string]string{"app": "ai-agent"})
	profile1.CreationTimestamp = metav1.NewTime(now)
	profile2 := newTestProfile("alpha-profile", "default", map[string]string{"app": "ai-agent"})
	profile2.CreationTimestamp = metav1.NewTime(now.Add(time.Second))
	store.ProfileSet(profile1)
	store.ProfileSet(profile2)

	matched := store.FindProfilesForLabels("default", map[string]string{"app": "ai-agent"})
	if len(matched) != 2 {
		t.Fatalf("expected 2 profiles, got %d", len(matched))
	}

	// Verify ordering by creation time (earlier first)
	if matched[0].Profile.Name != "beta-profile" || matched[1].Profile.Name != "alpha-profile" {
		t.Errorf("expected profiles sorted by creation time, got [%s, %s]", matched[0].Profile.Name, matched[1].Profile.Name)
	}
}

func TestFindProfilesForLabels_TieBreakOnName(t *testing.T) {
	// When CreationTimestamps are equal (common in unit tests and inside the
	// same reconcile second in production), ordering must remain
	// deterministic — name ascending — to keep downstream rule evaluation
	// reproducible. Run the build a few times to make a regression to
	// non-stable sort visible.
	for attempt := range 10 {
		store := NewStore()
		ts := metav1.NewTime(time.Now())
		names := []string{"charlie", "alpha", "bravo", "delta"}
		for _, n := range names {
			p := newTestProfile(n, "default", map[string]string{"app": "ai-agent"})
			p.CreationTimestamp = ts
			store.ProfileSet(p)
		}

		matched := store.FindProfilesForLabels("default", map[string]string{"app": "ai-agent"})
		if len(matched) != 4 {
			t.Fatalf("attempt %d: expected 4 profiles, got %d", attempt, len(matched))
		}
		want := []string{"alpha", "bravo", "charlie", "delta"}
		for i, w := range want {
			if matched[i].Profile.Name != w {
				t.Fatalf("attempt %d: position %d: expected %q, got %q (full order: %v)",
					attempt, i, w, matched[i].Profile.Name, profileNames(matched))
			}
		}
	}
}

func profileNames(profiles []*model.SecurityProfile) []string {
	out := make([]string, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, p.Profile.Name)
	}
	return out
}

func TestProfileSet_InvalidSelectorIsSkipped(t *testing.T) {
	store := NewStore()

	bad := &v1alpha1.SecurityProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "default"},
		Spec: v1alpha1.SecurityProfileSpec{
			Selector: metav1.LabelSelector{
				// "!" is not a valid label key.
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "!",
					Operator: metav1.LabelSelectorOpExists,
				}},
			},
		},
	}
	store.ProfileSet(bad)

	if _, ok := store.ProfileGet(types.NamespacedName{Name: "bad", Namespace: "default"}); ok {
		t.Fatal("expected invalid-selector profile to be skipped on initial Set, but it was stored")
	}
	if got := store.FindProfilesForLabels("default", map[string]string{"app": "ai-agent"}); len(got) != 0 {
		t.Fatalf("expected 0 matched profiles, got %d", len(got))
	}
}

func TestProfileSet_InvalidSelectorOnUpdateRemovesEntry(t *testing.T) {
	// Updating a previously valid profile with an invalid selector must
	// remove it rather than leaving the stale prior version live in the
	// store. This keeps the store aligned with the latest authoring intent.
	store := NewStore()
	nn := types.NamespacedName{Name: "p", Namespace: "default"}

	good := newTestProfile("p", "default", map[string]string{"app": "ai-agent"})
	store.ProfileSet(good)
	if _, ok := store.ProfileGet(nn); !ok {
		t.Fatal("precondition: expected good profile in store")
	}

	bad := &v1alpha1.SecurityProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: v1alpha1.SecurityProfileSpec{
			Selector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "!",
					Operator: metav1.LabelSelectorOpExists,
				}},
			},
		},
	}
	store.ProfileSet(bad)

	if _, ok := store.ProfileGet(nn); ok {
		t.Fatal("expected stale entry to be removed when update brings invalid selector")
	}
	if got := store.FindProfilesForLabels("default", map[string]string{"app": "ai-agent"}); len(got) != 0 {
		t.Fatalf("expected 0 matched profiles after invalid update, got %d", len(got))
	}
}

func TestProfileSet_NilIsNoop(t *testing.T) {
	store := NewStore()
	store.ProfileSet(nil)
	if len(store.ProfileList()) != 0 {
		t.Fatalf("expected nil ProfileSet to be a noop, got %d profiles", len(store.ProfileList()))
	}
}

func TestProfileDelete_NonExistentIsNoop(t *testing.T) {
	store := NewStore()
	store.ProfileDelete(types.NamespacedName{Name: "ghost", Namespace: "default"})
	if len(store.ProfileList()) != 0 {
		t.Fatalf("expected delete of missing key to be a noop, got %d profiles", len(store.ProfileList()))
	}
}

func TestFindProfilesForLabels_PartialLabelMatch(t *testing.T) {
	store := NewStore()

	profile := newTestProfile("strict-profile", "default", map[string]string{"app": "test", "env": "prod"})
	store.ProfileSet(profile)

	matched := store.FindProfilesForLabels("default", map[string]string{"app": "test", "env": "prod"})
	if len(matched) != 1 {
		t.Fatalf("expected 1 profile with both labels, got %d", len(matched))
	}

	matched2 := store.FindProfilesForLabels("default", map[string]string{"app": "test", "env": "dev"})
	if len(matched2) != 0 {
		t.Errorf("expected 0 profiles with partial labels, got %d", len(matched2))
	}

	matched3 := store.FindProfilesForLabels("default", map[string]string{"app": "test", "env": "prod", "extra": "label"})
	if len(matched3) != 1 {
		t.Errorf("expected 1 profile with extra pod labels, got %d", len(matched3))
	}
}

// TestProfileBatchApply_AddUpdateDelete exercises the informer-driven write
// path directly so the debounced batch applier reports coverage without
// requiring a fake controller-runtime cache.
func TestProfileBatchApply_AddUpdateDelete(t *testing.T) {
	s := NewStore()
	log := logr.Discard()

	add := newTestProfile("p", "default", map[string]string{"app": "x"})
	addKey := types.NamespacedName{Name: "p", Namespace: "default"}
	s.profileBatchApply(log, map[types.NamespacedName]profileEvent{
		addKey: {key: addKey, profile: add},
	})
	if got := len(s.ProfileList()); got != 1 {
		t.Fatalf("after add: expected 1 profile, got %d", got)
	}

	// Update with same key swaps labels.
	updated := newTestProfile("p", "default", map[string]string{"app": "y"})
	s.profileBatchApply(log, map[types.NamespacedName]profileEvent{
		addKey: {key: addKey, profile: updated},
	})
	if matched := s.FindProfilesForLabels("default", map[string]string{"app": "y"}); len(matched) != 1 {
		t.Fatalf("expected updated profile to match app=y, got %d", len(matched))
	}

	// Invalid selector is skipped (without panicking) and prior version
	// remains untouched.
	bad := &v1alpha1.SecurityProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: v1alpha1.SecurityProfileSpec{
			Selector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: "!", Operator: metav1.LabelSelectorOpExists,
			}}},
		},
	}
	s.profileBatchApply(log, map[types.NamespacedName]profileEvent{
		addKey: {key: addKey, profile: bad},
	})
	if matched := s.FindProfilesForLabels("default", map[string]string{"app": "y"}); len(matched) != 1 {
		t.Fatalf("invalid selector should leave the previous good entry in place")
	}

	// Delete event drops the key.
	s.profileBatchApply(log, map[types.NamespacedName]profileEvent{
		addKey: {key: addKey, deleted: true},
	})
	if got := len(s.ProfileList()); got != 0 {
		t.Fatalf("after delete: expected 0 profiles, got %d", got)
	}
}

// TestRunDebounceLoop_FlushesOnContextCancel covers the debounce loop's
// ctx.Done path. We wait until the event has been pulled into the pending
// map (signalled by the channel draining to length zero) before cancelling
// so the test asserts the deterministic "ctx cancel flushes pending" path
// rather than racing on which select arm fires first.
func TestRunDebounceLoop_FlushesOnContextCancel(t *testing.T) {
	s := NewStore()
	ctx, cancel := context.WithCancel(context.Background())

	eventCh := make(chan profileEvent, 1)
	key := types.NamespacedName{Name: "p", Namespace: "default"}
	eventCh <- profileEvent{key: key, profile: newTestProfile("p", "default", map[string]string{"app": "x"})}

	done := make(chan struct{})
	go func() {
		s.runDebounceLoop(ctx, logr.Discard(), eventCh)
		close(done)
	}()

	// Wait until the loop has consumed the channel (the event is now in
	// the pending map) so the subsequent ctx cancel deterministically
	// hits the flush-and-return arm.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(eventCh) > 0 {
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("debounce loop did not exit after ctx cancel")
	}
	if _, ok := s.ProfileGet(key); !ok {
		t.Errorf("expected pending profile to be flushed before exit")
	}
}

// TestRunDebounceLoop_FlushesOnDebounceTimer drives the timer branch by
// feeding an event and waiting slightly longer than the debounce interval
// so the timer (not ctx cancel) triggers the flush.
func TestRunDebounceLoop_FlushesOnDebounceTimer(t *testing.T) {
	prev := debounceInterval
	debounceInterval = 5 * time.Millisecond
	defer func() { debounceInterval = prev }()

	s := NewStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	eventCh := make(chan profileEvent, 1)
	key := types.NamespacedName{Name: "p", Namespace: "default"}
	eventCh <- profileEvent{key: key, profile: newTestProfile("p", "default", map[string]string{"app": "x"})}

	go s.runDebounceLoop(ctx, logr.Discard(), eventCh)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, ok := s.ProfileGet(key); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("debounce timer never flushed the pending event")
}
