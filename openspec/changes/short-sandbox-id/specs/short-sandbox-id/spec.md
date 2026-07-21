## ADDED Requirements

<!--
Traceability to the unchanged design artifact:
- Authoritative Sandbox ID resolution: Sections 4 and 6; Acceptance Criteria 2-3.
- Complete UID short-ID encoding: Section 5; Acceptance Criterion 1.
- Flag-controlled final assignment: Sections 6, 8, and 9; Acceptance Criteria 1-2.
- One-way CR identity transition: Sections 4.3 and 9.4; Acceptance Criteria 5-6 and 16.
- Reserved metadata protection: Sections 4.1, 9.4, and 13.1; Acceptance Criterion 4.
- Final assignment failure semantics: Sections 8.1-8.3 and 17; Acceptance Criterion 1.
- Opaque and unambiguous cache lookup: Section 10; Acceptance Criteria 5 and 7.
- Shared atomic route projection: Sections 11.1-11.4; Acceptance Criteria 5, 7-8.
- Version-fenced peer compatibility and deletion: Sections 11.3-11.7 and 17; Acceptance Criteria 8-9 and 11.
- Collision quarantine and targeted repair: Sections 10, 11.3, and 11.8; Acceptance Criteria 7 and 10.
- Authorized E2B resource diagnostics: Section 13; Acceptance Criterion 14.
- Point-in-time Checkpoint and opaque pagination identity: Section 12; Acceptance Criterion 13.
- Staged activation and rollback boundary: Section 14; Acceptance Criterion 15.
- Bounded identity observability: Section 15.
-->
### Requirement: Authoritative Sandbox ID resolution
The system SHALL return a Sandbox's non-empty `agents.kruise.io/sandbox-id` label unchanged and SHALL otherwise resolve the legacy `<namespace>--<name>` ID, independent of whether new short-ID assignment is enabled.

#### Scenario: Existing non-empty label is authoritative
- **WHEN** a Sandbox has a non-empty `agents.kruise.io/sandbox-id` label
- **THEN** every component returns that label unchanged without validating its alphabet, length, UID relationship, or origin

#### Scenario: Missing or empty label uses the legacy ID
- **WHEN** the label is absent or empty
- **THEN** the Sandbox resolves to `<namespace>--<name>`

### Requirement: Complete UID short-ID encoding
The system SHALL generate a short Sandbox ID by encoding all 16 UUID bytes of the Kubernetes UID with unpadded lowercase RFC 4648 Base32, producing 26 characters from `[a-z2-7]` without truncation.

#### Scenario: Valid UID is encoded deterministically
- **WHEN** short assignment processes the same valid 16-byte Kubernetes UID more than once
- **THEN** it produces the same 26-character lowercase unpadded Base32 value each time

#### Scenario: Invalid UID fails generation
- **WHEN** short assignment receives a UID that cannot be decoded as 16 UUID bytes
- **THEN** generation fails instead of persisting a fallback or truncated ID

### Requirement: Flag-controlled final assignment
The system SHALL use `--enable-short-sandbox-id=false` as an assignment-only gate and, when enabled, persist an unlabeled claim or clone's generated short ID at the final successful stage before returning its client-visible identity.

#### Scenario: Assignment is disabled
- **WHEN** claim or clone succeeds for an unlabeled Sandbox while short assignment is disabled
- **THEN** no short-ID label is added and the operation returns the legacy ID

#### Scenario: Assignment is enabled
- **WHEN** claim or clone succeeds for an unlabeled Sandbox while short assignment is enabled
- **THEN** the generated short ID is persisted before the success response returns that ID

#### Scenario: Clone uses its own identity
- **WHEN** a Sandbox is cloned while short assignment is enabled
- **THEN** the clone's own UID generates its short ID and no sandbox-ID label is inherited from the source or template

### Requirement: One-way CR identity transition
The system SHALL treat Sandbox ID as the identity of the Sandbox CR, preserve a non-empty label through recycle and later operations, and expose no simultaneous active legacy and short aliases.

#### Scenario: Recycled unlabeled Sandbox transitions later
- **WHEN** an unlabeled Sandbox returns to a pool and is later claimed with short assignment enabled
- **THEN** it may transition from its legacy ID to one persisted short ID

#### Scenario: Labeled Sandbox is recycled or assignment is disabled
- **WHEN** a labeled Sandbox is recycled, claimed again, or observed after the feature flag is disabled
- **THEN** it retains the same short ID and does not regain a legacy alias

#### Scenario: Ownership changes across claims
- **WHEN** a Sandbox CR is reused by another claim or tenant session
- **THEN** authorization and external consumers do not infer the current owner or session from the Sandbox ID alone

### Requirement: Reserved metadata protection
The system MUST reject or strip user-controlled and callback-controlled attempts to add, change, or delete the reserved Sandbox-ID label, while preserving a core-assigned label during metadata cleanup and recycle.

#### Scenario: Public input supplies the reserved label
- **WHEN** E2B extensions or SandboxClaim labels supply `agents.kruise.io/sandbox-id`
- **THEN** the request is rejected before infra, cache, or routing state is invoked

#### Scenario: Pool or template carries the reserved label
- **WHEN** SandboxSet or SandboxTemplate metadata is materialized into a Sandbox
- **THEN** the reserved internal label is not inherited

#### Scenario: Caller callback mutates the reserved label
- **WHEN** a pre-lock Modifier or final PostModifier adds, changes, or deletes the reserved key
- **THEN** the operation fails before that modified object is persisted, even when short assignment is disabled

#### Scenario: Recycle metadata lists the reserved label
- **WHEN** current, historical, or manually crafted cleanup metadata lists the reserved key
- **THEN** recycle preserves the existing short-ID label

### Requirement: Final assignment failure semantics
The system MUST fail the overall claim or clone and use its existing cleanup path when final identity refresh, callback, conflict retry, context handling, or persistence fails, and MUST NOT emit a success response before final identity is persisted.

#### Scenario: Final callback or update fails
- **WHEN** the final metadata stage returns an error after readiness work has completed
- **THEN** the operation fails through existing cleanup and does not return a partially successful Sandbox result

#### Scenario: Final callback makes no change
- **WHEN** the final callback reports `changed=false`
- **THEN** the returned Sandbox is refreshed from the direct read and no Update is issued

### Requirement: Opaque and unambiguous cache lookup
The claimed-Sandbox cache SHALL index exactly one resolved ID per Sandbox, treat client-provided IDs as opaque, and fail closed when more than one Sandbox matches an ID.

#### Scenario: Label update reaches the cache
- **WHEN** an informer observes a Sandbox transition from unlabeled to a non-empty short-ID label
- **THEN** the cache moves the entry from the legacy key to the short key without retaining both aliases

#### Scenario: Duplicate ID is indexed
- **WHEN** claimed-Sandbox lookup finds multiple objects for the same opaque ID
- **THEN** it returns an ambiguity error without selecting the first object or parsing the ID for fallback lookup

### Requirement: Shared atomic route projection
Manager and gateway SHALL use the same ObjectKey-, UID-, resourceVersion-, and SandboxID-aware routing semantics while maintaining separate physical stores, and an accepted ID transition SHALL replace the old route with the new route atomically.

#### Scenario: Legacy route transitions to short
- **WHEN** a full Route with a strictly newer resourceVersion changes the same UID from its legacy ID to its persisted short ID
- **THEN** one Store transaction removes the legacy ID and activates the short ID so a single snapshot never contains both

#### Scenario: Route is logged
- **WHEN** any shared Route is formatted for logs
- **THEN** its access token is rendered as `***`

### Requirement: Version-fenced peer compatibility and deletion
The routing system MUST accept constrained legacy ID-only peer records during disabled rollout, MUST require complete identity fields for full records, and MUST prevent stale, mismatched, or lower-authority updates and deletes from replacing or removing current full ownership.

#### Scenario: Old peer sends an ID-only record
- **WHEN** a well-formed Route has both namespace and name absent and includes non-empty ID, UID, and resourceVersion
- **THEN** it follows only the compatibility ID-only state machine and never invents an ObjectKey by parsing the ID

#### Scenario: Peer sends a partial or malformed record
- **WHEN** exactly one ObjectKey field is present, ID, UID, or resourceVersion is missing, or resourceVersion is not a well-formed positive integer
- **THEN** the peer endpoint returns `400 Bad Request` without Store mutation

#### Scenario: Stale or dominated peer event arrives
- **WHEN** a well-formed event is older, identity-mismatched, or dominated by current full ownership
- **THEN** it is an idempotent no-op and the peer endpoint returns `204 No Content`

#### Scenario: Old incarnation deletes a new one
- **WHEN** an update or delete for an old UID or old resourceVersion arrives after a newer incarnation owns the ObjectKey
- **THEN** fencing prevents that event from deleting the current route or reviving a retired legacy alias

### Requirement: Collision quarantine and targeted repair
The system MUST make duplicate full Sandbox IDs unroutable instead of using last-write-wins and SHALL repair known ambiguous ObjectKeys asynchronously through bounded direct Gets guarded by the affected record generation.

#### Scenario: Different ObjectKeys claim the same ID
- **WHEN** two full records claim one Sandbox ID
- **THEN** the ID is quarantined, successful lookup fails closed, each claimant is queued for repair, and the peer endpoint returns `409 Conflict`

#### Scenario: Ambiguous event requires authoritative repair
- **WHEN** equal resourceVersions leave a known ObjectKey identity ambiguous
- **THEN** the event adapter enqueues a deduplicated repair request and completes without blocking on an API read

#### Scenario: Repair result becomes stale
- **WHEN** the affected record generation advances while a direct Get is in flight
- **THEN** the stale observation is ignored and cannot overwrite newer Store state

#### Scenario: Repair discovers no object
- **WHEN** a generation-matched direct Get returns NotFound, deletion, or exclusion by the component predicate
- **THEN** repair applies an authoritative ObjectKey deletion

#### Scenario: Repair population scope
- **WHEN** normal route recovery or repair runs
- **THEN** it uses informer synchronization and targeted ObjectKey Gets and never issues a periodic direct API-server List of all Sandboxes

### Requirement: Authorized E2B resource diagnostics
E2B SHALL add protected namespace/name context to successful metadata and downstream errors only after Sandbox lookup and ownership authorization succeed, and MUST NOT disclose that context in not-found or unauthorized responses.

#### Scenario: Authorized successful response exposes metadata
- **WHEN** an authorized E2B response exposes Sandbox metadata
- **THEN** it adds `e2b.agents.kruise.io/sandbox-resource: <namespace>/<name>` after filtering ordinary metadata

#### Scenario: User attempts to spoof protected metadata
- **WHEN** a label extension supplies the Sandbox-ID key or the response-only resource key
- **THEN** E2B rejects the input before either key can be persisted or override generated response context

#### Scenario: Authorized downstream operation fails
- **WHEN** lookup and ownership authorization succeeded before a runtime, gateway, checkpoint, or lifecycle failure
- **THEN** the error retains its classification and appends `sandboxResource=<namespace>/<name>`

#### Scenario: Lookup or authorization fails
- **WHEN** the Sandbox is not found or ownership authorization fails
- **THEN** the response does not disclose namespace or name

### Requirement: Point-in-time Checkpoint and opaque pagination identity
Checkpoint creation SHALL persist the non-empty final Sandbox ID supplied by manager core at creation time, and pagination SHALL use the resolved ID as an opaque uniqueness component without parsing or historical rewriting.

#### Scenario: Sandbox transitions after a Checkpoint
- **WHEN** an unlabeled recycled Sandbox receives a short ID after an earlier Checkpoint was created
- **THEN** the earlier Checkpoint retains its legacy source ID and later Checkpoints use the short ID

#### Scenario: Empty Checkpoint identity is supplied
- **WHEN** infra receives an empty SandboxID for Checkpoint creation
- **THEN** it rejects persistence

#### Scenario: ID changes between list calls
- **WHEN** a Sandbox transitions between paginated list requests
- **THEN** pagination accepts the mutable opaque key behavior and does not retain a second identity

### Requirement: Staged activation and rollback boundary
Operators MUST roll out label-aware manager and gateway binaries with assignment disabled and MUST satisfy compatibility-drain, cache, collision, and repair health gates before enabling new short-ID assignment.

#### Scenario: Initial binary rollout
- **WHEN** no Sandbox already carries a short-ID label and assignment remains disabled
- **THEN** manager and gateway may roll out in either order while new receivers constrain old ID-only messages

#### Scenario: Activation readiness is incomplete
- **WHEN** any old replica remains, the bounded peer drain window has not elapsed, an ID-only record or unresolved collision remains, or a repair queue is not drained
- **THEN** operators do not enable short-ID assignment

#### Scenario: Assignment has occurred and the flag is disabled
- **WHEN** at least one short label has been persisted and operators turn the feature flag off
- **THEN** new assignments stop but existing labels remain authoritative and rolling back to label-unaware binaries remains unsafe

### Requirement: Bounded identity observability
The implementation SHALL report legacy resolution, assignment success/failure, collision, invalid route mutations, route compatibility, and targeted-repair queue health with bounded metric labels that exclude namespace, name, UID, and Sandbox ID.

#### Scenario: Identity event is measured
- **WHEN** legacy resolution, assignment, collision, invalid routing, route compatibility, or targeted repair backlog produces an observable result
- **THEN** aggregate metrics use fixed dimensions and omit resource identity from metric labels while normal event and retry details remain in structured logs

#### Scenario: Internal diagnostic is logged
- **WHEN** assignment, collision, or repair requires resource-specific diagnosis
- **THEN** structured logs may include namespace and name while successful assignment remains debug-level
