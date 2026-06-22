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
	"context"
	"maps"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/traffic-extension/model"
	env "github.com/openkruise/agents/pkg/traffic-extension/util/env"
	logutil "github.com/openkruise/agents/pkg/traffic-extension/util/logging"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

var (
	debounceInterval = env.Register("SECURITY_PROFILE_DEBOUNCE_TIME", 200*time.Millisecond,
		"Time to debounce profile events before applying updates").Get()
	debounceMaxInterval = env.Register("SECURITY_PROFILE_DEBOUNCE_TIME_MAX", 1*time.Second,
		"Max time to debounce profile events before applying updates").Get()
)

// Store is a thread-safe in-memory store for SecurityProfiles.
// It maintains a simple profile index and performs dynamic label-based
// matching at request time via FindProfilesForLabels.
type Store interface {
	// ProfileSet adds or updates a profile in the store.
	//
	// Production code drives the store through the informer event handlers
	// installed by RunSync, which feed profileBatchApply. ProfileSet is kept
	// for direct, synchronous insertion in tests and is NOT exercised on the
	// hot path; the unimplemented-action warnings emitted by profileBatchApply
	// are intentionally not duplicated here.
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
func NewStore() *configStore {
	s := &configStore{}
	s.snapshot.Store(newEmptySnapshot())
	return s
}

type configStore struct {
	snapshot atomic.Pointer[profileSnapshot]
	mu       sync.Mutex // protects write path only
}

type profileEvent struct {
	key     types.NamespacedName
	deleted bool
	profile *v1alpha1.SecurityProfile
}

func (s *configStore) RunSync(ctx context.Context, cache cache.Cache) error {
	log := ctrllog.Log.WithName("configstore")

	informer, err := cache.GetInformer(ctx, &v1alpha1.SecurityProfile{})
	if err != nil {
		return err
	}

	eventCh := make(chan profileEvent, 1024)

	_, err = informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			sp, ok := obj.(*v1alpha1.SecurityProfile)
			if !ok {
				return
			}
			eventCh <- profileEvent{
				key:     types.NamespacedName{Name: sp.Name, Namespace: sp.Namespace},
				profile: sp,
			}
		},
		UpdateFunc: func(_, newObj any) {
			sp, ok := newObj.(*v1alpha1.SecurityProfile)
			if !ok {
				return
			}
			eventCh <- profileEvent{
				key:     types.NamespacedName{Name: sp.Name, Namespace: sp.Namespace},
				profile: sp,
			}
		},
		DeleteFunc: func(obj any) {
			sp, ok := obj.(*v1alpha1.SecurityProfile)
			if !ok {
				tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown)
				if !ok {
					return
				}
				sp, ok = tombstone.Obj.(*v1alpha1.SecurityProfile)
				if !ok {
					return
				}
			}
			eventCh <- profileEvent{
				key:     types.NamespacedName{Name: sp.Name, Namespace: sp.Namespace},
				deleted: true,
			}
		},
	})
	if err != nil {
		return err
	}

	go s.runDebounceLoop(ctx, log, eventCh)
	return nil
}

func (s *configStore) runDebounceLoop(ctx context.Context, log logr.Logger, eventCh <-chan profileEvent) {
	debounceTimer := time.NewTimer(0)
	if !debounceTimer.Stop() {
		<-debounceTimer.C
	}
	debounceActive := false
	var maxTimer <-chan time.Time
	pending := make(map[types.NamespacedName]profileEvent)

	debounceC := func() <-chan time.Time {
		if debounceActive {
			return debounceTimer.C
		}
		return nil
	}

	flush := func() {
		if len(pending) == 0 {
			return
		}
		log.V(logutil.DEBUG).Info("Flushing profile events", "count", len(pending))
		s.profileBatchApply(log, pending)
		pending = make(map[types.NamespacedName]profileEvent)
		if debounceActive {
			debounceTimer.Stop()
			debounceActive = false
		}
		maxTimer = nil
	}

	for {
		select {
		case ev, ok := <-eventCh:
			if !ok {
				return
			}
			pending[ev.key] = ev
			if debounceActive {
				debounceTimer.Stop()
			}
			debounceTimer.Reset(debounceInterval)
			debounceActive = true
			if maxTimer == nil {
				maxTimer = time.After(debounceMaxInterval)
			}
		case <-debounceC():
			debounceActive = false
			flush()
		case <-maxTimer:
			flush()
		case <-ctx.Done():
			if debounceActive {
				debounceTimer.Stop()
			}
			flush()
			return
		}
	}
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

func (s *configStore) profileBatchApply(log logr.Logger, events map[types.NamespacedName]profileEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	old := s.snapshot.Load()
	newByKey := make(map[types.NamespacedName]*model.SecurityProfile, len(old.byKey))
	maps.Copy(newByKey, old.byKey)

	for _, ev := range events {
		if ev.deleted {
			delete(newByKey, ev.key)
		} else {
			sp, err := model.NewSecurityProfile(log, ev.profile)
			if err != nil {
				log.Error(err, "SecurityProfile has invalid selector; skipping", "profile", ev.key)
				continue
			}
			newByKey[ev.key] = sp
		}
	}

	s.snapshot.Store(buildSnapshot(newByKey))
}

func (s *configStore) ProfileSet(profile *v1alpha1.SecurityProfile) {
	if profile == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	old := s.snapshot.Load()
	nn := types.NamespacedName{Name: profile.Name, Namespace: profile.Namespace}

	log := ctrllog.Log.WithName("configstore")
	sp, err := model.NewSecurityProfile(log, profile)
	if err != nil {
		// An invalid selector cannot match any pod. Drop any prior version
		// so the store reflects the latest authoring intent rather than
		// silently serving a stale spec the user has since edited.
		log.Error(err,
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
	newByKey[nn] = sp

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
