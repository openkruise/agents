## 1. Sandbox ID Contract and Configuration

- [x] 1.1 Add the schema-level reserved Sandbox label constant while keeping the existing Checkpoint annotation constant and metadata maps distinct (design §§4.1, 12.1).
- [x] 1.2 Add label-aware ID resolution/assignment plus the legacy encoder and peer-boundary decoder,
  including table-driven round-trip, invalid-encoding, fallback, trust, invalid-UID, and idempotency
  tests (design §§5, 7.1, 11.6, 18.2).
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
- [x] 5.3 Keep claimed-Sandbox lookup to one indexed result under the system-owned global-ID uniqueness contract without parsing the client ID (design §10).
- [x] 5.4 Extend cache tables for default and injected resolvers, label-driven index movement, and one-key-only behavior (design §18.6).

## 6. Shared Route, Projection, and Store

- [x] 6.1 Add the neutral `pkg/sandboxroute` Route, projection-ready `ProjectionSource`, stateless `FromSandbox`, token-redacting `String`, and temporary compatibility aliases required for staged call-site migration (design §§11.1-11.2).
- [x] 6.2 Implement Store state for ObjectKey-backed full records, deletion fences, an active
  SandboxID-to-ObjectKey index, and structured mutation results (design §11.3).
- [x] 6.3 Validate resourceVersions at the Route boundary and use Kubernetes' older/equal/newer comparison semantics in the Store (design §11.4).
- [x] 6.4 Implement strictly-newer full-route upsert and atomic legacy-to-short replacement using
  only ObjectKey resourceVersion ordering (design §11.4).
- [x] 6.5 Admit every Route through one normalization and validation function, normalizing
  reversible legacy ID-only Routes and rejecting opaque/short ID-only or partial Routes (design
  §§11.4, 11.6).
- [x] 6.6 Store each complete Route only in the ObjectKey record table, resolve active reads through
  SandboxID-to-ObjectKey and then ObjectKey-to-record lookup, and maintain that index incrementally
  under the Store lock (design §11.3).
- [x] 6.7 Implement authoritative `Delete{ObjectKey,RV}`, conditional `DeleteIfTracked`, permanent
  RV-only fences, and the empty-RV synthetic-tombstone fallback (design §11.5).
- [x] 6.8 Add focused Store, codec, peer-boundary, and projection tables for RV fences, delete,
  record/fence mutual exclusion, normalization/rejection, token compatibility, state normalization,
  and token redaction (design §18.6).

## 7. Informer-Driven Deletion Fencing

- [x] 7.1 Remove Repairer, mutation generations, confirmation delay, queue wiring, and route
  APIReader observations from manager and gateway (design §11.8).
- [x] 7.2 Subscribe both components directly to Sandbox informer Add, Update, and Delete events and
  preserve the full object until a Route mutation is constructed (design §§11.7-11.8).
- [x] 7.3 Treat deletionTimestamp Add/Update events and normal DELETE objects as authoritative
  deletes carrying their observed resourceVersion (design §11.8).
- [x] 7.4 Treat every `DeletedFinalStateUnknown` as a key-only, empty-RV deletion because an
  embedded Sandbox object is not a trustworthy final-state watermark (design §11.8).
- [x] 7.5 Keep fences permanently and make policy exclusion conditional on pre-existing Store state
  so never-matched objects do not allocate fences (design §§11.3, 11.8).
- [x] 7.6 Cover normal DELETE, object-bearing and key-only tombstones, deletionTimestamp,
  out-of-order Upsert, and unseen policy-exclusion behavior (design §18.6).

## 8. Sandbox-Manager Route Integration

- [x] 8.1 Keep manager route policy, projection, and feeder composition in sandbox-manager while
  removing Repairer ownership (design §§7.2-7.3, 11.7).
- [x] 8.2 Remove `GetSandboxID` and `GetRoute` from `infra.Sandbox`, add only format-neutral `GetPodIP`, and keep any infra staleness dependency as an opaque Route reader (design §7.3).
- [x] 8.3 Wrap `infra.Sandbox` in a manager-owned `ProjectionSource` and pass it to shared `FromSandbox`, without asking infra to select an ID, choose token compatibility, or construct a Route (design §§7.2, 11.2).
- [x] 8.4 Route cache events and claim/clone/pause/resume/delete/recycle lifecycle synchronization through shared stateless projection and authority-specific Store operations (design §11.7).
- [x] 8.5 Replace manager proxy route storage and peer refresh handling with the shared Store while preserving component-specific status policy and HTTP result mapping (design §§11.1, 11.6-11.7).
- [x] 8.6 Register the neutral route subscription before the shared cache starts so initial LIST Add
  events populate the manager Store (design §11.8).
- [x] 8.7 Extend manager adapter and proxy tests for projection ownership, lifecycle updates/deletes,
  peer compatibility, and absence of infra-owned projection or repair workers (design §§18.3, 18.6).
- [x] 8.8 Keep Sandbox informer registration, tombstone decoding, and CRD conversion inside
  `sandboxcr`; Manager consumes only neutral Sandbox or Delete events and performs no route API reads.

## 9. Sandbox-Gateway Route Integration

- [x] 9.1 Wrap gateway registry around the shared Store and pass a lightweight state-snapshot source that owns label-aware ID resolution and token compatibility directly to shared `FromSandbox` (design §§11.1-11.2).
- [x] 9.2 Replace key-only reconciliation with a raw informer handler that projects full Routes and
  preserves deletion object resourceVersions (design §§11.2, 11.5, 11.7).
- [x] 9.3 Remove injected mixed-version delete fallback construction from gateway reconciliation and
  centralize reversible legacy decoding in `AdmitRoute` without parsing client lookup IDs (design
  §§7.1, 11.5-11.6).
- [x] 9.4 Normalize legacy peer updates/deletes through `AdmitRoute`, then apply the same Store
  mutation rules while preserving `400`/`204` endpoint results (design §§11.5-11.7, 17).
- [x] 9.5 Gate only Registry reads on initial handler synchronization while accepting informer and
  peer mutations before ready (design §§11.7-11.8).
- [x] 9.6 Extend gateway adapter, registry, and peer endpoint tables for full and reversible legacy
  payloads, opaque/partial rejection, stale peer no-ops, event-object deletion, tombstones, and
  initial-sync writes (design §18.6).

## 10. E2B, Checkpoint, and Pagination Surfaces

- [x] 10.1 Replace E2B direct infra ID selection in response conversion, logs, pagination, and operation context with `SandboxManager.ResolveSandboxID` (design §§7.2, 7.4, 16).
- [x] 10.2 Add protected response-only `e2b.agents.kruise.io/sandbox-resource` metadata after ordinary filtering and prevent user override or persistence (design §13.1).
- [x] 10.3 Append `sandboxResource=<namespace>/<name>` only to downstream errors after successful lookup and ownership authorization, preserving error codes and withholding context from not-found and unauthorized responses (design §13.2).
- [x] 10.4 Use neutral `GetPodIP` for opt-in E2B Pod-IP metadata without constructing a Route (design §§7.3-7.4, 18.7).
- [x] 10.5 Route Checkpoint creation through manager core, overwrite the internal option with the final resolved ID, and reject an empty core-supplied ID in infra (design §12.1).
- [x] 10.6 Keep Checkpoint history and Sandbox pagination IDs opaque and point-in-time, updating fixtures that assume reversibility while retaining explicit legacy cases (design §12).
- [x] 10.7 Extend E2B response/error/list/snapshot tables for protected metadata, spoofing, authorization disclosure, Pod IP, historical Checkpoints, empty IDs, and opaque pagination (design §18.7).

## 11. Observability

- [x] 11.1 Remove dedicated Sandbox ID Prometheus metrics for legacy resolution and assignment; keep identity diagnosis in structured logs only (design §15).
- [x] 11.2 Keep shared Store mutation and peer compatibility out of dedicated short-ID route metrics
  while retaining route outcomes in structured logs (design §15).
- [x] 11.3 Add structured assignment and fencing logs with fixed reason enums; keep successful
  assignment at debug level and preserve access-token redaction (design §15).

## 12. Verification and Boundary Audit

- [x] 12.1 Keep all changed Go tests table-driven, prefer existing tables, and use `expectError string` substring assertions for error cases (design §18.1).
- [x] 12.2 Run focused unit tests only for changed packages under `pkg/`; do not run E2E tests under `test/` (design §18.8).
- [x] 12.3 Statically confirm only manager core writes the Sandbox-ID label and that API/controller code outside core only compares the reserved key for validation or recycle preservation (design §18.8).
- [x] 12.4 Statically confirm infra, E2B, cache, proxy utilities, and shared routing contain no Sandbox-ID format/assignment policy or prohibited manager-domain imports (design §18.8).
- [x] 12.5 Statically confirm `infra.Sandbox` exposes neither `GetSandboxID` nor `GetRoute`, route
  maintenance contains no Repairer or APIReader query, gateway performs no direct production ID
  concatenation, and no new code parses client IDs on `--` (design §18.8).
- [x] 12.6 Build sandbox-manager and sandbox-gateway binaries to `/private/tmp` only after focused tests and static checks pass (design §18.8).

## 13. Route Projection Simplification

- [x] 13.1 Remove the stateful `Projector`, `ProjectionInput`, and projector fields/options from manager and gateway composition.
- [x] 13.2 Centralize Sandbox metadata, state normalization, owner, resolved ID, and access-token assembly in `sandboxroute.FromSandbox`, while keeping component-specific resolution in projection sources and avoiding `infra.Sandbox.GetRoute()`.
- [x] 13.3 Keep manager and gateway validation/retry semantics unchanged; retain gateway token fallback in its projection source and use one cached state snapshot for inclusion and projection.
- [x] 13.4 Update focused projection, manager, and gateway tests and re-run strict OpenSpec validation plus the affected builds.
