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

package authorization

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
)

// DefaultDecisionCacheTTL bounds how long a cached Decision may be served
// without a confirming informer event, per §8.3's own wording: "a short time
// to live". It exists only as a backstop for an event WatchForInvalidation
// somehow missed (an informer that has not yet resynced after a watch error,
// for instance); under normal operation every change is caught by the
// informer handlers the instant it happens, well inside this window. Ten
// seconds is short enough that a missed event self-heals almost immediately,
// while still turning the overwhelmingly common case, the same identity
// repeating the same request, into a map lookup instead of a List plus one
// Get per bound role.
const DefaultDecisionCacheTTL = 10 * time.Second

// Option adjusts an Authorizer at construction time.
type Option func(*Authorizer)

// WithTTL overrides DefaultDecisionCacheTTL.
func WithTTL(ttl time.Duration) Option {
	return func(a *Authorizer) { a.ttl = ttl }
}

// withClock overrides the time source. Tests use it to move the cache past
// its TTL without sleeping.
func withClock(now func() time.Time) Option {
	return func(a *Authorizer) { a.now = now }
}

// decisionCacheKey is the cache key. Two requests that agree on all four
// fields always produce the same Decision, because evaluate's result depends
// on nothing else.
type decisionCacheKey struct {
	Namespace     string
	AgentIdentity string
	Action        string
	Resource      string
}

// decisionCacheEntry is one cached verdict together with when it stops being
// trusted absent a confirming informer event.
type decisionCacheEntry struct {
	decision Decision
	expires  time.Time
}

// lookupCache returns the cached Decision for key if one exists and has not
// expired. An expired entry is dropped rather than left for storeCache to
// overwrite, so a cache that is never hit again does not hold stale entries
// forever.
func (a *Authorizer) lookupCache(key decisionCacheKey) (Decision, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.cache[key]
	if !ok {
		return Decision{}, false
	}
	if !a.now().Before(entry.expires) {
		delete(a.cache, key)
		return Decision{}, false
	}
	return entry.decision, true
}

// storeCache records decision for key, valid for the Authorizer's TTL from
// now.
func (a *Authorizer) storeCache(key decisionCacheKey, decision Decision) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cache[key] = decisionCacheEntry{decision: decision, expires: a.now().Add(a.ttl)}
}

// WatchForInvalidation registers informer event handlers that evict cached
// decisions the instant an AgentRole or AgentRoleBinding actually changes,
// so the TTL above only has to cover an event that is somehow missed.
//
// c must be backed by the same cluster state the Authorizer's reader serves;
// watching an unrelated cache would invalidate nothing useful. Call this once
// per Authorizer, after the manager owning c has started, before serving
// requests.
func (a *Authorizer) WatchForInvalidation(ctx context.Context, c ctrlcache.Cache) error {
	if err := a.watchKind(ctx, c, &securityv1alpha1.AgentRole{}); err != nil {
		return fmt.Errorf("watch AgentRole: %w", err)
	}
	if err := a.watchKind(ctx, c, &securityv1alpha1.AgentRoleBinding{}); err != nil {
		return fmt.Errorf("watch AgentRoleBinding: %w", err)
	}
	return nil
}

// watchKind registers the eviction handler on the informer for one kind.
func (a *Authorizer) watchKind(ctx context.Context, c ctrlcache.Cache, obj client.Object) error {
	informer, err := c.GetInformer(ctx, obj)
	if err != nil {
		return fmt.Errorf("get informer for %T: %w", obj, err)
	}
	if _, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(added any) { a.evictNamespaceOf(ctx, added) },
		UpdateFunc: func(_, updated any) { a.evictNamespaceOf(ctx, updated) },
		DeleteFunc: func(deleted any) { a.evictNamespaceOf(ctx, deleted) },
	}); err != nil {
		return fmt.Errorf("add event handler for %T: %w", obj, err)
	}
	return nil
}

// evictNamespaceOf drops every cached decision for obj's namespace.
//
// Authorization never crosses namespaces (see the package doc), so a change
// to any AgentRole or AgentRoleBinding can only possibly affect cached
// decisions within that object's own namespace. Evicting the whole namespace
// rather than tracking exactly which cached entries one role name could
// affect is deliberate: that precision is not needed yet, and the eviction
// itself runs only on a role or binding change, never on the request path.
func (a *Authorizer) evictNamespaceOf(ctx context.Context, obj any) {
	namespace, ok := namespaceOf(obj)
	if !ok {
		log.FromContext(ctx).V(4).Info("Authorization cache invalidation: could not read object namespace",
			"object", fmt.Sprintf("%T", obj))
		return
	}
	a.evictNamespace(namespace)
}

// namespaceOf reads the namespace an informer event is about. obj is
// ordinarily the typed object itself; on a delete the informer did not
// observe directly it instead arrives as a toolscache.DeletedFinalStateUnknown
// tombstone wrapping the last known object, which is unwrapped the same way
// client-go's own consumers do.
func namespaceOf(obj any) (string, bool) {
	metaObj, ok := obj.(metav1.Object)
	if !ok {
		tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown)
		if !ok {
			return "", false
		}
		return namespaceOf(tombstone.Obj)
	}
	return metaObj.GetNamespace(), true
}

// evictNamespace drops every cached decision whose key names namespace.
func (a *Authorizer) evictNamespace(namespace string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key := range a.cache {
		if key.Namespace == namespace {
			delete(a.cache, key)
		}
	}
}
