---
title: Sandbox Template Build and E2B Compatibility
authors:
  - "@ywExcellent"
reviewers:
  - "@zmberg"
  - "@furykerry"
creation-date: 2026-09-16
last-updated: 2026-11-23
status: provisional
see-also:
  - "/docs/proposals/20260123-sandbox-template.md"
---

# Sandbox Template Build and E2B Compatibility

This document is long because it is complete, not because it is
complicated. Read [What a User Does](#what-a-user-does) first. It tells
the whole story in plain words. Keep [Words Used in This
Document](#words-used-in-this-document) beside you: every hard word is
defined there once. After the story, every section starts with two plain
sentences that say what it is about, and only then gives the exact rules.
Every rule is one short sentence.

## Table of Contents

- [What a User Does](#what-a-user-does)
- [Words Used in This Document](#words-used-in-this-document)
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

## What a User Does

Alice writes agent code. She wants every one of her sandboxes to start
with numpy already installed and her files already in place. She does not
want to build, push, or publish a container image. Here is everything she
does:

1. **Reserve.** Alice asks for a new build of a template named
   `python-agent`. The system creates a fresh record for the build and a
   fresh work sandbox for it. Alice gets two IDs back: the templateID
   `python-agent` (the name she chose) and a random buildID such as
   `st-abcdefghijklmnopqrst`. If she wants, she can also give this
   attempt a tag, for example `v1.1`.
2. **Upload.** Alice packs her project files into one `tar.gz` and uploads
   it. The files land in a scratch folder inside the work sandbox. The
   upload goes through the gateway directly to the sandbox. It never
   passes through sandbox-manager.
3. **Submit.** Alice submits the recipe: one base image plus a short list
   of steps (`WORKDIR`, `ENV`, `COPY`, `RUN`).
4. **Build.** A controller swaps the placeholder image for Alice's image
   in that same sandbox, runs each step exactly once, then saves a
   checkpoint of the resulting filesystem. Alice polls a status endpoint
   until it says `ready`. If Alice switched on image acceleration, a
   provider warms the image in the background; that never delays the
   build.
5. **Use.** Alice creates sandboxes from the template. Each sandbox is
   restored from the checkpoint, so it starts with everything the steps
   produced. If the recipe had no steps at all, the sandbox simply
   cold-starts from the submitted image. Either way, no step ever runs
   again. She can ask by name (the newest successful attempt), by tag
   (`python-agent:v1.1`), or pinned to one buildID.

One way to picture it: the template name is the name of a dish, and each
build is one attempt to cook that dish. Many attempts can run at the same
time. Attempts never overwrite each other, but starting a new attempt
retires same-name attempts that were never submitted: they are marked
failed as superseded. When someone orders the dish, the kitchen serves
the newest attempt that finished successfully.

## Words Used in This Document

Every term below is used many times. Each has exactly one meaning here.

| Word | Meaning |
| --- | --- |
| Template name, **T** | The name the user chose, for example `python-agent`. This is the public templateID. |
| **buildID**, **B** | The random name of one build: `st-` followed by 20 lowercase letters and digits, for example `st-abcdefghijklmnopqrst`. |
| **Version** | One `SandboxTemplate` custom resource. It stores one build: the definition and the result. Its `metadata.name` is B. |
| **Build Sandbox** | The private work sandbox of one Version. Uploads land here and steps run here. Users never see it in their sandbox lists. |
| **Draft** | A reserved Version that has not been submitted yet. Drafts expire after the build timeout, 30 minutes by default. |
| **Superseded** | Marked failed because a newer reservation of the same name arrived. Only unsubmitted Builds can be superseded. The newest unsubmitted Build is the active one. |
| **Steps** | The build instructions: `RUN`, `COPY`, `ENV`, `USER`, `WORKDIR`. |
| **Checkpoint** | A saved copy of the Build Sandbox's filesystem, taken after the steps finish. |
| **Ready** | The build succeeded and can be used to create sandboxes. |
| **Placeholder image** | A small, controlled, never-exiting image that starts the Build Sandbox before the real image is known. |
| **`/blobs`** | The scratch folder (an emptyDir) inside the Build Sandbox where uploads are stored. |
| **Group** | All Versions in one namespace that carry the same template-name label. There is no group object; the label is the group. |
| **Tag** | A movable pointer to one Build, addressed as `name:tag`. It resolves to the latest-created Ready holder. `default` is the reserved tag meaning "no tag". |
| **ImageAccelerationBuilder** | A cluster-scoped resource that asks a provider to accelerate one image. Best-effort and asynchronous; it never blocks Ready. |
| **Lease** | A small Kubernetes object that records which controller replica currently runs a build. If the holder stops renewing for three minutes, another replica may take over — only to fail the build, never to rerun it. |

## Summary

1. The public templateID is the template name T. T is not a Kubernetes
   resource name, and no binding object exists. Versions of T are found
   by the immutable label `LabelTemplateName`
   (`e2b.agents.kruise.io/template-name`) within one namespace.
2. Every build is one Version. Its random `metadata.name` is the public
   buildID B: `st-` plus 20 lowercase alphanumeric characters. The E2B
   templateID is T. Kubernetes UIDs stay internal.
3. One reservation creates one fresh Version B carrying label T, plus one
   Version-owned placeholder Build Sandbox. Concurrent reservations for
   the same namespace and T succeed as independent Versions. The newest
   reservation supersedes the name's unsubmitted Builds.
4. A reservation may declare free-form tags; `name:tag` addresses them
   later. File uploads land in the active Build's `/blobs` emptyDir
   through the gateway. Submission completes the reserved Version in
   place.
5. The build controller swaps only the business-container image, keeping
   the Pod and the emptyDir, runs each step exactly once, then captures a
   Checkpoint. An interruption with an unknown result fails the build.
   Steps are never replayed.
6. Ready builds are consumed through existing delivery flows: Checkpoint
   restoration for captured builds, inline cold start for static
   definitions. Provisioning is never repeated when instances are
   created.

## Motivation and Scope

SandboxTemplate already serves two existing jobs: SandboxSet revision
materialization and Checkpoint restoration. Templates doing those jobs
keep their existing owners; they are not build Versions. The build
controller recognizes a managed Version by its template-name label alone.
It does not own, read, or create any name-level resource.

### Goals

1. Support the community E2B Template flow end to end: reserve, upload,
   submit, status, existence, list, details, delete, and instance
   creation.
2. Persist, per namespace: the template label, the frozen submitted
   definition, the original resource identities, and verified artifact
   references.
3. Accept one single-container image plus folded `RUN`, `USER`, `WORKDIR`,
   `ENV`, and `COPY` instructions. No OCI image is built or published.
4. Keep upload payloads off the sandbox-manager data path. No separate
   blob-storage service is required.
5. Run the accepted provisioning before Ready, and restore its effects
   later without executing anything again.

### Not in This Proposal (Deferred)

1. `from_template`, private image-registry credentials, `minFreeDiskMb`,
   start and ready commands, and team-qualified names.
2. Build logs. They stay empty sequences; durable logs and pagination come
   later.
3. Custom Template APIs, warm pools, companion SandboxSets, explicit
   rebuild APIs, and picking historical Builds. A namespace-scoped group
   may freely hold multiple independent Versions meanwhile.
4. Image acceleration is in scope through the shared
   `ImageAccelerationBuilder` resource and controller. Providers remain
   deployment-specific. A deployment without a provider keeps Builders
   Pending. Acceleration stays best-effort and is never a Ready
   prerequisite.
5. Other build backends. This build entry speaks the `k8s` vocabulary
   only. Existing backend identifiers are unchanged.
6. Upload sealing, upload-completion callbacks, and submit/upload mutual
   exclusion. Misusing uploads inside your own Build adds no new
   coordination subsystem.
7. E2B Build memory and process-state capture. Independent Checkpoints
   keep their existing explicit capture scopes.

## Architecture

How to read the picture: the user's SDK talks only to HTTP endpoints on
the left. Everything below Manager is Kubernetes. The controller at the
bottom does the cooking and writes the result back onto the Version.

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
 Infra: Version storage / Build Sandbox creation / delivery
       |
       v
 Namespace + template-name label T groups independent Versions
       |
       v
 Version B -> Build Sandbox B -> Pod B + /blobs emptyDir
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
| Manager | Backend-neutral reservation, submission, catalog, and delivery orchestration; existing quota and delivery policy. |
| Infra | Kubernetes-backed Sandbox and Template capabilities: informer reads, conditional writes, runtime initialization, and existing claim/clone operations. |
| sandbox-gateway | Verifies the Build upload capability and streams PUT archives to the unmodified runtime's POST multipart endpoint. |
| SandboxTemplate controller | The independent build lifecycle: execution workers, capture, status, and cleanup. It must not import Manager or API packages. |
| Sandbox controller | Pod creation, runtime injection, in-place updates, and normal Sandbox lifecycle. |
| Checkpoint provider | Captures and restores the declared business-container contents through the Checkpoint resource contract. |

The build controller orchestrates Checkpoint resources; it does not
implement the capture backend. A captured build therefore requires a
separately installed Checkpoint provider. If the provider is missing, the
build cannot publish Ready, and it still burns no more than its total
build budget.

## Resource Model

This section defines what objects exist, what names they carry, and who
owns what.

### Identity and Ownership

| Identity or resource | What it is |
| --- | --- |
| Template name T | The public templateID, grouped by caller namespace and the immutable Version label. Not a `metadata.name`. |
| Version name B | The public buildID: `st-` plus 20 lowercase alphanumeric characters, freshly generated for each reservation. |
| Version UID | Internal only. A fence against deletion and same-name replacement. |
| Build Sandbox name B | One private execution sandbox, controlled by the Version UID. |
| Build Sandbox UID | Saved on the Version after creation. Never replaced on a cache miss. |
| Pod UID | Saved when upload or runtime readiness is observed. If it changes, the build is interrupted. |
| Execution container ID | Saved before provisioning and checked again at capture. A restarted container cannot produce a valid build artifact. |
| Checkpoint name | The Kubernetes resource that anchors the capture for validation and cleanup. |
| Delivered Sandbox name | Instances default to `sbx-{B without the leading st-}-{5-character apiserver suffix}`. Explicit clone names or prefixes win. |
| Checkpoint backend ID | The restore lookup key, resolved from a successful Checkpoint status. |

- **Name collisions are rejected.** T must not collide with a SandboxSet
  or Checkpoint name in the same namespace. Reservation and SandboxSet
  admission check both directions against the namespace-scoped Version
  labels. A collision is never resolved by implicit precedence.
- **Reserved names.** The name `tags` is reserved. A template name may
  carry at most one `:tag` suffix. T and B are not Kubernetes UIDs.
- **Every Version is independent.** It owns its Build Sandbox and its
  Checkpoint through its UID. Group membership comes only from the
  immutable template-name label. There is no group resource, no
  allocation record, and no historical name ledger.
- **Neighbors stay untouched.** Subordinate SandboxSet and ordinary
  Snapshot templates keep their existing owners and are never reclaimed
  as Draft builds. The template-name label is not copied onto Checkpoints
  or ordinary recovery templates. A template build creates no separate
  Checkpoint-owned recovery template.
- **T exists through its Versions.** An empty Version set leaves no
  independent name binding. An old B never resolves to a later Version in
  the same group.
- **Old names keep working.** Existing Version names remain unchanged for
  lookup and cleanup. New generation uses `st-` names. No second
  public-ID mapping is added.

### Version Types

The Version is a normal Kubernetes resource. These are its fields:

```go
const LabelTemplateName = "e2b.agents.kruise.io/template-name"

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
    Checkpoint        *CheckpointStrategy
    ImageAcceleration *ImageAccelerationStrategy
}

type CheckpointStrategy struct {
    Enable bool
}

type ImageAccelerationStrategy struct {
    Enable bool
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

1. Existing workload, runtime, pause, probe, startup, and lifecycle fields
   keep serving their existing materialization flows. The E2B DSL produces
   one business-container definition. Admission adds no container-count
   restriction.
2. Each step sets exactly one of `command` or `copy`. A command `cwd` is
   empty or absolute. A COPY `src` is context-relative; a COPY `dest` is
   absolute.
3. `Mode` tells "unset" apart from "explicit zero". The wire format uses
   octal strings; the CR stores the integer value: `"0755"` becomes 493
   and `"0000"` becomes 0.
4. Deferred fields remain in the model but authorize nothing. Build
   admission rejects `fromTemplate`, start, and ready requests.
5. `Checkpoint` and `ImageAcceleration` use strategy structs with an
   `enable` field. Future options (for example capture scope or provider
   hints) extend the structs without another type change.
6. `Backend` defaults to `k8s` and is immutable. `LabelTemplateName` must
   be nonempty at reservation and is immutable from creation. After
   submission, the definition and any published artifact reference are
   immutable.

### Image Acceleration Builder

This is one new, tiny, cluster-wide resource. It says "please accelerate
this image"; it is not the provider's artifact itself.

```go
type ImageAccelerationBuilderPhase string

const (
    ImageAccelerationBuilderPending  ImageAccelerationBuilderPhase = "Pending"
    ImageAccelerationBuilderCreating ImageAccelerationBuilderPhase = "Creating"
    ImageAccelerationBuilderReady    ImageAccelerationBuilderPhase = "Ready"
    ImageAccelerationBuilderFailed   ImageAccelerationBuilderPhase = "Failed"
)

type ImageAccelerationBuilderSpec struct {
    Image string
}

type ImageAccelerationBuilderStatus struct {
    ObservedGeneration int64
    Phase              ImageAccelerationBuilderPhase
    Conditions         []metav1.Condition
}

type ImageAccelerationBuilder struct {
    metav1.TypeMeta
    metav1.ObjectMeta
    Spec   ImageAccelerationBuilderSpec
    Status ImageAccelerationBuilderStatus
}

type ImageAccelerationBuilderList struct {
    metav1.TypeMeta
    metav1.ListMeta
    Items []ImageAccelerationBuilder
}
```

1. The whole Spec is immutable. `Image` is required, 1 to 2048 characters,
   and the only field. The shared schema carries no provider-specific
   network fields or artifact identifier.
2. Status is a subresource and stays observational. `Phase` accepts only
   the four values above. `Conditions` is a map keyed by type.
3. Before creating a Builder, the controller checks its informer-backed
   image index. An existing Builder is reused whatever its phase is.
   Concurrent requests share one deterministic `iab-` name.
4. Providers attach their configuration and asynchronous resource identity
   as internal metadata. Configuration is prepared once for a new Builder.
   Provider retries reuse the same persisted inputs and idempotency token.
5. Builders carry no Version owner reference. Build cleanup removes
   neither them nor their provider artifacts. Acceleration results never
   enter Version `status.artifacts`.

| Builder phase | Meaning |
| --- | --- |
| Pending | No provider is configured. No acceleration result is claimed. |
| Creating | The provider is preparing or querying acceleration. |
| Ready | The provider confirmed that its acceleration result is ready. |
| Failed | The provider reported a terminal failure. |

### Reservation Metadata

A Draft is just a Version with reserved resources and no image yet. All
of this state lives on the Version itself; nothing is kept elsewhere.

| Metadata | What it holds |
| --- | --- |
| Version name and Spec | The buildID, the reserved CPU/memory envelope, and the runtime declarations. |
| Version `LabelTemplateName` label | The immutable T, used with the namespace for grouping and for recognizing managed Versions. |
| Version `e2b.agents.kruise.io/template-tags` annotation | The tags this Build holds, as a JSON object mapping each free-form tag to its assignment time. Written at reservation; lives outside the frozen Spec. |
| Build Sandbox Spec | The configured placeholder image reference, without tag resolution. |
| Version `e2b.agents.kruise.io/template-execution-identity` annotation | Only this Build's Sandbox UID, Pod UID, and container ID. The Version UID comes from its metadata. |
| Build execution Lease | Version-owned execution holder identity and renewal time, with a three-minute validity window. |
| Version `e2b.agents.kruise.io/template-cleanup` annotation and finalizer | The nonempty cleanup operation ID gates that Version and scopes rollback. The finalizer retains it until its worker and owned-resource cleanup converge. |

- **Draft vs. submitted.** A Version with no container image is a Draft. A
  Version with an image has been submitted, and its Pod template must pass
  normal Kubernetes validation. Neither detection nor admission imposes a
  single-container limit. A Draft stores its reserved resources and
  runtimes directly.
- **What submission does.** Admission validates the immutable
  template-name label, preserves each named container's resources and the
  runtime envelope, and freezes the Spec. The image is written together
  with the accepted definition in one patch. No separate submission
  annotation exists.
- **Who writes status.** Only the controller, as the asynchronous
  execution result.
- **Who owns the annotations.** The owning use cases and the controller.
  They are not public protocol parameters. Conditional writes fence
  informer views before resource creation, submission, execution,
  consumption, and deletion. Execution identities and cleanup intent live
  on each Version, never in a shared name record.

### Draft and Build States

| State | Plain meaning |
| --- | --- |
| Unsubmitted Draft | Positive and equal CPU/memory requests and limits, reserved runtimes, no business image, immutable label set. Expires after `--template-build-timeout` counted from `metadata.creationTimestamp`, 30 minutes by default. Expiry cleans only this Version and its owned resources, and keeps sibling Versions. |
| Pending | No execution has started. Draft and submitted are told apart by the business image, never by this phase. |
| Building | Accepted work is executing, or capture is pending. |
| Ready | Every declared operation completed, and the required artifact and the accepted Version definition were verified. |
| Failed | Terminal. A confirmed nonzero exit, a timeout, identity loss, an unknown in-flight execution result, or supersession by a newer same-name reservation. Commands are never replayed. |
| Cleanup marked | Not a new phase. While the finalizer cleanup runs, new uploads, submissions, queries, and consumption are blocked for this Version only. The label never gates the group. |

1. A failed reservation returns an error; no successful Build response is
   returned. Every reservation creates a fresh B. Requests are not
   deduplicated by namespace and T. A missing Version is not recovered.
2. **Supersession.** A newer reservation of the same name supersedes the
   name's unsubmitted Builds. The reserve use case marks each Draft
   Failed with reason `Superseded` before returning. Submitted, Building,
   Ready, and Failed Builds are never superseded. No group-wide lock is
   held. Concurrent reservations converge on the newest-created
   unsubmitted Build as the name's active Build. A superseded holder finds
   out on its next interaction: build status reports `error` with the
   Superseded reason, submission returns 409, and the existing Failed
   cleanup releases its placeholder Sandbox while issued upload grants
   expire within their bounded validity. Submission enforces the same rule
   itself: it declines an unsubmitted Build that is not the name's
   newest-created unsubmitted Build. So a superseded Draft cannot be
   finalized even if its marking write lagged.
3. Submission preserves the caller's image tag or digest without registry
   access and finalizes the same Version once. A retry whose normalized
   definition equals the accepted Spec succeeds. A different input returns
   409. Callers who need immutable image content supply a digest
   themselves.
4. A build with nonempty steps must capture, with `persistentContents:
   [filesystem]`. Files produced by the steps are restored later without
   rerunning them. In-memory state and background processes started by
   RUN are not restored. A caller cannot publish provisioning effects as
   a static image-only definition.
5. A build with no steps may become Ready as a static definition without a
   checkpoint. Its instances cold-start from the submitted image.

## E2B Protocol

This section lists every HTTP route and the exact contract of each.

### Route Inventory

Native and customized E2B entries use the same handlers and models. The
customized entry keeps its existing `/kruise/api` prefix and
Sandbox-address adaptation. Where a route accepts `{B or T}`, a
Build-shaped name in the templateID slot resolves as B first, then as T,
and the response echoes the requested templateID. The templateID may also
carry one tag, written `name:tag`; [Tags](#tags) defines how the tag
selects a Build.

| Operation | HTTP route | Successful response |
| --- | --- | --- |
| Reserve | POST `/v3/templates` | 202 with T/B and metadata. |
| Probe COPY input | GET `/templates/{T}/files/{hash}` | 201 with `present: false` and a signed `url`. |
| Submit | POST `/v2/templates/{B or T}/builds/{B}` | Empty 202. |
| Build status | GET `/templates/{B or T}/builds/{B}/status` | 200 with status, the requested templateID, B, empty logs, and a sanitized failure reason when applicable. |
| Alias/existence | GET `/templates/aliases/{alias}` | 200 for a nonempty namespace-scoped Version group, or 404 when empty. Existence does not imply Ready. |
| Assign tags | POST `/templates/tags` | 201 with the assigned tags and the target Build. |
| Remove tags | DELETE `/templates/tags` | 204 after removing the named tags from every holder. |
| List tags | GET `/templates/{T}/tags` | 200 with one entry per held tag per Build. |
| Tag groups | GET `/templates/{T}/tags/groups` | 200 with per-tag ready assignments, latest first, paginated. |
| Tag count | GET `/templates/{T}/tags/count` | 200 with the number of distinct ready tags. |
| Tag existence | GET `/templates/{T}/tags/exists` | 200 with `exists` (at least one ready assignment) and the normalized tag. |
| Tag assignments | GET `/templates/{T}/tags/{tag}/assignments` | 200 with that tag's ready assignments, latest first, keyset-paginated. |
| List | GET `/v2/templates` | Existing SandboxSet entries plus one projected entry per template-name group: the default Ready Version when available, otherwise the latest-created Build with its current status. |
| Details | GET `/templates/{T}` | Existing pool details, or the group's default Ready Version projection. |
| Delete | DELETE `/templates/{T}` | 204 after accepting cleanup of the request-time Version UID set. Referenced groups return 409 without deletion. New Builds after the selection are unaffected. |
| Create instance | POST `/sandboxes` | 201 after existing delivery, runtime initialization, and request-level configuration. |

### Tags

A tag is a movable pointer to one Build of a template name. It is not
part of the immutable build definition. The E2B SDK passes `name:tag`
through as the templateID; the backend splits it, and the name part keeps
its DNS-label rules.

| Question | Answer |
| --- | --- |
| Storage | The Version annotation `e2b.agents.kruise.io/template-tags`, a JSON object mapping each tag to its RFC3339 assignment time. |
| Charset | Free-form strings, validated and normalized at the API layer, matching the E2B `normalizedTag` contract. Kubernetes label limits never apply. |
| Indexing | One field-index entry per tag value, following the existing `spec.image` index technique. |
| Resolution | The eligible Ready Version with the latest `metadata.creationTimestamp` among that tag's holders. |
| `default` | Reserved and never stored. `name:default` is identical to `name`. A reserve request containing `default` is normalized to no tag. Wire responses always list `default` first. |

1. Reserve writes the requested tags into the annotation when the Version
   is created. Tags live outside the frozen Spec, so moving a tag later
   never touches the submitted definition.
2. **Why not a label:** label values cap at 63 characters with a
   restricted charset, and single-tag matching would need an index anyway.
   **Why not status:** status is the controller's asynchronous result,
   while tags change through user and Manager actions. **Why not a new
   resource:** it would reintroduce the name-level binding this model
   deliberately avoids.
3. Supersession and deletion need no tag-specific rules. A superseded
   Build was never Ready, so it never wins a tag resolution. Deleting a
   Build removes its tags with it.
4. Assign (`POST /templates/tags`) resolves its `target`, written
   `name:tag`, through the same resolution rule. It patches the target
   Build's annotation: each new tag is written with the current time; a
   tag the Build already holds keeps its original assignment time. Remove
   (`DELETE /templates/tags`) deletes the named tags from every holder in
   the group. Both are Manager-side annotation patches; neither touches
   the frozen Spec.
5. Assignment listings are derived, not stored. Each entry carries a
   deterministic `assignmentId` (a UUID derived from the Version UID and
   the tag), the Build ID, the assignment time, the Build's creation time,
   and its finish time when known. Only READY holders appear, newest
   first, with keyset cursor pagination. One boundary versus E2B: a
   removed or moved-away assignment leaves no tombstone event and simply
   disappears from the listings.
6. Count and existence are ready-scoped, matching the E2B definitions.
   Count returns the number of distinct ready tags. Existence asks
   whether the tag has at least one ready assignment and echoes the
   normalized tag. The list route returns one entry per held tag per
   Build, whether or not that Build is Ready.

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
| cpuCount | Defaults to 1. An explicit value must be a positive whole CPU count. |
| memoryMB | Defaults to 1024. An explicit value must be positive whole MiB. |
| tags | Free-form strings, validated and normalized at the API layer. `default` is accepted and normalized away. |
| teamID | No caller-selected namespace and no cross-team addressing. |
| minFreeDiskMb | Unsupported. |

1. Manager generates a fresh random B and creates an independent Draft in
   the caller's namespace with `LabelTemplateName: T`, the requested
   resources, runtimes, and tags. It then creates the Version-owned Build
   Sandbox. Concurrent requests for the same namespace and T create
   distinct Versions with no shared reservation record. The newest
   reservation marks the name's older unsubmitted Builds Failed with
   reason `Superseded`. Reconciliation never creates or recreates a
   Version.
2. Reservation returns 202 with T/B only after those creation steps
   succeed. A failure or a lost response may leave a Draft behind for
   expiry cleanup. A new request does not resume that reservation.
3. The Build Sandbox runs the configured placeholder image reference
   without resolving tags. It mounts the `/blobs` emptyDir, gets the
   declared runtime injection, and disables service-account-token
   automount. It never enters claim, user-delivery, CSI, quota-projection,
   or recycle flows. It is never a user Sandbox list entry.

### COPY Upload

```json
{"present":false,"url":"<signed-build-sandbox-files-address>"}
```

1. `filesHash` is exactly 64 lowercase hexadecimal characters. It is an
   opaque SDK key. It is not recomputed from the body, and it is not
   compared with an ETag, MD5, or content digest.
2. Each archive is stored at `/blobs/{filesHash}.tar.gz` in the active
   Build's emptyDir. A probe resolves T to the name's newest-created
   unsubmitted Version. No cross-Build cache hit is claimed, and probes
   return `present: false`.
3. A grant is valid for five minutes. It is bound to the original Sandbox
   UID, Pod UID, exact blob path, and expiry. The URL exposes neither raw
   UIDs nor the runtime access token.
4. The gateway accepts only the signed Build PUT on the runtime files
   endpoint. It verifies the capability before injecting runtime
   authentication and the Basic root identity. It then streams a POST
   multipart to the unmodified envd.
5. Multipart prefix and suffix handling covers headers-only bodies,
   chunks, and trailers without buffering the complete archive.
   Non-identity HTTP Content-Encoding is rejected. Ordinary requests keep
   their existing behavior.
6. The SDK completes its PUT before submitting. A previously issued grant
   may remain usable until expiry after submission; the caller owns its
   own Build's concurrent writes. No upload sealing or completion callback
   exists.
7. `force` and `forceUpload` are accepted no-ops. They create no separate
   blob-publication state machine.

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
| fromImage | Required and nonempty as a valid image reference. The caller's tag or digest is preserved without registry access. Only public, unauthenticated references are supported in this period. The business image must keep running. |
| fromTemplate | Planned future work. Requests are rejected until it is implemented. |
| fromImageRegistry | Unsupported. Private registries are not addressed through the submit request; the deferred extension path is `imagePullSecrets` on the Version Pod template. |
| WORKDIR | Updates the working-directory cursor. A relative path resolves from the cursor's previous value. |
| USER | Updates the execution user cursor. |
| ENV | Updates the environment cursor. Expansion counts against the total recipe budget. |
| RUN | Appends one command with the current user/cwd/env. |
| COPY | Appends the context-relative source, resolved absolute destination, effective user, optional octal mode, and filesHash. Destination trailing-slash semantics are preserved. |
| Unknown instructions / startCmd / readyCmd | Rejected. We never accept work that will not run. |

1. Requests and expanded recipes are bounded by the existing one-MiB
   template-input limit. Invalid names, keys, argument shapes, path
   escapes, or unsupported private headers return 400.
2. `x-e2b-kruise-runtime` names one additional runtime. The reserved
   runtime envelope cannot change at submission, and it always includes
   agent-runtime.
3. `x-e2b-kruise-image-acceleration` accepts a boolean intent only.
   Acceleration is asynchronous and never delays Ready.
4. `force` requests no replay and bypasses no submitted-input
   immutability.

### Query and Error Behavior

| Situation | Behavior |
| --- | --- |
| Empty T group, missing or deleting B, or a T/B label mismatch | Template query returns 404. Lookup checks B in the caller's namespace and its immutable label against T. A deleted Version is not recreated, and an old B never resolves to another Version. |
| Draft / Building | Build status reports waiting or building, never Ready. |
| Ready | Status reports ready. Details use the existing required E2B fields and resource projection. |
| Failed | Status reports error with a sanitized reason and, where available, the confirmed failed-step index. |
| Superseded Build | Status reports `error` with the Superseded reason. Submitting its B returns 409. Probes and uploads resolve T to the active Build. |
| No Ready Version during creation | The same create-path not-found response as a missing template. |
| Ambiguous resource name, or changed submitted input | 409. |
| Bad protocol input, or an unsupported workload projection | 400. |
| Temporary resource-creation, cache, or runtime failure | An unavailable response, never a fabricated success. |

Which build does a plain name point to? The eligible Ready Version with
the latest `metadata.creationTimestamp` — not the latest submission, not
the latest completion. Cleanup-marked or deleting Versions are ineligible.
A newer Pending or Failed build never replaces an older Ready Version. A
`name:tag` address resolves the same way, restricted to that tag's
holders.

`logs` and `logEntries` stay `[]`. BuildInfo carries T/B, name/alias, and
default tags — never CR status, artifact identifiers, or runtime
credentials. List and details keep their existing pool projections;
grouped Version details require the supported equal, single-container,
whole-CPU/whole-MiB envelope. Spawn counters are not introduced.

## Build Execution

This section is about the controller: who runs the steps, what happens
when a replica dies, and what is allowed to be retried.

### Worker and Interruption Contract

1. **Who runs the work.** Version reconciliation and the execution
   Runnable run on every controller replica. Each worker is keyed by
   Version UID and bound to its process lifecycle. Reconcile never waits
   for a RUN command to finish.
2. **The Lease.** Replicas acquire a native `coordination.k8s.io/v1`
   Lease named `template-build-{UID}`, where UID is the Version UID,
   through conditional creation or update. The Lease carries a controller
   OwnerReference to that Version with `BlockOwnerDeletion: true`. The
   holder identity is a UUID generated at each controller process startup.
3. **Authorization before side effects.** The Lease holder must also win
   the UID/resource-version compare-and-swap that first writes
   `BuildReady=False` with reason `Running` — before any remote workload
   side effect. Execution authorization is never reset for command
   replay.
4. **Renewal.** The holder renews once per minute. Three minutes without
   successful renewal expires ownership, tolerating two missed renewals.
   Read or renewal errors alone do not prove that another holder is lost.
5. **Scheduling.** Local workers and observing replicas schedule their
   next check at the earlier of Lease expiry and the total build deadline;
   they do not poll every two seconds. Version and owned-resource changes
   wake reconciliation earlier. Acquisition conflicts keep short retries.
6. **Takeover after failure.** Taking over an expired or missing Lease
   for an already-started build with an unknown execution result grants
   only failure and cleanup authority: mark Interrupted, never replay RUN.
   A known Provisioned result proceeds to capture even while its local
   worker is exiting.
7. **Cancellation.** Target Version deletion, expiry, or replacement,
   Lease ownership loss, and process shutdown cancel local work.
   Per-Version deletion gates stop subsequent remote commands. Normal
   Sandbox/Pod deletion terminates in-flight guest execution. Deleting or
   expiring one Version never cancels sibling workers or cleans their
   resources.
8. **The budget.** The total execution-and-capture budget uses
   `--template-build-timeout`, default 30 minutes. It covers image
   readiness, runtime initialization, commands, and capture. Renewal,
   restart, or requeue never refreshes it. Its start is separate from the
   Draft creation-based expiry deadline.
9. **Retries.** Only idempotent identity, resource, and status operations
   retry on optimistic conflicts, using client-go's DefaultBackoff with
   jitter through context-aware waits; cancellation stops retries. Result
   publication reloads the same Version UID and generation, so metadata
   edits never discard a known outcome, and a stale failure decision can
   never overwrite known success. Process Start runs exactly once per
   step. A missing terminal event or an uncertain transport result is
   Interrupted. A confirmed nonzero exit or terminal process error is
   ExecutionFailed, even when the process error is also returned through
   the RPC error value.

`BuildReady` is the only build-progress condition. Draft and static
definitions use Phase and ObservedGeneration and never create it. Its
first Running write fixes the execution-and-capture budget through
LastTransitionTime. Later pending stages change only the Reason, never
the Status, and never restart the timer.

| Build progress | BuildReady Status | Reason |
| --- | --- | --- |
| Execution authorized | False | Running |
| RUN completed, capture pending | False | Provisioned |
| All declared work completed | True | Provisioned or Captured |
| Terminal failure | False | InvalidDefinition, Interrupted, Timeout, ExecutionFailed, CaptureConflict, or CaptureFailed |

### Optional Image Acceleration

1. Submitted builds that request `imageAcceleration`, including Ready
   builds, ensure one `ImageAccelerationBuilder` per business image. The
   immutable definition and artifact references never change for this.
2. Acceleration is asynchronous. A preparation failure produces a warning
   and blocks neither execution, capture, nor Ready publication. Without a
   provider, the shared controller keeps the Builder Pending instead of
   publishing a fabricated result.
3. Provider setup and new-Builder metadata preparation run through
   nil-safe extension points. Provider SDKs, network acquisition,
   resource identifiers, and Pod matching annotations stay outside the
   shared model and controller.
4. Runtime and placeholder image pre-warming is deferred. No
   provider-specific cache reclamation and no shared-cache lifecycle
   management is introduced.

### Same-Pod Image Transition

The build happens in the same Pod the uploads landed in — that is the
whole trick that keeps the uploaded files.

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

1. The placeholder is a controlled, non-exiting, runtime-capable image.
   The business image must also keep running. No deferred-start wrapper
   and no Pod-recreation strategy is added.
2. Only the business image changes. The Pod UID and the `/blobs` emptyDir
   must stay unchanged. A replacement Pod invalidates uploads and
   interrupts execution.
3. Sandbox Ready plus a new Pod spec image is not enough. Before
   provisioning, the controller also checks the actual Pod template
   revision, the in-place update state, the business container's
   Running/Ready state, and the container identity. Equivalent image
   references may retain ImageID; the generic completion check accepts a
   replaced ContainerID only when the running image matches the requested
   reference.
4. A terminal Pod or container, or a later container restart, is detected
   even while Sandbox Ready has not yet converged. Capture must use the
   same execution container that ran the steps.

### RUN and COPY

1. RUN uses the existing agent-runtime process service with explicit
   user/cwd/env. The server and client timeouts are bounded by the
   remaining budget. Steps execute serially and are never retried.
2. Captured stdout and stderr are capped at 8192 bytes per operation. Raw
   command results are never written into CR conditions or debug result
   logs.
3. COPY runs inside the Build Sandbox as root. It extracts the archive
   into a unique temporary directory, applies the effective ownership and
   mode, and materializes the contents at the resolved destination. No
   archive is ever unpacked on a controller host.
4. The source prefix for SDK globs is the static context path before the
   first glob segment. The SDK archive has already selected the uploaded
   members.
5. Files and symlink targets follow trailing-slash semantics. Symlinks
   get `chown -h` and no chmod. Directories merge through tar with
   destination-directory-symlink preservation and existing-directory
   metadata retention.
6. Shell values are quoted as data. Explicit mode 0000 is preserved.
   Temporary extraction data is cleaned up on success and on failure,
   including read-only source modes.

## Capture and Consumption

This section is what happens after the steps finish: taking the photo and
serving dishes from it.

### Checkpoint Publication

1. Capture creates a deterministic Version-owned Checkpoint named
   `{B}-checkpoint`, with Sandbox and Pod B, business container `runtime`,
   `keepRunning: true`, and the declared persistent contents. Its
   controller OwnerReference targets the Version UID with
   `BlockOwnerDeletion: true`. The Checkpoint does not inherit
   `LabelTemplateName`.
2. The accepted Version is the restore-definition record. Template
   capture creates no additional SandboxTemplate, and template
   consumption reads the validated Version directly.
3. The Version supplies the business workload and runtime declarations.
   Build-only `/blobs` staging and `RestartPolicyNever` are not copied
   into delivered instances. Checkpoint initialization metadata and
   existing delta/resume records remain intact.
4. Ready requires the original execution Pod and container, matching
   capture scope and initialization metadata, the accepted Version, the
   current observed generation, Checkpoint Succeeded, and a nonempty
   backend ID. A failed or invalid capture is terminally Failed.

```yaml
status:
  phase: Ready
  observedGeneration: 2
  artifacts:
    - type: checkpoint
      name: st-abcdefghijklmnopqrst-checkpoint
      value: backend-checkpoint-id
```

`name` locates the Version-owned Checkpoint for validation and cleanup.
`value` is passed to the existing clone lookup indexed by
`status.checkpointId`. Neither identifier is an OCI image.

### Instance Creation

| Resolved input | Delivery |
| --- | --- |
| Existing SandboxSet | The existing pool claim or on-demand creation policy. |
| Group with a default Ready captured Version | Checkpoint clone using the artifact value and the accepted Version workload. The namespace/label, Version identity, and artifact ownership are validated before materialization. |
| Group with a default Ready static Version | Inline cold-start Sandbox from the Version workload. No companion pool is required or created. |
| Build ID B of a Ready Version | The same static or captured delivery, pinned to that Build. A name shaped like a Build ID resolves as B first, then as T. |
| Existing legacy checkpoint ID | Existing clone behavior through its Checkpoint-owned recovery template, when no pool or group resolves the name. |
| Group without an eligible Ready Version | Rejected. A cleanup-marked Version is never consumed, and a captured Build never falls back to its original image. |

1. Request-level initialization, environment variables, identity, dynamic
   mounts, resource overrides, and quota/admission stay in their existing
   delivery flows.
2. A restored build receives request initialization and a fresh runtime
   access token when initialization is enabled. The frozen recipe never
   reruns.
3. Delivered Sandboxes are not controlled by the Build Version and carry
   no live TemplateRef. They contain the frozen inline workload or the
   checkpoint restore identifier.
4. An existing SandboxSet `templateRef` consumes a managed Version as
   static when Status is Ready at the current observed generation and has
   no artifacts. Readiness and capture completeness belong to the
   controller's publication contract; consumers do not recheck submission
   or provisioning. Deleting and captured Versions cannot be cold-started
   as static images.
5. Static and captured instances both default to
   `sbx-{B without the leading st-}-{5-character suffix}`, with the suffix
   generated by the apiserver. Ordinary Snapshot clone naming and
   explicit clone naming extensions are unchanged.

## Cleanup

Cleanup removes one Version's kitchen — its worker, its Lease, its Build
Sandbox, its Checkpoint — without touching sibling Versions. Deletion of
a whole name is a batch of per-Version cleanups over a snapshot of the
group.

1. For each Version selected for deletion, cleanup intent is recorded and
   the finalizer retained before any destructive work. DELETE acceptance
   does not mean all Kubernetes resources are already gone.
2. Cleanup intent blocks new uploads, submissions, queries, and
   consumption for that Version only, never the whole group. Draft expiry
   first requests Version deletion with UID/resource-version
   preconditions; the finalizer keeps the Version until owned-resource
   cleanup completes. If submission wins the conflict, resources remain
   untouched. If expiry wins, the deletion timestamp prevents
   finalization. Only the target Version and its children are cleaned;
   sibling Versions are retained.
3. DELETE T first selects the existing Version UID set and marks every
   selected Version before checking references. References include
   external SandboxSets targeting Versions or existing Checkpoint-owned
   recovery templates, and Sandboxes still using the artifact. If
   references remain, the request rolls back its own markers and returns
   409 without deleting any selected Version.
4. Cleanup cancels the target Version's local worker and waits for it to
   stop, using per-Version deletion gates for remote execution. It
   requests deletion of the owned execution Lease, Build Sandbox,
   Checkpoint, and any existing owned recovery records. OwnerReferences
   and each target's UID/resource-version preconditions govern deletion;
   historical Sandbox UID values in the execution-identity annotation
   never block cleanup. Finalizer release follows a confirmed foreground
   DELETE, then an optimistic patch removes only the business cleanup
   finalizer while keeping the garbage collector's finalizer. Foreground
   cascade, not an empty informer List, provides the remaining deletion
   barrier.
5. Successful builds release their temporary Build Sandbox and keep the
   Ready Version and Checkpoint. Failed builds release the execution
   Sandbox and unreferenced partial captures and keep their failure
   record.
6. Execution identities and deletion intent stay on their owning Version
   through cleanup retries. A missing Version is never recreated, and a
   missing cached Sandbox is never replaced under the old Build handle.
   Bypassed finalizers and orphaned resources are not repaired from
   historical records. When the group's Version set becomes empty, no
   independent name binding remains.
7. Normal route discovery observes execution-sandbox deletion. Issued
   upload grants have bounded validity and cannot authorize a replacement
   Pod or Sandbox. No separate upload-revocation callback exists.

What DELETE T covers: only the selected Version UIDs, including Draft and
Building Versions. A confirmed deletion interrupts their execution without
replay. New same-name Builds reserved after the selection remain usable.
Repeated deletion is idempotent. If an individual delete fails after
other deletes were accepted, completed deletions are not rolled back, and
the remaining markers permit a retry. There is no group-wide deletion
lock. Existing SandboxSet and ordinary Checkpoint deletion behavior is
unchanged.

## Security and Deployment

### Isolation

1. Requests are authenticated, and the namespace is derived from the API
   key. Administrators use the configured template namespace instead of
   supplying arbitrary namespace input.
2. Build Sandboxes never enter user Sandbox list or quota projections and
   receive no control-plane service-account token. The independent
   controller keeps its deployment's ordinary isolation and runtime
   transport.
3. The gateway fails closed for Build routes, even when ordinary traffic
   authentication is disabled. Capability verification precedes runtime
   credential injection. Signatures and injected credentials are never
   logged.
4. Runtime TLS declarations must be honored by configured client
   material. A declared TLS runtime is never silently downgraded to
   plaintext.
5. Archive processing stays inside the guest and inside its execution
   budget. The deployment must provide the normal Sandbox
   filesystem/ephemeral-storage boundaries and the capture backend. Linux
   bash/GNU tar COPY behavior is a deployment prerequisite.

### Required Configuration

| Component | Configuration or prerequisite |
| --- | --- |
| sandbox-manager | `--template-build-placeholder-image` accepts a platform image reference, as a tag or a digest. Reservation stores the configured reference without tag resolution. Manager RBAC grants `sandboxtemplates/finalizers` update so Build Sandboxes can carry a blocking Version owner reference. |
| agent-sandbox-controller | Build Version reconciliation and execution workers run on every replica. Other controllers keep the Manager's `--leader-elect` behavior. |
| agent-sandbox-controller | `--template-build-timeout`, default 30m. Draft expiry starts at Version creation; execution and capture share a separate budget starting at the first `BuildReady=False`, reason `Running` transition. |
| Runtime clients | Configure client TLS material when the deployed runtime advertises TLS. |
| sandbox-gateway | Load the Build-upload filter and keep its signed-route verification and streaming transformation. |
| Runtime | Existing agent-runtime injection, envd initialization, process service, and multipart file upload. No envd fork is required. |
| Checkpoint provider | A working filesystem capture/restore implementation for E2B builds. Independent memory Checkpoints keep their existing backend requirements. A missing backend cannot produce a Ready build. |

The controller needs informer-backed Template/Sandbox/Pod/Checkpoint/Lease
reads, conditional Lease, Version, and Sandbox writes, Version status
writes, and owned-resource create/delete permissions. Source RBAC markers
and generated RBAC must match these resources and operations. No
APIReader List and no new unapproved APIReader Get path is added.

## Upgrade Strategy

1. Install the Version schema and the immutable `LabelTemplateName`
   validation before enabling reservation. Existing SandboxSet and
   Checkpoint-owned templates keep their owners and behavior without this
   label.
2. The previous draft Build contract was never exposed to users. Install
   the command/COPY union schema and the required artifact backend value
   before exposing Build operations. No old Build-data migration and no
   compatibility branch is introduced.
3. Deploy Manager, gateway, runtime configuration, and controller as one
   compatible build flow. A protocol-only controller must never mark
   unexecuted provisioning Ready. Register Version finalizer defaulting
   for label-identified Versions only together with its cleanup
   controller; creation must retain that cleanup finalizer.
4. After a restart, an active execution Lease is observed, not mistaken
   for a local worker. Once ownership expires, an in-flight build with an
   unknown execution result fails rather than replaying. A known
   provisioned result may continue idempotent Checkpoint-resource
   reconciliation against its recorded identities.
5. Rollback never reinterprets a public B as a UID and never reuses a
   replaced Sandbox or Pod. Deferred capabilities need no compatibility
   shims in the build execution path.
6. Install the shared Builder CRD before enabling its provider. A
   deployment migrating from another acceleration implementation performs
   that migration separately: this controller neither observes nor
   converts pre-existing resources, and existing provider artifacts must
   not be deleted as a side effect.

## Test Plan

Local process and HTTP tests validate orchestration and transport
behavior, not backend snapshot contents. Linux filesystem COPY tests and
a real SDK build/upload/capture/restore run must demonstrate the guest
behavior and the retained provisioning effects. Component and workload
images are approved before the actual-environment run.

### Identity and reservation

- T and B shape; envelope defaults and bounds.
- Namespace and name conflicts, both directions.
- Concurrent independent Versions under the same namespace and label.
- Supersession of unsubmitted Builds by newer same-name reservations, with
  submitted/Ready Builds unaffected.
- Label immutability and managed-Version recognition.
- No label propagation to Checkpoints or ordinary recovery templates.
- Failed or ambiguous reservation without replay.
- Version and owned-resource UID checks.
- Draft-only expiry; empty-group existence; no missing-Version recreation;
  finalization.

### Protocol

- Actual route registration; empty 202 submission; required response
  fields; native and customized entries.
- Namespace/label grouping and T/B matching.
- Build-shaped templateID slot resolution on `{B or T}` routes.
- Free-form tag normalization and `name:tag` latest-created-Ready
  resolution; default equivalence.
- Assign and remove tag routes patching the annotation.
- Ready-scoped count and existence.
- Keyset-paginated assignments with deterministic assignment IDs and no
  tombstones.
- Existence without Ready; unsupported inputs; folded-size budget.

### Upload

- Signed UID/Pod/path/expiry scope.
- Runtime and TLS initialization; no credential leakage.
- Headers, chunks, and trailers; first-byte streaming.
- Ordinary-request regression.

### Worker lifecycle

- Nonblocking all-replica reconcile and Runnable.
- Version-owned Lease with a process-UUID holder.
- CAS of the first Running condition before remote side effects.
- One worker per Version UID; minute renewal and three-minute ownership
  loss; serial RUN.
- Shutdown and target-Version deletion cancellation without sibling
  cancellation.
- Capture of a known Provisioned result while the worker exits.
- Unknown-result failure and no replay.

### Image convergence

- Same Pod and emptyDir; actual in-place completion.
- Equivalent image references retaining ImageID with verified ContainerID
  replacement.
- Stale Ready/spec combinations; terminal containers; new container IDs;
  identity loss.

### COPY

- File rename and directory merge; trailing slash; symlink ownership.
- Explicit zero mode; quoting; glob prefixes.
- Missing or invalid input; cleanup on failure.

### Capture

- Business-container scope; no extra Template-build recovery object.
- Version integrity; initialization metadata; original execution
  container.
- Failed capture; missing backend ID.
- Checkpoint/Lease blocking owner references.
- BuildReady reason/status transitions.
- Stable first-Running LastTransitionTime budget, separate from the
  Draft creation-based expiry.

### Consumption and cleanup

- Artifact-value clone; static cold start without a pool; Version-based
  restore definition.
- Latest-created Ready selection despite newer Pending/Failed builds or a
  different completion order.
- `st-` build IDs and build-derived `sbx-` instance names.
- Fresh delivery environment and token.
- Per-Version Ready/deletion gates and UID/resource-version/finalizer
  safety.
- Submission-versus-Draft-expiry conflicts.
- Reference blocking, including existing recovery records.
- Cleanup by owner and target identity rather than historical
  SandboxUID.
- Confirmed foreground DELETE before optimistic business-finalizer
  release, retaining the GC finalizer.
- Empty informer List not proving deletion.
- Retained artifacts and temporary-Sandbox cleanup.

### Image acceleration

- Shared Builder schema and cluster scope; creation without a provider
  stays Pending.
- Informer deduplication; fixed provider metadata; unknown-result retry;
  terminal failure.
- No Template Ready dependency.
- Provider readiness and actual Pod cache hits are verified separately.

### Regression and deployment

- Narrow unit and race runs.
- Generated-file and dependency-closure checks.
- A real gateway/runtime/backend SDK flow before declaring the build
  feature verified.

Group deletion tests must prove snapshot UID scope, reference rollback,
partial-delete retry, and survival of subsequently reserved same-name
Builds.
