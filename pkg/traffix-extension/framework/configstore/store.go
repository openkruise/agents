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
package configstore

import (
	"sort"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// Store is a thread-safe in-memory store for SecurityProfiles.
// It maintains a simple profile index and performs dynamic label-based
// matching at request time via FindProfilesForLabels.
type Store interface {
	// ProfileSet adds or updates a profile in the store.
	ProfileSet(profile *v1alpha1.SecurityProfile)

	// ProfileGet retrieves a profile by its NamespacedName.
	ProfileGet(namespacedName types.NamespacedName) (*v1alpha1.SecurityProfile, bool)

	// ProfileDelete removes a profile from the store.
	ProfileDelete(namespacedName types.NamespacedName)

	// ProfileList returns all profiles.
	ProfileList() []*v1alpha1.SecurityProfile

	// FindProfilesForLabels dynamically matches profiles against the given labels.
	// Only profiles in the same namespace as the podLabels namespace are considered.
	// Returns profiles sorted by name for deterministic ordering.
	FindProfilesForLabels(podNamespace string, podLabels map[string]string) []*v1alpha1.SecurityProfile
}

// NewStore creates a new in-memory configuration store.
func NewStore() Store {
	return &configStore{
		profiles: make(map[types.NamespacedName]*v1alpha1.SecurityProfile),
	}
}

type configStore struct {
	mu sync.RWMutex

	// profiles: key = NamespacedName of the SecurityProfile
	profiles map[types.NamespacedName]*v1alpha1.SecurityProfile
}

// --- Profile operations ---

func (s *configStore) ProfileSet(profile *v1alpha1.SecurityProfile) {
	if profile == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	nn := types.NamespacedName{Name: profile.Name, Namespace: profile.Namespace}
	s.profiles[nn] = profile
}

func (s *configStore) ProfileGet(namespacedName types.NamespacedName) (*v1alpha1.SecurityProfile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.profiles[namespacedName]
	return p, ok
}

func (s *configStore) ProfileDelete(namespacedName types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.profiles, namespacedName)
}

func (s *configStore) ProfileList() []*v1alpha1.SecurityProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*v1alpha1.SecurityProfile, 0, len(s.profiles))
	for _, p := range s.profiles {
		result = append(result, p)
	}
	return result
}

// --- Dynamic label matching ---

func (s *configStore) FindProfilesForLabels(podNamespace string, podLabels map[string]string) []*v1alpha1.SecurityProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var matched []*v1alpha1.SecurityProfile
	for nn, profile := range s.profiles {
		// Only consider profiles in the same namespace.
		if nn.Namespace != podNamespace {
			continue
		}

		selector, err := metav1.LabelSelectorAsSelector(&profile.Spec.Selector)
		if err != nil {
			// An invalid selector cannot match any pod; skip this profile
			// and log the failure so misconfigurations are observable.
			ctrllog.Log.WithName("configstore").Error(err,
				"SecurityProfile has invalid selector; skipping",
				"profile", nn.String())
			continue
		}
		if selector.Matches(labels.Set(podLabels)) {
			matched = append(matched, profile)
		}
	}

	// Sort by namespace+name for deterministic iteration order.
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].Namespace != matched[j].Namespace {
			return matched[i].Namespace < matched[j].Namespace
		}
		return matched[i].Name < matched[j].Name
	})

	return matched
}
