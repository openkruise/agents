---
title: Sandbox Template Build and E2B Compatibility
authors:
  - "@ywExcellent"
reviewers:
  - "@zmberg"
  - "@furykerry"
creation-date: 2026-09-16
last-updated: 2026-11-19
status: provisional
see-also:
  - "/docs/proposals/20260123-sandbox-template.md"
---

# Sandbox Template Build and E2B Compatibility

## Table of Contents

- [Summary](#summary)
- [Motivation and Scope](#motivation-and-scope)
- [Architecture](#architecture)
- [Resource Model](#resource-model)
- [E2B Protocol](#e2b-protocol)
- [Build Execution](#build-execution)
- [Capture and Consumption](#capture-and-consumption)
- [Cleanup](#cleanup)
- [Security and Deployment](#security-and-deployment)
- [Upgrade Strategy](#upgrade-strategy)
- [Test Plan](#test-plan)

## Summary

1. A metadata-only `SandboxTemplateClaim` owns the public template name. Its name is the namespace-scoped `templateID`, written as T.
2. A Claim-owned `SandboxTemplate` stores one Build definition and result. Its generated name is the public `buildID`, written as B: `st-` followed by 20 lowercase alphanumeric characters. Kubernetes UIDs remain internal identities.
3. E2B reservation creates or reuses Claim T and allocates a fresh Version B with one placeholder Build Sandbox. File uploads land in that Sandbox's `/blobs` emptyDir through the gateway. Submission completes the reserved Version in place.
4. The independent template controller changes only the business-container image in place, retaining the Pod and emptyDir, executes steps once, and captures a Checkpoint. A result-unknown interruption fails instead of replaying commands.
5. Ready builds are consumed through existing delivery flows: Checkpoint restoration for captured builds and inline cold start for static definitions. Provisioning is never repeated during instance creation.

## Motivation and Scope

SandboxTemplate also serves existing SandboxSet revision materialization and Checkpoint restoration. Those subordinate templates keep their existing owners and are not Build Versions. The build controller manages only Claim-owned Versions.

### Goals

1. Support the community E2B Template reserve, upload, submit, status, existence, list, details, delete, and instance-creation flow.
2. Persist namespace-scoped names, immutable submitted definitions, original resource identities, and verified artifact references.
3. Accept a single-container image with folded RUN, USER, WORKDIR, ENV, and COPY instructions, without building or publishing an OCI image.
4. Keep upload payloads off the Manager data path and require no separate blob-storage service.
5. Run accepted provisioning before Ready and restore its effects without re-execution.

### Deferred Capabilities

1. `from_template`, private image-registry credentials, `minFreeDiskMb`, start/ready commands, non-default tags, and team-qualified names are not supported.
2. Build logs remain empty sequences; durable logs and pagination are deferred.
3. Custom Template APIs, warm pools, companion SandboxSets, explicit rebuild APIs, tag management, and historical Build selection are deferred. Claim ownership does not impose a single-Version limit.
4. Image-acceleration execution, its Builder resource, and provider integration are deferred. Existing acceleration input remains best-effort intent and is not a Ready prerequisite.
5. The backend vocabulary for this build entry is `k8s`. Existing backend identifiers remain unchanged; other build backends and routing are not introduced.
6. No upload sealing, upload-completion callback, or submit/upload mutual exclusion is added. Upload misuse within the caller's own Build does not introduce another coordination subsystem.
7. E2B Build memory and process-state capture are deferred. Independent Checkpoints retain their existing explicit capture scopes.

## Architecture

```text
Community E2B SDK
  reserve -> file probes and uploads -> submit -> status -> Sandbox.create
       |            |                    |         |          |
       v            v                    v         v          v
 E2B API       sandbox-gateway        E2B API    E2B API     E2B API
       |       signed PUT -> POST         |         |          |
       v          multipart              v         v          v
 Manager: reservation / submission / catalog / delivery orchestration
       |
       v
 Infra: Claim and Version storage / Build Sandbox allocation / delivery
       |
       v
 Claim T -> Version B -> Build Sandbox B -> Pod B + /blobs emptyDir
                |
                v
 Independent SandboxTemplate controller
   UID-bound worker -> image convergence -> serial provisioning
                |
                v
 Version-owned Checkpoint (accepted workload remains in Version)
                |
                v
 Version status: Ready + verified artifact
```

| Component | Responsibility |
| --- | --- |
| E2B API | Authentication, namespace derivation, HTTP models and errors, request validation, DSL folding, signed-upload URL formatting. |
| Manager | Backend-neutral reservation/submission/catalog rules and use-case orchestration; existing quota and delivery policy. |
| Infra | Kubernetes-backed Sandbox and Template capabilities, informer reads, conditional writes, runtime initialization, and existing claim/clone operations. |
| sandbox-gateway | Verify the Build upload capability and stream PUT archives to the unmodified runtime's POST multipart endpoint. |
| SandboxTemplate controller | Independent Build lifecycle, execution workers, capture, status, and cleanup. Its dependency closure excludes Manager and API packages. |
| Sandbox controller | Pod creation, runtime injection, in-place updates, and normal Sandbox lifecycle. |
| Checkpoint provider | Capture and restore the declared business-container contents through the Checkpoint resource contract. |

The build controller orchestrates Checkpoint resources; it does not implement the capture backend. Captured Builds require a separately installed Checkpoint provider. A missing provider cannot publish Ready and is bounded by the total Build budget.

## Resource Model

### Identity and Ownership

| Identity or resource | Meaning |
| --- | --- |
| Claim name T | Public templateID within the caller's namespace. |
| Version name B | Public buildID, generated independently for each reservation, not derived from Claim UID. |
| Claim UID / Version UID | Internal fences against deletion and same-name replacement. |
| Build Sandbox name B | One private execution Sandbox, controlled by Version UID. |
| Build Sandbox UID | Persisted after allocation; never replaced on a cache miss. |
| Pod UID | Persisted when upload/runtime readiness is observed; changing it interrupts the Build. |
| Execution container ID | Persisted before provisioning and checked through capture; a restarted container cannot supply a successful Build artifact. |
| Checkpoint name | Kubernetes artifact resource anchor for validation and cleanup. |
| Delivered Sandbox name | Template instances default to `sbx-{B without leading st-}-{5-character apiserver suffix}`. Explicit clone names or prefixes retain precedence. |
| Checkpoint backend ID | The restore lookup key, resolved from successful Checkpoint status. |

1. T cannot collide with a SandboxSet or Checkpoint name in the same namespace. Reservation and SandboxSet admission check both directions. Ambiguous names are rejected rather than resolved by implicit precedence.
2. The name `tags` is reserved. Names may carry only the `:default` suffix. T and B are not Kubernetes UIDs.
3. Each Version belongs to one Claim through its controller OwnerReference. A Claim can own multiple independently named Versions and stores no single Build allocation. Recreating T after complete deletion allocates new UIDs; an old Build handle cannot follow the new Claim.
4. Subordinate SandboxSet and ordinary Snapshot templates do not acquire a Claim owner and are not reclaimed as Draft Builds. Template Builds no longer create a separate Checkpoint-owned recovery template.
5. Existing Version names remain unchanged and usable for lookup and cleanup. New generation uses `st-` names without adding a second public-ID mapping.

### Claim and Version Types

```go
type SandboxTemplateClaim struct {
    metav1.TypeMeta
    metav1.ObjectMeta
}

type SandboxTemplateSpec struct {
    Backend              string
    BuildOptions         *SandboxTemplateBuildOptions
    Template             *corev1.PodTemplateSpec
    VolumeClaimTemplates []corev1.PersistentVolumeClaim
    PersistentContents   []string
    Runtimes             []RuntimeConfig
    PauseStrategy        *PauseStrategy
    Probes               []Probe
    AutoPausePolicy      *AutoPausePolicy
    StartupMode          StartupMode
    Lifecycle            *SandboxLifecycle
}

type SandboxTemplateBuildOptions struct {
    FromTemplate      string
    Steps             []TemplateBuildStep
    StartCommand      *TemplateBuildCommand
    ReadyCommand      *TemplateBuildCommand
    Checkpoint        bool
    ImageAcceleration bool
}

type TemplateBuildStep struct {
    Command *TemplateBuildCommand
    Copy    *TemplateBuildCopy
}

type TemplateBuildCommand struct {
    Command string
    User    string
    Cwd     string
    Env     map[string]string
}

type TemplateBuildCopy struct {
    Src         string
    Dest        string
    User        string
    Mode        *int32
    FilesHash   string
    ForceUpload bool
}

type SandboxTemplateStatus struct {
    ObservedGeneration int64
    Phase              SandboxTemplateBuildPhase
    Artifacts          []SandboxTemplateArtifact
    Conditions         []metav1.Condition
}

type SandboxTemplateArtifact struct {
    Type  string
    Name  string
    Value string
}
```

1. Existing workload, runtime, pause, probe, startup, and lifecycle fields remain available to their existing materialization flows. The E2B DSL produces a business-container definition; Version admission adds no separate container-count restriction.
2. Exactly one of `command` or `copy` is set per step. Command `cwd` is empty or absolute; COPY `src` is context-relative and `dest` is absolute.
3. `Mode` preserves absent permissions versus explicit zero. Wire COPY permissions are octal strings; the CR stores their integer value, so `"0755"` becomes 493 and `"0000"` becomes 0.
4. Deferred fields remain represented in the existing model but do not authorize their execution. Build admission rejects unsupported fromTemplate/start/ready behavior.
5. Backend defaults to `k8s` and is immutable. A submitted Version's definition, ownership, and request fingerprint cannot change. Published artifact references are immutable.

### Reservation Metadata

| Metadata | Purpose |
| --- | --- |
| Version name and Spec | BuildID, reserved CPU/memory envelope, and runtime declarations. |
| Build Sandbox Spec | Configured placeholder image reference, without tag resolution. |
| Claim Version-identity annotation | Version UID, Sandbox UID, Pod UID, execution container ID, and deletion intent. |
| Version request-fingerprint annotation | Canonical submitted-input identity for idempotent retries. |
| Claim cleanup annotations and finalizer | Block new work and retain identities until owned-resource cleanup converges. |

For Claim-owned Versions, no container image denotes Draft; setting an image denotes submitted input, whose Pod template must pass normal Kubernetes validation. Neither submission detection nor admission imposes a single-container limit. Draft stores reserved resources and runtimes directly. Admission checks the owning Claim's name and UID, preserves each named container's resources and the runtime envelope during one-time completion, and then freezes Spec. Images are written with the accepted definition in one patch; no separate submission annotation is required. Status remains the controller's asynchronous execution result.

The annotations belong to the owning use cases and controller. They are not additional public protocol parameters. Conditional writes fence informer views before allocation, submission, execution, consumption, and deletion.

### Draft and Build States

| State | Behavior |
| --- | --- |
| Unsubmitted Draft | Contains equal positive CPU/memory requests and limits, reserved runtimes, no business image, and an owner Claim. Expires after one hour without deleting the Claim or sibling Versions. |
| Pending | No execution has started. Draft and submitted definitions are distinguished by the business image, not by this phase. |
| Building | Accepted work is executing or capture is pending. |
| Ready | Every declared operation completed, and any required artifact and accepted Version definition were verified. |
| Failed | Terminal failure, confirmed nonzero exit, timeout, identity loss, or an unknown in-flight execution result. Commands are not replayed. |
| Cleanup marked | Not a new phase. New uploads, submissions, queries, and consumption are blocked while finalizer cleanup proceeds. |

1. Reservation failure returns an error without a successful Build response. A new reservation allocates a fresh B; requests are not deduplicated by Claim name, and a missing Version is not recovered.
2. Submission resolves the image to a digest, records a canonical recipe fingerprint, and finalizes the same Version. An identical retry succeeds; a different input returns 409.
3. Nonempty provisioning steps force capture with `persistentContents: [filesystem]`. Files produced by provisioning are restored without rerunning steps; in-memory state and background processes started by RUN are not restored. A caller cannot publish provisioning effects as a static image-only definition.
4. No-step Builds may become Ready as static definitions without a snapshot. Their instances cold-start from the submitted image.

## E2B Protocol

### Route Inventory

Native and customized E2B entries use the same handlers and models. The customized entry retains its existing `/kruise/api` prefix and Sandbox-address adaptation.

| Operation | HTTP route | Successful response |
| --- | --- | --- |
| Reserve | POST `/v3/templates` | 202 with T/B and metadata. |
| Probe COPY input | GET `/templates/{T}/files/{hash}` | 201 with `present: false` and signed `url`. |
| Submit | POST `/v2/templates/{T}/builds/{B}` | Empty 202. |
| Build status | GET `/templates/{T}/builds/{B}/status` | 200 with status, T/B, empty logs, and a sanitized failure reason when applicable. |
| Alias/existence | GET `/templates/aliases/{alias}` | 200 for a live Claim or 404 when absent/deleting; existence does not imply Ready. |
| List | GET `/v2/templates` | Existing SandboxSet entries plus one projected entry per Claim. |
| Details | GET `/templates/{T}` | Existing pool details or the Claim's Ready Version projection. |
| Delete | DELETE `/templates/{T}` | Existing pool/checkpoint semantics or acceptance of Claim cleanup intent. |
| Create instance | POST `/sandboxes` | 201 after existing delivery, runtime initialization, and request-level configuration. |

### Reserve

```http
POST /v3/templates
X-API-Key: <api-key>
Content-Type: application/json

{"name":"python-agent","cpuCount":1,"memoryMB":1024}
```

```json
{
  "templateID": "python-agent",
  "buildID": "st-abcdefghijklmnopqrst",
  "public": false,
  "names": ["python-agent"],
  "tags": ["default"],
  "aliases": []
}
```

| Input | Contract |
| --- | --- |
| name | A supported namespace-scoped name. Legacy alias-only input is not accepted by this build entry. |
| cpuCount | Defaults to 1; an explicit value must be a positive whole CPU count. |
| memoryMB | Defaults to 1024; an explicit value must be positive whole MiB. |
| tags | Only the default tag vocabulary is accepted. |
| teamID | No caller-selected namespace or cross-team addressing. |
| minFreeDiskMb | Unsupported. |

1. Manager creates or reads Claim T, generates a fresh random B, creates Draft B with the requested resources and runtimes, then creates its Version-owned Build Sandbox. Claim reconciliation does not create or recreate Versions.
2. Reservation returns 202 with T/B only after these creation steps succeed. A failure or lost response may leave a Draft for expiry cleanup; a new request does not resume that reservation.
3. The Build Sandbox stores the configured placeholder image reference without resolving tags and uses `/blobs` emptyDir, declared runtime injection, and disabled service-account-token automount. It does not enter normal claim, user-delivery, CSI, quota-projection, or recycle flows. Build Sandboxes are not user Sandbox list entries.

### COPY Upload

```json
{"present":false,"url":"<signed-build-sandbox-files-address>"}
```

1. `filesHash` is exactly 64 lowercase hexadecimal characters and is an opaque SDK key. It is not recomputed from the body or compared with an ETag, MD5, or content digest.
2. Each archive is stored at `/blobs/{filesHash}.tar.gz` in this Build's emptyDir. No cache hit across Builds is claimed; probes return `present: false`.
3. A grant is valid for five minutes and is bound to the original Sandbox UID, Pod UID, exact blob path, and expiry. The URL exposes neither raw UIDs nor the runtime access token.
4. The gateway accepts only the signed Build PUT on the runtime files endpoint. It verifies the capability before injecting runtime authentication and Basic root identity, then streams POST multipart to unmodified envd.
5. Multipart prefix/suffix handling covers headers-only bodies, chunks, and trailers without buffering the complete archive. Non-identity HTTP Content-Encoding is rejected. Ordinary requests keep their existing behavior.
6. The SDK completes its PUT before submitting. A previously issued grant may remain usable until expiry after submission; the caller is responsible for its own Build's concurrent writes. No upload sealing or completion callback is required.
7. `force` and `forceUpload` are accepted no-ops and do not create a separate blob-publication state machine.

### Submit and DSL Folding

```http
POST /v2/templates/python-agent/builds/st-abcdefghijklmnopqrst
X-API-Key: <api-key>
Content-Type: application/json

{
  "fromImage": "example.com/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "steps": [
    {"type":"WORKDIR","args":["/app"]},
    {"type":"ENV","args":["APP_MODE","build"]},
    {"type":"COPY","args":["src/","./","root","0755"],"filesHash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
    {"type":"RUN","args":["python -m pip install numpy"]}
  ]
}
```

| Input or instruction | Behavior |
| --- | --- |
| fromImage | Required nonempty image; pin its digest before finalization. The business image must remain running. |
| fromTemplate / fromImageRegistry | Unsupported. |
| WORKDIR | Update the absolute working-directory cursor. |
| USER | Update the execution user cursor. |
| ENV | Update the environment cursor; expansion counts against the total recipe budget. |
| RUN | Append one command with the current user/cwd/env. |
| COPY | Append context-relative source, resolved absolute destination, effective user, optional octal mode, and filesHash. Preserve destination trailing-slash semantics. |
| Unknown instructions / startCmd / readyCmd | Reject instead of accepting work that will not run. |

1. Requests and expanded recipes are bounded by the existing one-MiB template-input limit. Invalid names, keys, argument shapes, path escapes, or unsupported private headers return 400.
2. `x-e2b-kruise-runtime` names an additional runtime; the reserved runtime envelope cannot change at submission and always includes agent-runtime.
3. `x-e2b-kruise-image-acceleration` accepts a boolean intent only. Acceleration work is deferred and cannot delay Ready.
4. `force` does not request replay or bypass submitted-input immutability.

### Query and Error Behavior

| Situation | Behavior |
| --- | --- |
| Missing or deleting T/B | Template query returns 404. A deleted Version is not recreated; an old B never resolves to a replacement Claim. |
| Draft / Building | Build status reports waiting/building, not Ready. |
| Ready | Status reports ready. Details use the existing required E2B fields and resource projection. |
| Failed | Status reports error with a sanitized reason and confirmed failed-step index where available. |
| No Ready Version during creation | Same create-path not-found response as a missing template. |
| Ambiguous resource name / changed submitted input | 409. |
| Bad protocol input or unsupported workload projection | 400. |
| Temporary allocation, cache, runtime, or image-resolution failure | Classified unavailable response rather than fabricated success. |

`logs` and `logEntries` remain `[]`. BuildInfo carries T/B, name/alias, and default tags, not CR status, artifact identifiers, or runtime credentials. List/details retain existing pool projections; Claim details require the supported equal, single-container whole-CPU/whole-MiB envelope. Spawn counters are not introduced.

## Build Execution

### Worker and Interruption Contract

1. The controller registers a leader-gated Runnable. Each active worker is keyed by Version UID and derives its cancellation context from the manager lifecycle; reconcile never waits for a RUN command to finish.
2. The worker persists a Running execution condition before any workload side effect. Reconcile observing that marker without a live worker fails the Build as Interrupted instead of starting another worker.
3. Claim cleanup, Version deletion/replacement, leadership loss, and shutdown cancel the worker. Cleanup waits for the local worker to stop before removing the execution Sandbox.
4. The total execution-and-capture budget starts once and defaults to 30 minutes. It includes image readiness, runtime initialization, commands, and capture; restart or requeue does not refresh it.
5. Only idempotent identity, resource, and status operations retry on optimistic conflicts. Process Start is invoked once per step. A missing terminal event or an uncertain transport result is Interrupted; a confirmed nonzero exit or terminal process error is ExecutionFailed, even when the process error is also returned through the RPC error value.

### Same-Pod Image Transition

```text
Version definition finalized
  -> observe original Sandbox and Pod identities
  -> persist original Pod UID
  -> patch business image + capture scope on the same Sandbox
  -> wait for expected Sandbox revision and actual Pod in-place completion
  -> verify changed container identity and bind the execution container ID
  -> initialize runtime
  -> execute steps serially
  -> persist provisioned result
  -> capture
```

1. The placeholder is a controlled, non-exiting runtime-capable image. The business image must also remain running; no deferred-start wrapper or Pod-recreation strategy is added.
2. Only the business image is changed. Pod UID and the `/blobs` emptyDir must remain unchanged. A replacement Pod invalidates uploads and interrupts execution.
3. Sandbox Ready and a new Pod spec image are insufficient by themselves. The controller also checks the actual Pod template revision, in-place update state, business-container Running/Ready state, and container identity before beginning provisioning.
4. A terminal Pod/container or a subsequent container restart is detected even if Sandbox Ready has not yet converged. Capture must use the same execution container that ran the steps.

### RUN and COPY

1. RUN uses the existing agent-runtime process service with explicit user/cwd/env and a server/client timeout bounded by the remaining budget. Steps execute serially and are not retried.
2. Captured stdout/stderr memory is bounded to 8192 bytes per operation; raw command results are not written into CR conditions or debug result logs.
3. COPY executes inside the Build Sandbox as root. It extracts the archive into a unique temporary directory, applies the effective ownership/mode, and materializes its contents at the resolved destination. No archive is unpacked on a controller host.
4. The source prefix for SDK globs is the static context path before the first glob segment; the SDK archive already selects the uploaded members.
5. File and symlink targets follow trailing-slash semantics. Symlinks receive `chown -h` and no chmod. Directories merge through tar with destination-directory-symlink preservation and existing-directory metadata retention.
6. Shell values are quoted as data. Explicit mode 0000 is preserved. Temporary extraction data is cleaned up on success or failure, including read-only source modes.

## Capture and Consumption

### Checkpoint Publication

1. Capture creates a deterministic Version-owned Checkpoint named `{B}-checkpoint`, with Sandbox/Pod B, business container `runtime`, `keepRunning: true`, and the declared persistent contents.
2. The accepted Version is the restore-definition record. Template capture creates no additional SandboxTemplate, and template consumption reads the validated Version directly.
3. The Version supplies business workload and runtime declarations. Build-only `/blobs` staging and `RestartPolicyNever` are not copied into delivered instances; Checkpoint initialization metadata and existing delta/resume records remain intact.
4. Ready requires the original execution Pod/container, matching capture scope and initialization metadata, the accepted Version, current observed generation, Checkpoint Succeeded, and a nonempty backend ID. A failed or invalid capture is terminally Failed.

```yaml
status:
  phase: Ready
  observedGeneration: 2
  artifacts:
    - type: checkpoint
      name: st-abcdefghijklmnopqrst-checkpoint
      value: backend-checkpoint-id
```

`name` locates the Version-owned Checkpoint for validation and cleanup. `value` is passed to the existing clone lookup indexed by `status.checkpointId`. Neither identifier is an OCI image.

### Instance Creation

| Resolved input | Delivery |
| --- | --- |
| Existing SandboxSet | Existing pool claim or existing on-demand creation policy. |
| Claim with Ready captured Version | Checkpoint clone using artifact value and the accepted Version workload; validate Version and artifact ownership before materialization. |
| Claim with Ready static Version | Inline cold-start Sandbox from the Version workload, without requiring or creating a companion pool. |
| Existing legacy checkpoint ID | Existing clone behavior using its Checkpoint-owned recovery template when no pool/Claim entry resolves the name. |
| Claim without Ready Version or under cleanup | Reject; never fall back to the original image of a captured Build. |

1. Request-level initialization, environment variables, identity, dynamic mounts, resource overrides, and quota/admission remain in their existing delivery flows.
2. A restored Build receives request initialization and a fresh runtime access token when initialization is enabled. The frozen build recipe is not rerun.
3. Delivered Sandboxes are not controlled by the Build Version and do not carry a live TemplateRef. They contain the frozen inline workload or checkpoint restore identifier.
4. Existing SandboxSet `templateRef` admission consumes a managed Version as static when Status is Ready at the current observed generation and has no artifacts. Readiness and capture completeness belong to the controller's publication contract; consumers do not repeat submission or provisioning checks. Deleting and captured Versions cannot be cold-started as static images.
5. Both static and captured Template instances default to `sbx-{B without leading st-}-{5-character suffix}`, with the suffix generated by the apiserver. Ordinary Snapshot clone naming and explicit clone naming extensions are unchanged.

## Cleanup

1. DELETE marks the Claim and retains its finalizer before destructive work. Acceptance does not imply all Kubernetes resources have already disappeared.
2. The Claim marker blocks new reservations, submissions, and consumption. Draft expiry cleans only that Version and its owned resources, retaining the Claim and sibling Versions; successful submission cancels stale Draft expiry.
3. Reference checks include external SandboxSets targeting Versions or existing Checkpoint-owned recovery templates, and Sandboxes whose restore identifiers still depend on the artifact. A blocked request clears its marker and returns a conflict without deleting referenced resources.
4. Cleanup cancels UID-bound workers and waits for them to stop, then removes owned Build Sandboxes, owned Checkpoints and any existing owned recovery records, Versions, and the Claim binding. UID/resource-version preconditions protect replacements.
5. Successful Builds release their temporary Build Sandbox while retaining the Ready Version, Claim, and Checkpoint. Failed Builds release the execution Sandbox and unreferenced partial captures while retaining their failure record.
6. Original identities and deletion intent remain recorded through cleanup retries. A missing Version is never recreated, and a missing cached Sandbox is never replaced under the old Build handle. An empty Claim is retained until explicitly deleted.
7. Normal route discovery observes execution-Sandbox deletion; issued upload grants have bounded validity and cannot authorize a replacement Pod or Sandbox. No separate upload-revocation callback is required.

## Security and Deployment

### Isolation

1. Authenticate requests and derive the namespace from the API key. Administrators use the configured template namespace rather than supplying arbitrary namespace input.
2. Build Sandboxes do not enter user Sandbox list/quota projections and receive no control-plane service-account token. The independent controller retains its deployment's ordinary isolation and runtime transport.
3. The gateway fails closed for Build routes, even when ordinary traffic authentication is disabled. Capability verification precedes runtime credential injection; signatures and injected credentials are not logged.
4. Runtime TLS declarations must be honored by configured client material. A declared TLS runtime is not silently downgraded to plaintext.
5. Archive processing is confined to the guest and its execution budget. The deployment must provide the normal Sandbox filesystem/ephemeral-storage boundaries and capture backend. Linux bash/GNU tar COPY behavior is a deployment prerequisite.

### Required Configuration

| Component | Configuration or prerequisite |
| --- | --- |
| sandbox-manager | `--template-build-placeholder-image` accepts a platform image reference, either a tag or a digest. Reservation stores the configured reference without tag resolution. |
| agent-sandbox-controller | `--leader-elect`; Build execution requires an elected controller lifecycle. |
| agent-sandbox-controller | `--template-build-timeout`, default 30m, covering execution and capture together. |
| Runtime clients | Configure client TLS material when the deployed runtime advertises TLS. |
| sandbox-gateway | Load the Build-upload filter and retain its signed-route verification and streaming transformation. |
| Runtime | Existing agent-runtime injection, envd initialization, process service, and multipart file upload. No envd fork is required. |
| Checkpoint provider | A working filesystem capture/restore implementation for E2B Builds. Independent memory Checkpoints retain their existing backend requirements. Missing backend readiness cannot produce a Ready Build. |

The controller needs informer-backed Template/Claim/Sandbox/Pod/Checkpoint reads, conditional Claim and Sandbox writes, Version status writes, and owned-resource create/delete permissions. Generated RBAC is checked alongside the source markers. No APIReader List or new unapproved APIReader Get path is added.

## Upgrade Strategy

1. Install the Claim CRD and generated model/schema before enabling reservation. Claim-less legacy templates keep their existing ownership and behavior.
2. The previous draft Build contract was not exposed to users. Install the command/COPY union schema and required artifact backend value before exposing Build operations; no old Build-data migration or compatibility branch is introduced.
3. Deploy Manager, gateway, runtime configuration, and controller as one compatible build flow. A protocol-only controller must not mark unexecuted provisioning Ready. Register the Claim defaulting webhook that adds the cleanup finalizer only together with its cleanup controller.
4. After a restart, a persisted in-flight execution marker fails rather than replaying. A known provisioned result may continue idempotent Checkpoint-resource reconciliation against its recorded identities.
5. Rollback does not reinterpret public B as a UID or reuse a replaced Sandbox/Pod. Deferred capabilities do not require compatibility shims in the Build execution path.

## Test Plan

| Area | Required coverage |
| --- | --- |
| Identity and reservation | T/B shape, envelope defaults/bounds, namespace/name conflicts, independent Versions under one Claim, failed or ambiguous reservation without replay, owner UID checks, Draft-only expiry, no missing-Version recreation, and finalization. |
| Protocol | Actual route registration, empty 202 submission, required response fields, native/customized entries, unsupported inputs, folded-size budget, default tags. |
| Upload | Signed UID/Pod/path/expiry scope, runtime/TLS initialization, no credential leakage, headers/chunks/trailers, first-byte streaming, ordinary-request regression. |
| Worker lifecycle | Reconcile remains nonblocking; one worker per Version UID; serial RUN; shutdown/leadership/cleanup cancellation; unknown-result failure and no replay. |
| Image convergence | Same Pod and emptyDir, actual in-place completion, stale Ready/spec combinations, terminal containers, new container IDs, identity loss. |
| COPY | File rename and directory merge, trailing slash, symlink ownership, explicit zero mode, quoting, glob prefixes, missing/invalid input, cleanup on failure. |
| Capture | Business-container scope, no extra Template-build recovery object, Version integrity, initialization metadata, original execution container, failed capture, missing backend ID, stable total budget. |
| Consumption and cleanup | Artifact-value clone, static cold start without a pool, Version-based restore definition, `st-` Build IDs and Build-derived `sbx-` instance names, fresh delivery environment/token, Ready/deletion gates, reference blocking including existing recovery records, retained artifacts and temporary-Sandbox cleanup. |
| Regression and deployment | Narrow unit/race runs; generated-file and dependency-closure checks; real gateway/runtime/backend SDK flow before declaring the build feature verified. |

Local process/HTTP tests validate orchestration and transport behavior, not backend snapshot contents. Linux filesystem COPY tests and a real SDK build/upload/capture/restore run must demonstrate the guest behavior and retained provisioning effects. Component and workload images are approved before the actual-environment run.
