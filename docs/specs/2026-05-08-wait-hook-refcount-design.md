# Wait Hook Refcount Design

## Context

`pkg/cache/utils/wait.go` currently stores one wait hook per object key. Concurrent waiters for the same object and action reuse the same `WaitEntry`. Concurrent waiters for the same object but a different action are rejected.

The current cleanup path deletes the map entry unconditionally when any waiter exits. This allows a reused waiter to delete the hook while another waiter is still waiting, which weakens the intended deduplication behavior for concurrent Pause/Resume requests.

## Goals

- Keep the existing rule that one object can only wait for one action at a time.
- Allow multiple waiters for the same object and same action to share one entry.
- Delete the entry only after the last waiter exits.
- Avoid binding a new waiter to an entry that has already been deleted from the map.
- Reduce the chance that a late joiner misses the completion signal and waits until timeout.
- Keep the design local to `pkg/cache/utils/wait.go` and avoid changing Pause/Resume business logic.

## Non-goals

- Do not introduce completed-result caching.
- Do not change the action-conflict behavior.
- Do not move operation state into lower-level components in this change.
- Do not change the public `WaitTask` API.

## Design

### Refcounted WaitEntry

Add a mutex-protected reference count to `WaitEntry`:

```go
type WaitEntry[T client.Object] struct {
    Action WaitAction

    ctx       context.Context
    done      chan struct{}
    checker   CheckFunc[T]
    closeOnce sync.Once

    mu   sync.Mutex
    refs int
}
```

Lifecycle is managed through package-level helper functions rather than public entry methods.

### AcquireEntry

`AcquireEntry` is responsible for finding or creating the map entry and incrementing its refcount only when the entry is still the current value in `waitHooks`.

Expected behavior:

- Use `LoadOrStore` to find or create the entry.
- If the existing entry has a different action, return the existing conflict error and do not increment refs.
- Lock the entry.
- Re-read `waitHooks.Load(key)` while holding the entry lock.
- Increment refs only if the map still points to the same entry.
- If the map no longer points to the same entry, unlock and retry.

This prevents a waiter from acquiring an orphan entry that another goroutine has just removed.

### ReleaseEntry

`ReleaseEntry` is responsible for decrementing the refcount and deleting the entry when the last waiter exits.

Expected behavior:

- Lock the entry.
- Decrement refs.
- If refs reaches zero, call `waitHooks.CompareAndDelete(key, entry)` while still holding the entry lock.
- Unlock the entry.

Holding the lock across both decrement and delete keeps release/delete as one lifecycle transition from the entry's perspective. Any concurrent acquire that saw the old entry must wait for the lock, then re-check whether the entry is still current before incrementing refs.

### Late Joiner Check

After a waiter successfully acquires an entry, perform an immediate refresh-and-check before entering the select loop. This handles the case where a previous waiter completed and removed the old entry just before the new waiter registered.

The check must not call `satisfiedFunc(obj)` directly, because `obj` can be stale. It must call `update(obj)` first, then evaluate the refreshed object.

### Periodic Polling

Add a ticker inside the wait select loop with a 10 second interval. On each tick, refresh the object and evaluate the condition.

This does not guarantee instant completion, but it changes the missed-event fallback from waiting until the full timeout to waiting at most one poll interval after the desired state becomes visible to the update function.

### CheckObjectSatisfied

Extract a helper with `(bool, error)` semantics:

```go
func CheckObjectSatisfied[T client.Object](ctx context.Context, obj T, update UpdateFunc[T], satisfiedFunc CheckFunc[T]) (bool, error)
```

Behavior:

- Run `update(obj)`.
- Run `satisfiedFunc(updated)`.
- Return `true, nil` when satisfied.
- Return `false, nil` when not satisfied.
- Return `false, err` for update errors or checker errors.

`DoubleCheckObjectSatisfied` should reuse this helper and keep its existing public behavior by converting `satisfied=false` into `object is not satisfied during double check`.

## Wait Flow

`WaitForObjectSatisfied` should follow this sequence:

1. Run the existing fast path `satisfiedFunc(obj)` before registering.
2. If already satisfied, return immediately.
3. If timeout is non-positive, return the existing skipped-wait error.
4. Acquire the entry with `AcquireEntry`.
5. Defer `ReleaseEntry`.
6. Run immediate `CheckObjectSatisfied`.
7. If satisfied, return nil.
8. Start a 10 second ticker.
9. Select on `entry.Done()`, ticker, and `waitCtx.Done()`.
10. On `entry.Done()`, call `DoubleCheckObjectSatisfied`.
11. On ticker, call `CheckObjectSatisfied`; return nil if satisfied, continue if not satisfied.
12. On timeout/cancel, call `DoubleCheckObjectSatisfied`.

## Error Handling

- Action mismatch remains an immediate error.
- Immediate refresh/check returns real update or checker errors immediately.
- Ticker refresh/check returns real update or checker errors immediately.
- A simple "not satisfied yet" ticker result continues waiting without logging as an error.
- Timeout/cancel keeps the existing final double-check behavior.

## Testing

Add table-driven tests in `pkg/cache/utils/wait_test.go` covering:

- Two same-action waiters share one entry and the entry remains until both release.
- A reused waiter exiting first does not delete the entry while another waiter is still waiting.
- A late waiter does not acquire an orphan entry after deletion.
- Different actions for the same object still return the existing conflict behavior.
- Immediate post-acquire check returns success without waiting when the refreshed object is already satisfied.
- Ticker polling returns success when no done signal arrives but the refreshed object becomes satisfied.

Use short test-specific poll intervals if the implementation allows injecting the interval internally; otherwise keep tests driven by direct signaling where possible to avoid slow tests.

## Open Decisions

- Poll interval is 10 seconds for production behavior.
