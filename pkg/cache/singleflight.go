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

package cache

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
)

const (
	// SingleflightAnnotationPrefix is the annotation key prefix for distributed single-flight.
	// Full key format: singleflight.agents.kruise.io/<logicalKey>
	SingleflightAnnotationPrefix = "singleflight." + agentsv1alpha1.InternalPrefix

	// SingleflightWaitKeyPrefix is used to create distinct wait hook keys that
	// do NOT collide with existing WaitForObjectSatisfied entries (which use
	// WaitHookKey = "*type/ns/name"). Format: "sf:<key>/<type>/<ns>/<name>".
	SingleflightWaitKeyPrefix = "sf:"

	// DefaultSingleflightPreemptionThreshold is the default duration
	// after which a stale Runner can be preempted. This is the single
	// source of truth; do not duplicate this constant elsewhere.
	DefaultSingleflightPreemptionThreshold = 5 * time.Minute

	// releaseLockTimeout is the timeout for the background context used during lock release.
	releaseLockTimeout = 5 * time.Minute
)

// singleflightAnnotation holds the parsed annotation value.
type singleflightAnnotation struct {
	Seq        int64
	Done       bool
	LastUpdate int64 // Unix seconds
}

// parseSingleflightAnnotation parses "<seq>:<done>:<lastUpdate>" from the annotation value.
// Returns false if the annotation is missing or malformed; caller treats missing as "0:true:0".
func parseSingleflightAnnotation(obj client.Object, key string) (singleflightAnnotation, bool) {
	annKey := SingleflightAnnotationPrefix + key
	val, ok := obj.GetAnnotations()[annKey]
	if !ok || val == "" {
		return singleflightAnnotation{}, false
	}
	parts := strings.Split(val, ":")
	if len(parts) != 3 {
		return singleflightAnnotation{}, false
	}
	seq, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return singleflightAnnotation{}, false
	}
	done := parts[1] == "true"
	lastUpdate, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return singleflightAnnotation{}, false
	}
	return singleflightAnnotation{Seq: seq, Done: done, LastUpdate: lastUpdate}, true
}

// setSingleflightAnnotation sets the annotation on the object.
func setSingleflightAnnotation(obj client.Object, key string, seq int64, done bool) {
	annKey := SingleflightAnnotationPrefix + key
	value := fmt.Sprintf("%d:%v:%d", seq, strconv.FormatBool(done), time.Now().Unix())
	anns := obj.GetAnnotations()
	if anns == nil {
		anns = make(map[string]string)
	}
	anns[annKey] = value
	obj.SetAnnotations(anns)
}

// isPreemptable returns true if a Runner with the given lastUpdate timestamp
// has exceeded the preemption threshold and can be preempted.
func isPreemptable(lastUpdate int64, threshold time.Duration) bool {
	return time.Now().Unix()-lastUpdate > int64(threshold.Seconds())
}

// DistributedSingleFlightDo executes a distributed single-flight operation.
//
// Each K8s object is a single-flight group identified by logical key.
// The protocol uses annotation singleflight.agents.kruise.io/<key> with
// value "<seq>:<done>:<lastUpdate>" to coordinate Run/Wait across replicas.
//
// Parameters:
//   - ctx: caller context; used for wait and retry loops. Lock release uses
//     a separate background context to prevent caller-cancellation leaks.
//   - cache: Provider with APIReader, client, and waitHooks.
//   - obj: the K8s object acting as the group; must exist in the API server.
//   - key: logical operation key (e.g., "pause-resume"). Pause and Resume
//     share the same key to serialize against each other.
//   - precheck: called before the Run/Wait fork and in each retry iteration.
//     Must return nil if the operation can still proceed; non-nil to abort.
//     Must NOT be called after Wait returns.
//   - modifier: applied to the object during the optimistic update that sets
//     seq:false. Combined with annotation write to reduce API calls.
//   - function: executed only by the Runner. Both success and failure must be
//     followed by lock release (done=true) via defer.
//
// Returns the latest object after operation completion.
//   - Run path: the object after the final update (modifier + function applied).
//   - Wait path: the object from informer snapshot after done==true was observed.
//
// Callers must check the returned object's business state and retry if needed.
func DistributedSingleFlightDo[T client.Object](
	ctx context.Context,
	cache Provider,
	obj T,
	key string,
	precheck func(T) error,
	modifier func(T),
	function func(T) error,
	preemptionThreshold time.Duration,
) (T, error) {
	// Phase 1: Read latest from API server (first read must bypass informer to avoid stale decisions)
	fresh := obj.DeepCopyObject().(T)
	namespacedName := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	if err := cache.GetAPIReader().Get(ctx, namespacedName, fresh); err != nil {
		return obj, fmt.Errorf("singleflight: failed to read object from API server: %w", err)
	}

	ann, hasAnn := parseSingleflightAnnotation(fresh, key)
	if !hasAnn {
		ann = singleflightAnnotation{Seq: 0, Done: true, LastUpdate: 0}
	}

	// precheck on the fresh object
	if err := precheck(fresh); err != nil {
		return fresh, err
	}

	// Phase 2: Fork — Wait or Compete
	if ann.Done || (ann.Done == false && isPreemptable(ann.LastUpdate, preemptionThreshold)) {
		return singleflightCompete(ctx, cache, fresh, key, ann, precheck, modifier, function, preemptionThreshold)
	}
	// done == false and not preemptable: a legitimate Runner is in progress
	return singleflightWait(ctx, cache, fresh, key, ann.Seq)
}

// singleflightSeqConflictError is a sentinel error used internally to break
// the retry loop when another goroutine has claimed our seq or higher.
type singleflightSeqConflictError struct {
	currentSeq int64
	mySeq      int64
}

func (e *singleflightSeqConflictError) Error() string {
	return fmt.Sprintf("singleflight seq conflict: current=%d, mine=%d", e.currentSeq, e.mySeq)
}

func singleflightCompete[T client.Object](
	ctx context.Context,
	cache Provider,
	obj T,
	key string,
	prevAnn singleflightAnnotation,
	precheck func(T) error,
	modifier func(T),
	function func(T) error,
	preemptionThreshold time.Duration,
) (T, error) {
	log := klog.FromContext(ctx).WithValues("singleflightKey", key, "object", klog.KObj(obj))
	namespacedName := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	seq := prevAnn.Seq + 1

	// claimed holds the successfully-updated object after the retry loop.
	// It must be captured inside the retry closure so function() receives
	// the post-modifier, post-annotation object — NOT the stale outer `obj`.
	var claimed T

	// Try to claim the lock with optimistic update
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Re-read from informer inside the retry loop
		fresh := obj.DeepCopyObject().(T)
		if err := cache.GetClient().Get(ctx, namespacedName, fresh); err != nil {
			return err
		}
		ann, hasAnn := parseSingleflightAnnotation(fresh, key)
		if !hasAnn {
			ann = singleflightAnnotation{Seq: 0, Done: true, LastUpdate: 0}
		}

		// precheck on every retry iteration
		if err := precheck(fresh); err != nil {
			return err
		}

		if ann.Seq >= seq {
			// Someone else claimed this seq or higher. Exit retry; let caller handle.
			// We return a sentinel error to break the retry loop.
			return &singleflightSeqConflictError{currentSeq: ann.Seq, mySeq: seq}
		}

		// Apply modifier + set annotation
		modifier(fresh)
		setSingleflightAnnotation(fresh, key, seq, false)
		if err := cache.GetClient().Update(ctx, fresh); err != nil {
			return err
		}
		claimed = fresh // capture the successfully-updated object
		return nil
	})

	if err != nil {
		if conflict, ok := err.(*singleflightSeqConflictError); ok {
			// Someone else won; transition to Wait or re-enter Compete if preemptable.
			fresh := obj.DeepCopyObject().(T)
			if getErr := cache.GetClient().Get(ctx, namespacedName, fresh); getErr != nil {
				return obj, fmt.Errorf("singleflight: failed to read after seq conflict: %w", getErr)
			}
			ann, _ := parseSingleflightAnnotation(fresh, key)
			if !ann.Done && isPreemptable(ann.LastUpdate, preemptionThreshold) {
				// Preempt the stale runner
				return singleflightCompete(ctx, cache, fresh, key, ann, precheck, modifier, function, preemptionThreshold)
			}
			log.Info("singleflight: seq conflict, entering wait", "currentSeq", conflict.currentSeq, "mySeq", seq)
			return singleflightWait(ctx, cache, fresh, key, seq)
		}
		return obj, fmt.Errorf("singleflight: failed to claim lock: %w", err)
	}

	// We are the Runner. `claimed` holds the post-modifier object.
	log.Info("singleflight: claimed lock, entering run phase", "seq", seq)

	// Guarantee lock release
	defer func() {
		releaseSingleflightLock(cache, claimed, key, seq)
	}()

	// Execute function with the post-modifier object.
	// The function error is returned to the caller (NOT swallowed).
	// The caller (Pause/Resume retry loop) will see the error and can decide
	// whether to retry or abort.
	funcErr := function(claimed)
	if funcErr != nil {
		log.Error(funcErr, "singleflight: function failed, releasing lock")
	}
	return claimed, funcErr
}

func singleflightWait[T client.Object](
	ctx context.Context,
	cache Provider,
	obj T,
	key string,
	watchedSeq int64,
) (T, error) {
	log := klog.FromContext(ctx).WithValues("singleflightKey", key, "object", klog.KObj(obj), "watchedSeq", watchedSeq)

	// Use a distinct key prefix ("sf:<key>/...") so single-flight wait entries
	// do NOT collide with existing WaitForObjectSatisfied entries which use
	// WaitHookKey format "*type/ns/name".
	waitKey := fmt.Sprintf("%s%s/%s", SingleflightWaitKeyPrefix, key, cacheutils.WaitHookKey(obj))
	// Use a distinct action to avoid colliding with Pause/Resume/Checkpoint wait entries
	action := cacheutils.WaitAction("singleflight:" + key)

	checker := func(fresh T) (bool, error) {
		ann, hasAnn := parseSingleflightAnnotation(fresh, key)
		if !hasAnn {
			// Annotation was removed; treat as done
			return true, nil
		}
		if ann.Done || ann.Seq > watchedSeq {
			return true, nil
		}
		return false, nil
	}

	entry := cacheutils.NewWaitEntry(ctx, action, checker)
	actual, loaded := cache.GetWaitHooks().LoadOrStore(waitKey, entry)
	if loaded {
		// An existing wait entry for this key already exists.
		// We must wait on the EXISTING entry's channel, not the newly created one.
		entry = actual.(*cacheutils.WaitEntry[T])
		log.V(5).Info("singleflight: reusing existing wait entry", "existingAction", entry.Action)
	} else {
		log.V(5).Info("singleflight: created wait entry")
		go pollSingleflightWaitEntry(ctx, cache, obj, key, entry)
	}

	// Wait for the Runner to finish (or ctx cancellation)
	select {
	case <-entry.Done():
		log.V(5).Info("singleflight: wait entry signaled done")
	case <-ctx.Done():
		cache.GetWaitHooks().Delete(waitKey)
		return obj, fmt.Errorf("singleflight: wait context cancelled: %w", ctx.Err())
	}

	// Clean up wait entry
	cache.GetWaitHooks().Delete(waitKey)

	// Return latest from informer (NOT prechecked; caller checks business state)
	fresh := obj.DeepCopyObject().(T)
	namespacedName := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	if err := cache.GetClient().Get(ctx, namespacedName, fresh); err != nil {
		return obj, fmt.Errorf("singleflight: failed to read object after wait: %w", err)
	}
	return fresh, nil
}

func pollSingleflightWaitEntry[T client.Object](
	ctx context.Context,
	cache Provider,
	obj T,
	key string,
	entry *cacheutils.WaitEntry[T],
) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	namespacedName := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	for {
		select {
		case <-ctx.Done():
			entry.Close()
			return
		case <-entry.Done():
			return
		case <-ticker.C:
			fresh := obj.DeepCopyObject().(T)
			if err := cache.GetClient().Get(ctx, namespacedName, fresh); err != nil {
				continue
			}
			satisfied, err := entry.Check(fresh)
			if satisfied || err != nil {
				entry.Close()
				return
			}
		}
	}
}

// releaseSingleflightLock sets done=true on the annotation with a guaranteed
// background context. It retries until success or the background context expires.
func releaseSingleflightLock[T client.Object](
	cache Provider,
	obj T,
	key string,
	seq int64,
) {
	releaseCtx, cancel := context.WithTimeout(context.Background(), releaseLockTimeout)
	defer cancel()

	namespacedName := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	log := klog.FromContext(releaseCtx).WithValues("singleflightKey", key, "object", namespacedName, "seq", seq)

	err := retry.OnError(
		retry.DefaultBackoff,
		func(err error) bool { return true }, // always retry
		func() error {
			fresh := obj.DeepCopyObject().(T)
			if err := cache.GetAPIReader().Get(releaseCtx, namespacedName, fresh); err != nil {
				return err
			}
			ann, _ := parseSingleflightAnnotation(fresh, key)
			if ann.Seq != seq {
				// Another runner has taken over; our seq is obsolete. No need to release.
				return nil
			}
			setSingleflightAnnotation(fresh, key, seq, true)
			return cache.GetClient().Update(releaseCtx, fresh)
		},
	)
	if err != nil {
		log.Error(err, "singleflight: failed to release lock after all retries; lock will be preempted by timeout")
	} else {
		log.V(5).Info("singleflight: lock released successfully")
	}
}
