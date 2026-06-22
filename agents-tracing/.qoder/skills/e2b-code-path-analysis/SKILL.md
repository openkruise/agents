---
name: e2b-code-path-analysis
description: Analyze the code paths of an E2B-compatible API scenario in pkg/servers/e2b to produce solid input for E2E test coverage. Verifies E2B Python SDK semantics via context7, reads the handler implementation, brainstorms concurrency/parameter/environment scenarios with the user, enumerates every execution path with its trigger conditions, scores importance and risk, and writes a Markdown analysis doc. Use when the user names an E2B usage scenario (create/pause/resume/connect/snapshot/timeout/list/delete sandbox, list/get/delete template, api-keys, etc.) and wants code-path analysis, E2E coverage planning, or robustness assessment of sandbox-manager E2B handlers; or says "分析 E2B 调用路径", "E2B 代码路径分析", "E2E 覆盖分析".
---

# E2B Code Path Analysis

## Overview

This skill turns a single E2B usage scenario (e.g. "create sandbox", "pause sandbox") into a rigorous,
written **code-path analysis** that downstream E2E coverage work can build on directly.

It is an **interactive, multi-phase analysis workflow**, not a one-shot report generator. You verify the
real E2B contract, read the real implementation, expand the scenario space *with the user*, enumerate every
execution path and its trigger conditions, score each path, and only then write the document.

Three principles drive everything:

1. **Verify before you reason.** Never describe E2B behavior from memory. Fetch the current E2B Python SDK
   docs via context7 and confirm with the user (see [[feedback_verify_e2b_spec]]).
2. **Trust only sandbox-manager.** Treat every other component (API Server, scheduler, controllers,
   gateway, informer cache, kubelet/CSI, cloud infra) — *and all user input* — as unreliable and adversarial.
3. **Confirm the scenario set before analyzing.** Path analysis is only as good as the scenario list. Expand
   broadly, then STOP and get the user to confirm before doing the heavy path enumeration.

## When to Use

Trigger when the user:

- Names an E2B usage scenario and wants its code paths analyzed for robustness / risk.
- Wants to plan or prioritize **E2E coverage** for an E2B-compatible endpoint.
- Asks to assess what could break a sandbox-manager E2B handler under concurrency, bad input, or a degraded cluster.
- Says "分析 E2B 调用路径" / "E2B 代码路径分析" / "E2E 覆盖分析" / "code path analysis".

## When NOT to Use

- The user wants the analysis for a non-E2B server (`pkg/proxy`, `pkg/sandbox-gateway`, controllers). The
  trust-boundary framing still helps, but the endpoint→handler map and context7 verification step do not apply.
- The user only wants a quick code read of one handler with no path enumeration / scoring / doc output. Just read it.
- The user wants you to *write or run* E2E tests. This skill produces the analysis input; it does not write tests.
  (E2E tests live under `test/`; never run them. Handler unit tests are the `*_test.go` siblings — read them as
  evidence of existing coverage, do not run the full suite.)

## Core Principles (hard rules, no exceptions)

| # | Rule | Why / how |
|---|------|-----------|
| 1 | **Verify E2B semantics via context7 first** | Training data drifts from the live E2B SDK. Resolve the E2B library and query the exact Python method(s) for the scenario; confirm with the user before reading code. |
| 2 | **Distrust everything outside sandbox-manager** | API Server latency/conflicts/watch lag, scheduler delay/failure, gateway misrouting/replica skew, controller reconcile lag/crash, stale informer cache, kubelet/CSI/cloud faults — every external dependency is a failure injection point in your analysis. |
| 3 | **Distrust user input** | Any parameter, any combination, any count, any order. Enumerate malformed / boundary / hostile inputs explicitly; never assume the SDK is the only caller. |
| 4 | **Confirm the scenario set before path enumeration** | After Phase 3 brainstorming, STOP and get explicit user confirmation. Do not jump to Phase 4 on your own. |
| 5 | **Always analyze concurrency at two levels** | (a) same replica (in-process shared state, locks, caches, expectations, TOCTOU) and (b) cross-replica (only K8s is shared truth; per-replica routing tables, memberlist gossip lag, conflicting CR writes). sandbox-manager is multi-replica. |
| 6 | **Read the real code, current routes** | Routes and file layout drift (recent refactors moved files). Re-read `pkg/servers/e2b/routes.go` for the live endpoint→handler map, then locate each handler by grep — do not trust this doc's snapshot table blindly. |
| 7 | **Score every path** | Each enumerated path gets an Importance level and a Risk level with explicit reasoning. Unscored paths are not done. |

### Trust Boundary (the mental model)

```
                 ┌─────────────────────────── TRUSTED ───────────────────────────┐
   user / SDK ──▶ │  sandbox-manager replica (handler logic, in-process state)     │
   (UNTRUSTED)    └───────┬───────────────────────────────────────────────┬───────┘
                          │ everything below is UNTRUSTED                  │
        ┌─────────────────┼──────────────────┬──────────────┬─────────────┼───────────────┐
        ▼                 ▼                  ▼              ▼             ▼               ▼
   API Server        scheduler          controllers      gateway     informer cache   peers/memberlist
   (latency,         (slow/failed       (reconcile lag,  (misroute,  (staleness,      (gossip lag,
   conflict, 5xx)     scheduling)        crash, restart)  replica     read-after-write  split view)
                                                          skew)        gap)
                          ▼
                 other sandbox-manager replicas (NOT in-process shared; only K8s + gossip)
```

When analyzing a path, for every arrow crossing the boundary ask: *what if it is slow / fails / returns stale /
returns a conflict / returns a different replica?* Each answer is a candidate execution path.

## Workflow

```
E2B scenario (from user)
   │
   ▼
[1] Verify E2B semantics via context7  ──▶ confirm Python API + HTTP endpoint with user
   │
   ▼
[2] Locate implementation              ──▶ routes.go → handler → read handler + deps
   │
   ▼
[3] Brainstorm scenario space          ──▶ 6 dimensions → STOP, confirm scenario list with user
   │
   ▼
[4] Enumerate execution paths          ──▶ single-request / same-replica / cross-replica / side-effects / delayed
   │
   ▼
[5] Score importance + risk            ──▶ per-path Impact level + Risk level + E2E priority
   │
   ▼
[6] Write Markdown doc                 ──▶ default docs/analysis/<scenario>.md
```

---

## Phase 1 — Verify E2B Semantics (context7)

Do this **before** reading any Go code.

1. Resolve the E2B library: `resolve-library-id` with `libraryName: "E2B"` (and try `"e2b code interpreter"` /
   the E2B Python SDK if the first match is thin).
2. `query-docs` for the exact scenario, e.g. "create sandbox", "pause/resume sandbox", "set sandbox timeout",
   "connect to running sandbox", "create snapshot". Capture:
   - The Python method(s) and their signature / parameters / defaults / which are required.
   - The documented behavior, return shape, and any documented error conditions.
   - The underlying HTTP request the SDK issues (method + path), so you can map to `routes.go`.
3. If context7 is missing detail, fall back to fetching `https://e2b.dev/docs` for the relevant page — still do
   **not** answer from memory.
4. **Present what you found to the user and confirm** the scenario, the exact Python API, and the HTTP endpoint
   before continuing.

Record the context7 library id and the doc source — they go in the output doc's metadata.

## Phase 2 — Locate the Implementation

1. Read `pkg/servers/e2b/routes.go` and find the route whose method+path matches the HTTP request from Phase 1.
   The route maps to a handler method on `*Controller` (registered for both the native path and the
   `adapters.CustomPrefix + "/api"` path — note both are live).
2. Note the middleware chain. `CheckApiKey` runs on every sandbox route and, when a `{sandboxID}` is present,
   resolves ownership via `sc.manager.GetOwnerOfSandbox(sandboxID)` — a cross-replica routing concern.
   API-key routes add `CheckCreateAPIKeyPermission` / `CheckDeleteAPIKeyPermission`.
3. Locate the handler implementation by grepping for the method name (e.g. `func (sc *Controller) PauseSandbox`).
   See the Quick Reference table for the likely file, but confirm by grep.
4. Read the handler **and its key dependencies**: the manager/infra calls it makes, models it decodes, adapters
   it uses, and any shared state it touches. Read the sibling `*_test.go` to see what is already covered.

## Phase 3 — Brainstorm the Scenario Space (interactive)

Expand the single scenario across **all** of these dimensions. For each, propose concrete cases:

1. **Single-endpoint concurrency** — same endpoint called N times concurrently, same or different params
   (e.g. 50 concurrent `create`; double `pause` on the same sandbox; `resume` racing `resume`).
2. **Multi-endpoint concurrency** — any combination/order/count across endpoints
   (e.g. `pause` racing `delete`; `connect` during `resume`; `timeout` during `pause`).
3. **Parameter combinations** — hostile/boundary/malformed input: missing required fields, oversized values,
   wrong types, duplicate IDs, unknown template, cross-team IDs, injection-y strings. Never trust the SDK to
   be the only caller.
4. **Cluster/environment variation** — distrust every external component: API Server latency/conflict/5xx,
   thousands of existing sandboxes, slow/failed scheduling, gateway misrouting or replica skew, controller
   reconcile lag or crash/restart, stale informer cache, node/CSI/cloud faults.
5. **Side effects from *other* endpoints** — endpoints not in this scenario that, when called concurrently,
   change shared state and affect this path (e.g. a `DeleteSandbox` invalidating an in-flight `connect`;
   TTL/timeout expiry firing mid-request; template deletion during create).
6. **Delayed / timing sequences** — `create` then immediate `connect`; `connect` then idle 5 min then
   `command run`; `pause` then `resume` after hibernation; request arriving after timeout expiry; read
   immediately after a write (informer/gossip propagation gap).

Present the expanded list grouped by dimension. **STOP and ask the user to confirm, trim, or extend** the
scenario set. Do not proceed to Phase 4 until the user confirms.

## Phase 4 — Enumerate Execution Paths

For each confirmed scenario, walk the handler branch-by-branch. Treat every `if` / early return / error
return / external call outcome as a potential distinct path. Organize paths into five buckets:

### 4.1 Single-request paths
Every branch a single request can take. For each path record: **trigger condition**, **external deps crossed**,
**result / side effect** (status code, CR mutation, in-memory mutation), and **recoverability** (auto-retry?
controller self-heals? manual ops? unrecoverable?).

### 4.2 Same-replica concurrent paths
Identify in-process **shared mutable state**: routing/owner tables, expectations (`pkg/utils/expectations`),
caches, per-controller maps, locks. Look for **TOCTOU** (check-then-act) windows and idempotency gaps. Ask:
two concurrent requests on the same replica — does the second corrupt/duplicate/lose the first's effect?

### 4.3 Cross-replica concurrent paths
Only K8s (and memberlist gossip) is shared; in-process state is **not**. Ask: requests landing on different
replicas — conflicting CR writes (resourceVersion conflict), owner-table divergence, gateway routing to a
replica that does not yet know the sandbox, read-after-write across replicas. sandbox-manager is multi-replica;
treat split state as the default, not the exception.

### 4.4 Side-effect paths from other endpoints
Concurrent calls to endpoints outside the scenario that mutate shared state and alter this path's outcome.

### 4.5 Delayed / timing paths
Paths gated on elapsed time or propagation lag: TTL/timeout expiry, hibernation transitions, informer/gossip
read-after-write gaps, stale-cache reads.

Give each path a stable ID (e.g. `P1`, `C2`, `X3`) so scoring and the doc can reference it.

## Phase 5 — Score Importance and Risk

Score **every** path on two orthogonal axes.

### Importance (Impact) — consequence severity, adjusted for recoverability

| Level | Meaning |
|-------|---------|
| **P0 — Critical** | Crashes a replica (panic/OOM/goroutine leak), loses traffic (misrouted/dropped requests), loses data (corrupt snapshot/checkpoint, wrongly deleted CR), or renders a sandbox permanently unusable. Online Agent service is damaged and cannot self-recover. |
| **P1 — Severe** | Transient sandbox unavailability, constrained runtime (timeout/resource), or wrong authz/cross-tenant access — but recoverable via retry, controller eventual consistency, or ops intervention. |
| **P2 — Moderate** | Inaccurate status codes, missing observability, non-critical degradation. User-visible but core availability intact. |
| **P3 — Minor** | Edge-parameter handling, log/metric blemishes. Negligible business impact. |

**Recoverability rule:** if a path self-heals via automatic retry or controller reconcile, drop it one level.

### Risk (Fragility) — probability of causing a future production incident

| Level | Meaning |
|-------|---------|
| **High** | (a) Relies on an implicit contract future code changes can easily break (E2B-compat fields, response shape, state-machine ordering); **or** (b) sensitive to environment/infra change (API Server latency, scheduler, gateway, controllers, informer cache, cloud storage/network); **or** (c) triggered by ops actions (rolling release, config/feature-gate change, replica scale up/down). |
| **Medium** | Has a contract but with test/validation guardrails, or is environment-sensitive but protected by retry/timeout. |
| **Low** | Purely internal, idempotent, no external dependency, well-validated input. |

### E2E coverage priority matrix

| Importance \ Risk | High | Medium | Low |
|-------------------|------|--------|-----|
| **P0 / P1** | **Must cover first** | Cover | Cover |
| **P2** | Cover | Nice to have | Defer |
| **P3** | Nice to have | Defer | Defer |

## Phase 6 — Write the Markdown Document

Output one document per analyzed scenario. Default directory: **`docs/analysis/`** (create it if absent);
honor any directory the user specifies. Suggested filename: `e2b-<scenario-slug>.md` (e.g.
`e2b-pause-sandbox.md`). Use the Write tool — never `sed`/`cat`/redirection.

### Document template

```markdown
# E2B Code Path Analysis: <Scenario>

## Metadata
- Scenario:
- E2B Python API (verified via context7):
- HTTP endpoint / route:
- Handler / implementation file:
- sandbox-manager commit:
- Analyst / date:

## 1. E2B Contract (verified)
- Python call example:
- Parameters (defaults / required):
- Expected behavior & return:
- Documented errors:
- Source (context7 library id / e2b.dev URL):

## 2. Implementation Overview
- Request flow: middleware chain → handler main path
- External components crossed (outside trust boundary):
- Shared mutable state (in-process / cross-replica):

## 3. Confirmed Scenario Set
| # | Scenario | Dimension | Notes |
|---|----------|-----------|-------|

## 4. Execution Paths
### 4.1 Single-request
| Path ID | Trigger condition | Deps crossed | Result / side effect | Recoverability |
|---------|-------------------|--------------|----------------------|----------------|
### 4.2 Same-replica concurrent
### 4.3 Cross-replica concurrent
### 4.4 Side effects from other endpoints
### 4.5 Delayed / timing

## 5. Scoring & Risk
| Path ID | Importance | Risk | Consequence | E2E priority | Rationale |
|---------|-----------|------|-------------|--------------|-----------|

## 6. E2E Coverage Recommendations
- High-priority cases to write:
- Failure-injection points needed (which external dep to perturb):
- Existing coverage (handler *_test.go / under test/):
- Coverage gaps:

## 7. Open Questions / Follow-ups
```

After writing, summarize the highest-priority paths to the user and point to the file.

---

## Quick Reference: E2B endpoint → handler map

Verified from `routes.go` at time of writing. **Re-read `routes.go` and grep the handler** to confirm; the file
column is a hint, not authority (handlers have moved between files in recent refactors).

| E2B scenario | HTTP | Path | Handler (`*Controller`) | Likely file |
|--------------|------|------|-------------------------|-------------|
| Create sandbox | POST | `/sandboxes` | `CreateSandbox` | `create.go` |
| List sandboxes | GET | `/v2/sandboxes` | `ListSandboxes` | `list.go` |
| Describe sandbox | GET | `/sandboxes/{id}` | `DescribeSandbox` | `sandbox.go` / `core.go` |
| Delete (kill) sandbox | DELETE | `/sandboxes/{id}` | `DeleteSandbox` | `sandbox.go` / `core.go` |
| Pause sandbox | POST | `/sandboxes/{id}/pause` | `PauseSandbox` | `pause_resume.go` |
| Resume sandbox | POST | `/sandboxes/{id}/resume` | `ResumeSandbox` | `pause_resume.go` |
| Connect sandbox | POST | `/sandboxes/{id}/connect` | `ConnectSandbox` | `sandbox.go` / `core.go` |
| Set timeout | POST | `/sandboxes/{id}/timeout` | `SetSandboxTimeout` | `timeout.go` |
| Create snapshot | POST | `/sandboxes/{id}/snapshots` | `CreateSnapshot` | `snapshot.go` |
| List snapshots | GET | `/snapshots` | `ListSnapshots` | `snapshot.go` / `list.go` |
| List templates | GET | `/templates` | `ListTemplates` | `templates.go` |
| Get template | GET | `/templates/{id}` | `GetTemplate` | `templates.go` |
| Delete template | DELETE | `/templates/{id}` | `DeleteTemplate` | `templates.go` |
| Browser version | GET | `/browser/{id}/json/version` | `BrowserUse` | grep |
| Debug | GET | `/debug` | `Debug` | grep |
| List teams | GET | `/teams` | `ListTeams` | `api_key.go` |
| List API keys | GET | `/api-keys` | `ListAPIKeys` | `api_key.go` |
| Create API key | POST | `/api-keys` | `CreateAPIKey` | `api_key.go` |
| Delete API key | DELETE | `/api-keys/{id}` | `DeleteAPIKey` | `api_key.go` |

Every route is registered twice (native + `<CustomPrefix>/api` prefix). API-key routes only register when
`sc.keyCfg != nil`.

## Red Flags — STOP if you catch yourself doing these

- "I know how E2B `create` behaves" → No. Verify via context7 first (rule 1).
- "I'll just analyze the obvious happy path" → No. Enumerate every branch + the 5 concurrency/timing buckets.
- "sandbox-manager is single-replica, skip cross-replica" → No. It is multi-replica; cross-replica is required.
- "The gateway/scheduler/API Server will behave correctly" → No. Everything outside sandbox-manager is untrusted.
- "The SDK validates input, so params are safe" → No. Distrust all input; the SDK is not the only caller.
- "I'll jump straight to writing paths" → No. Confirm the scenario set with the user first (rule 4).
- "I'll score later / leave a few paths unscored" → No. Every path gets Importance + Risk.

## Common Mistakes

- **Reasoning about E2B from memory** instead of context7 → wrong contract, wrong endpoint mapping.
- **Trusting the snapshot route table** instead of re-reading `routes.go` → analyzing a moved/renamed handler.
- **Only analyzing single-request paths** → missing the concurrency and timing bugs E2E most needs to catch.
- **Treating external components as reliable** → missing exactly the env/ops-driven incidents that hit production.
- **Skipping user confirmation of the scenario set** → analyzing the wrong scenarios, wasted depth.
- **Importance without recoverability adjustment** → over-rating retryable paths.
- **Conflating Importance and Risk** → they are orthogonal; a P1 path can be Low risk and vice versa.
- **Running E2E tests or the full unit suite** → read tests as evidence; never run `test/`, keep unit scope tight.
- **Compound Bash (`&&` / `>` / `xargs`) or `sed -i`** → see global CLAUDE.md Bash rules; use Write/Edit and split calls.
```
