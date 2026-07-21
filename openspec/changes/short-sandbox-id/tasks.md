## 1. Sandbox ID Contract and Configuration

- [x] 1.1 Add the schema-level reserved Sandbox label constant while keeping the existing Checkpoint annotation constant and metadata maps distinct (design §§4.1, 12.1).
- [x] 1.2 Add `pkg/sandboxid` with `Resolve`, `Legacy`, `GenerateShort`, and `AssignShort`, including table-driven encoding, fallback, trust, invalid-UID, and idempotency tests (design §§5, 7.1, 18.2).
- [x] 1.3 Add `--enable-short-sandbox-id=false` to sandbox-manager and make it gate assignment only, never label-aware resolution (design §6; spec: Flag-controlled final assignment).
- [x] 1.4 Expose `SandboxManager.ResolveSandboxID` and route server-facing ID resolution through the manager facade (design §7.2).

## 2. Generic Infra Mutation Plumbing

- [x] 2.1 Change claim and clone pre-lock `Modifier` callbacks to return errors and update existing callers to preserve their current mutations while returning `nil` (design §8).
- [x] 2.2 Add the optional metadata-only `PostModifier func(metav1.Object) (bool, error)` to claim and clone options without introducing sandbox-ID policy into infra (design §8).
- [x] 2.3 Execute PostModifier after existing post-processing by direct `APIReader` Get, deep copy, conditional Update, and wrapper refresh; skip all extra work when the callback is nil (design §8.1).
- [x] 2.4 Re-read through `APIReader` and re-run the idempotent callback on each conflict retry, honoring context cancellation and preserving NotFound as an operation failure (design §§8.1-8.2, 17).
- [x] 2.5 Preserve manager error codes and original callback/update causes through the public claim and clone boundaries (design §§8.2, 17).
- [x] 2.6 Extend the existing infra claim/clone tables for nil, unchanged, changed, conflict, callback error, cancellation, refresh, and ordering cases (design §18.5).

## 3. Manager Assignment and Reserved-Key Guards

- [x] 3.1 Guard caller pre-lock Modifier callbacks by snapshotting reserved-key presence and value and rejecting add, change, or delete before persistence (design §§4.1, 7.2).
- [x] 3.2 Guard caller PostModifier callbacks with the same exact presence/value rule, including deletion of an existing empty entry (design §§4.1, 7.2).
- [x] 3.3 Compose the guarded caller PostModifier before core `AssignShort`, combine changed results, and stop on the first error (design §7.2).
- [x] 3.4 Install guards even when assignment is disabled, keep guard failures terminal, and prevent candidate/create retry from treating them as retriable selection failures (design §§4.1, 7.2, 17).
- [x] 3.5 Return claim and clone success only after infra refreshes the final object, preserving an existing label and deriving clone identity from the clone UID (design §§8.3, 9).
- [x] 3.6 Extend manager claim/clone tables for flag states, callback composition, reserved-key mutations, existing labels, recycled legacy transition, cleanup, and final returned IDs (design §18.3).

## 4. Public Metadata and Recycle Boundaries

- [x] 4.1 Reject exact E2B label extensions for both `agents.kruise.io/sandbox-id` and `e2b.agents.kruise.io/sandbox-resource` before claim or clone options are built (design §§4.1, 13.1).
- [x] 4.2 Reject the reserved Sandbox-ID label in SandboxClaim reconciliation before invoking infra (design §4.1).
- [x] 4.3 Confirm SandboxSet and SandboxTemplate materialization strips the reserved internal label and add explicit table cases (design §§4.1, 9.4).
- [x] 4.4 Exclude the reserved label from `BuildUserMetadataKeys` and make `resetMetadataForPool` preserve it even when historical or crafted cleanup metadata lists the key (design §9.4).
- [x] 4.5 Extend existing E2B, SandboxClaim, materialization, metadata-key, and recycle tables for all protected-key paths (design §18.4).

## 5. Neutral Cache Lookup

- [x] 5.1 Add optional claimed-Sandbox ID resolver injection to `pkg/cache` while retaining the legacy resolver for existing callers (design §10).
- [x] 5.2 Inject label-aware resolution from sandbox-manager regardless of the assignment flag and avoid importing the manager-domain package from cache (design §§7.1, 10).
- [x] 5.3 Change claimed-Sandbox lookup to distinguish zero, one, and multiple indexed matches and return a fail-closed ambiguity error without parsing the client ID (design §10).
- [x] 5.4 Extend cache tables for default and injected resolvers, label-driven index movement, one-key-only behavior, and duplicate-ID lookup (design §18.6).

## 6. Shared Route, Projection, and Store

- [x] 6.1 Add the neutral `pkg/sandboxroute` Route, projection-ready `ProjectionSource`, stateless `FromSandbox`, token-redacting `String`, and temporary compatibility aliases required for staged call-site migration (design §§11.1-11.2).
- [x] 6.2 Implement Store indexes for ObjectKey, UID, and SandboxID plus full records, compatibility ID-only records, retired/deletion fences, collision markers, mutation generations, and structured mutation results (design §11.3).
- [x] 6.3 Implement explicit older/equal/newer/unorderable resourceVersion comparison using identical-string equality and base-10 unsigned ordering (design §11.4).
- [x] 6.4 Implement full-route upsert, same-UID ID transition, different-UID replacement, ID-only adoption, retired ownership, and atomic legacy-to-short replacement (design §11.4).
- [x] 6.5 Implement the constrained ID-only compatibility state machine so full or retired ownership cannot be downgraded, aliased, or quarantined by lower-authority traffic (design §11.4).
- [x] 6.6 Implement cross-ObjectKey collision quarantine and recovery without last-write-wins, retaining claimant state while removing collided IDs from lookup (design §11.3).
- [x] 6.7 Implement authoritative ObjectKey deletion, conditional full-peer deletion, conditional ID-only deletion, legacy fallback restricted to ID-only records, and UID/RV fences (design §11.5).
- [x] 6.8 Add focused Store and projection tables for every full/ID-only conversion, RV/UID fence, collision, delete, alias-prevention, count, token compatibility, state normalization, and token-redaction case in design §18.6.

## 7. Asynchronous Targeted Repair and Compatibility Expiry

- [x] 7.1 Add a shared deduplicated rate-limiting Repairer queue keyed by ObjectKey that retains the newest affected-record generation and runs fixed low-concurrency workers (design §11.8).
- [x] 7.2 Inject component-specific authoritative observation, projection, scope, inclusion, deletion, and exclusion callbacks without broadening either component's watched Sandbox set (design §§11.7-11.8).
- [x] 7.3 Apply present and absent authoritative observations only when the affected record or fence generation still matches, ignoring stale in-flight results (design §11.8).
- [x] 7.4 Retry transient Get/projection failures with rate-limited backoff, stop on context cancellation, and avoid indefinite retries for confirmed live duplicate claimants (design §11.8).
- [x] 7.5 Expire ID-only records and prune retired/deletion fences only after the bounded old-peer drain window and required targeted confirmation, never activating an ID during pruning (design §§11.3, 11.8).
- [x] 7.6 Add focused Repairer tests for deduplication, newest generation, direct-read outcomes, backoff, stale results, unrelated Store activity, claimant repair, fence confirmation, expiry, and absence of full Sandbox List calls (design §18.6).

## 8. Sandbox-Manager Route Integration

- [x] 8.1 Move manager route policy, projection, feeder composition, and Repairer ownership from `sandboxcr.Infra` to the sandbox-manager composition root (design §§7.2-7.3, 11.7).
- [x] 8.2 Remove `GetSandboxID` and `GetRoute` from `infra.Sandbox`, add only format-neutral `GetPodIP`, and keep any infra staleness dependency as an opaque Route reader (design §7.3).
- [x] 8.3 Wrap `infra.Sandbox` in a manager-owned `ProjectionSource` and pass it to shared `FromSandbox`, without asking infra to select an ID, choose token compatibility, or construct a Route (design §§7.2, 11.2).
- [x] 8.4 Route cache events and claim/clone/pause/resume/delete/recycle lifecycle synchronization through shared stateless projection and authority-specific Store operations (design §11.7).
- [x] 8.5 Replace manager proxy route storage and peer refresh handling with the shared Store while preserving component-specific status policy and HTTP result mapping (design §§11.1, 11.6-11.7).
- [x] 8.6 Start manager targeted repair with the required neutral Infra observation source and the same visibility/inclusion predicate as the normal feeder (design §11.8).
- [x] 8.7 Extend manager adapter and proxy tests for projection ownership, lifecycle updates/deletes, peer compatibility, collision/repair enqueue, and absence of infra-owned projection workers (design §§18.3, 18.6).
- [x] 8.8 Keep Sandbox reconcile registration, CRD conversion, direct API reads, and Kubernetes error classification inside `sandboxcr`; Manager route code consumes only `types.NamespacedName` keys and neutral `infra.Sandbox` values from `infra.RouteSandboxSource`, with nil as authoritative absence.

## 9. Sandbox-Gateway Route Integration

- [x] 9.1 Wrap gateway registry around the shared Store and pass a lightweight state-snapshot source that owns label-aware ID resolution and token compatibility directly to shared `FromSandbox` (design §§11.1-11.2).
- [x] 9.2 Update gateway reconciliation to project full Routes for present included Sandboxes and authoritatively delete by ObjectKey for NotFound, deletion, or exclusion (design §§11.2, 11.7).
- [x] 9.3 Restrict direct `<namespace>--<name>` construction to the injected mixed-version delete fallback that can remove only an ID-only compatibility record (design §§7.1, 11.5).
- [x] 9.4 Route gateway peer updates and deletes through full/ID-only validation, Store authority rules, and `400`/`204`/`409` endpoint results (design §§11.5-11.7, 17).
- [x] 9.5 Start gateway targeted repair with its direct API reader and reuse the controller's namespace, label, state, deletion, and inclusion policy (design §§11.7-11.8).
- [x] 9.6 Extend gateway adapter, registry, and peer endpoint tables for full and ID-only payloads, malformed ObjectKeys, stale events, authoritative deletion, collision enqueue, and no direct ID parsing (design §18.6).

## 10. E2B, Checkpoint, and Pagination Surfaces

- [x] 10.1 Replace E2B direct infra ID selection in response conversion, logs, pagination, and operation context with `SandboxManager.ResolveSandboxID` (design §§7.2, 7.4, 16).
- [x] 10.2 Add protected response-only `e2b.agents.kruise.io/sandbox-resource` metadata after ordinary filtering and prevent user override or persistence (design §13.1).
- [x] 10.3 Append `sandboxResource=<namespace>/<name>` only to downstream errors after successful lookup and ownership authorization, preserving error codes and withholding context from not-found and unauthorized responses (design §13.2).
- [x] 10.4 Use neutral `GetPodIP` for opt-in E2B Pod-IP metadata without constructing a Route (design §§7.3-7.4, 18.7).
- [x] 10.5 Route Checkpoint creation through manager core, overwrite the internal option with the final resolved ID, and reject an empty core-supplied ID in infra (design §12.1).
- [x] 10.6 Keep Checkpoint history and Sandbox pagination IDs opaque and point-in-time, updating fixtures that assume reversibility while retaining explicit legacy cases (design §12).
- [x] 10.7 Extend E2B response/error/list/snapshot tables for protected metadata, spoofing, authorization disclosure, Pod IP, historical Checkpoints, empty IDs, and opaque pagination (design §18.7).

## 11. Observability

- [x] 11.1 Add bounded metrics for legacy resolution, assignment success/failure, and collisions without namespace, name, UID, or Sandbox-ID labels (design §15).
- [x] 11.2 Add bounded route metrics for invalid mutations, ID-only/collision records, and legacy fallback usage (design §15).
- [x] 11.3 Expose targeted-repair queue depth while keeping repair outcomes, stale results, retries, and PostModifier details in existing structured logs and operation-stage timings (design §15).
- [x] 11.4 Add structured assignment, collision, fencing, and repair logs with fixed reason enums; keep successful assignment at debug level and preserve access-token redaction (design §15).

## 12. Verification and Boundary Audit

- [x] 12.1 Keep all changed Go tests table-driven, prefer existing tables, and use `expectError string` substring assertions for error cases (design §18.1).
- [x] 12.2 Run focused unit tests only for changed packages under `pkg/`; do not run E2E tests under `test/` (design §18.8).
- [x] 12.3 Statically confirm only manager core writes the Sandbox-ID label and that API/controller code outside core only compares the reserved key for validation or recycle preservation (design §18.8).
- [x] 12.4 Statically confirm infra, E2B, cache, proxy utilities, and shared routing contain no Sandbox-ID format/assignment policy or prohibited manager-domain imports (design §18.8).
- [x] 12.5 Statically confirm `infra.Sandbox` exposes neither `GetSandboxID` nor `GetRoute`, infra starts no route feeder/Repairer, gateway performs no direct production ID concatenation, and no new code parses client IDs on `--` (design §18.8).
- [x] 12.6 Build sandbox-manager and sandbox-gateway binaries to `/private/tmp` only after focused tests and static checks pass (design §18.8).

## 13. Route Projection Simplification

- [x] 13.1 Remove the stateful `Projector`, `ProjectionInput`, and projector fields/options from manager and gateway composition.
- [x] 13.2 Centralize Sandbox metadata, state normalization, owner, resolved ID, and access-token assembly in `sandboxroute.FromSandbox`, while keeping component-specific resolution in projection sources and avoiding `infra.Sandbox.GetRoute()`.
- [x] 13.3 Keep manager and gateway validation/retry semantics unchanged; retain gateway legacy-ID observability and token fallback in its projection source and use one cached state snapshot for inclusion and projection.
- [x] 13.4 Update focused projection, manager, and gateway tests and re-run strict OpenSpec validation plus the affected builds.
