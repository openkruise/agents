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

// Package configstore provides an in-memory store for SecurityProfiles.
// Profile matching is performed dynamically at request time using pod labels
// extracted from Envoy filter_state, rather than maintaining a pre-computed
// pod-to-profile index.
//
// The store uses a copy-on-write strategy with atomic.Pointer for lock-free
// reads. Writes (from reconciler) are rare and protected by a mutex.
package configstore

import (
	"sort"
	"sync"
	"sync/atomic"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/traffix-extension/model"
)

// Store is a thread-safe in-memory store for SecurityProfiles.
// It maintains a simple profile index and performs dynamic label-based
// matching at request time via FindProfilesForLabels.
type Store interface {
	// ProfileSet adds or updates a profile in the store.
	ProfileSet(profile *v1alpha1.SecurityProfile)

	// ProfileGet retrieves a profile by its NamespacedName.
	ProfileGet(namespacedName types.NamespacedName) (*model.SecurityProfile, bool)

	// ProfileDelete removes a profile from the store.
	ProfileDelete(namespacedName types.NamespacedName)

	// ProfileList returns all profiles.
	ProfileList() []*model.SecurityProfile

	// FindProfilesForLabels dynamically matches profiles against the given labels.
	// Only profiles in the same namespace as the podLabels namespace are considered.
	// Returns profiles sorted by creation time (earlier first).
	FindProfilesForLabels(podNamespace string, podLabels map[string]string) []*model.SecurityProfile
}

// profileSnapshot is an immutable point-in-time view of all profiles.
// It is replaced atomically on every write operation (copy-on-write).
type profileSnapshot struct {
	byKey       map[types.NamespacedName]*model.SecurityProfile
	byNamespace map[string][]*model.SecurityProfile
}

func newEmptySnapshot() *profileSnapshot {
	return &profileSnapshot{
		byKey:       make(map[types.NamespacedName]*model.SecurityProfile),
		byNamespace: make(map[string][]*model.SecurityProfile),
	}
}

// NewStore creates a new in-memory configuration store.
func NewStore() Store {
	s := &configStore{}
	s.snapshot.Store(newEmptySnapshot())
	return s
}

type configStore struct {
	snapshot atomic.Pointer[profileSnapshot]
	mu       sync.Mutex // protects write path only
}

// --- Read path (lock-free) ---

func (s *configStore) ProfileGet(namespacedName types.NamespacedName) (*model.SecurityProfile, bool) {
	snap := s.snapshot.Load()
	p, ok := snap.byKey[namespacedName]
	return p, ok
}

func (s *configStore) ProfileList() []*model.SecurityProfile {
	snap := s.snapshot.Load()
	result := make([]*model.SecurityProfile, 0, len(snap.byKey))
	for _, p := range snap.byKey {
		result = append(result, p)
	}
	return result
}

func (s *configStore) FindProfilesForLabels(podNamespace string, podLabels map[string]string) []*model.SecurityProfile {
	snap := s.snapshot.Load()

	nsProfiles := snap.byNamespace[podNamespace]
	if len(nsProfiles) == 0 {
		return nil
	}

	var matched []*model.SecurityProfile
	for _, sp := range nsProfiles {
		if sp.Selector.Matches(labels.Set(podLabels)) {
			matched = append(matched, sp)
		}
	}

	return matched
}

// --- Write path (copy-on-write, mutex-protected) ---

func (s *configStore) ProfileSet(profile *v1alpha1.SecurityProfile) {
	if profile == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	old := s.snapshot.Load()
	nn := types.NamespacedName{Name: profile.Name, Namespace: profile.Namespace}

	selector, err := metav1.LabelSelectorAsSelector(&profile.Spec.Selector)
	if err != nil {
		// An invalid selector cannot match any pod. Drop any prior version
		// so the store reflects the latest authoring intent rather than
		// silently serving a stale spec the user has since edited.
		ctrllog.Log.WithName("configstore").Error(err,
			"SecurityProfile has invalid selector; removing from store",
			"profile", nn.String())
		if _, existed := old.byKey[nn]; !existed {
			return
		}
		newByKey := make(map[types.NamespacedName]*model.SecurityProfile, len(old.byKey))
		for k, v := range old.byKey {
			if k != nn {
				newByKey[k] = v
			}
		}
		s.snapshot.Store(buildSnapshot(newByKey))
		return
	}

	newByKey := make(map[types.NamespacedName]*model.SecurityProfile, len(old.byKey)+1)
	for k, v := range old.byKey {
		newByKey[k] = v
	}
	newByKey[nn] = &model.SecurityProfile{
		Profile:  profile,
		Selector: selector,
	}

	s.snapshot.Store(buildSnapshot(newByKey))
}

func (s *configStore) ProfileDelete(namespacedName types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()

	old := s.snapshot.Load()
	if _, exists := old.byKey[namespacedName]; !exists {
		return
	}

	newByKey := make(map[types.NamespacedName]*model.SecurityProfile, len(old.byKey))
	for k, v := range old.byKey {
		if k != namespacedName {
			newByKey[k] = v
		}
	}

	s.snapshot.Store(buildSnapshot(newByKey))
}

// buildSnapshot constructs a complete profileSnapshot from a byKey map,
// including the pre-computed byNamespace index sorted by creation time.
func buildSnapshot(byKey map[types.NamespacedName]*model.SecurityProfile) *profileSnapshot {
	byNamespace := make(map[string][]*model.SecurityProfile)

	for nn, sp := range byKey {
		byNamespace[nn.Namespace] = append(byNamespace[nn.Namespace], sp)
	}

	for _, profiles := range byNamespace {
		sort.Slice(profiles, func(i, j int) bool {
			ci := profiles[i].Profile.CreationTimestamp
			cj := profiles[j].Profile.CreationTimestamp
			if !ci.Equal(&cj) {
				return ci.Before(&cj)
			}
			// Tie-break on name so ordering stays deterministic when
			// CreationTimestamps collide (common in tests and within the
			// same reconcile second in production).
			return profiles[i].Profile.Name < profiles[j].Profile.Name
		})
	}
	return &profileSnapshot{
		byKey:       byKey,
		byNamespace: byNamespace,
	}
}
