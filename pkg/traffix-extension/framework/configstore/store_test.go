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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
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
