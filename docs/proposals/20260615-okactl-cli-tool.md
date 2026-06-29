---
title: okactl CLI Tool
authors:
  - "@mahe"
creation-date: 2026-06-02
last-updated: 2026-06-16
status: implementable
---

# okactl CLI Tool

## Table of Contents

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals/Future Work](#non-goalsfuture-work)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
  - [Implementation Details](#implementation-details)
    - [Architecture](#architecture)
    - [Command Design](#command-design)
    - [Directory Structure](#directory-structure)
    - [Key Design Decisions](#key-design-decisions)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Alternatives](#alternatives)
- [Test Plan](#test-plan)
- [Implementation History](#implementation-history)

## Summary

`okactl` is a kubectl-style CLI tool for OpenKruise Agents that provides simplified operational
commands for managing sandbox resources. It wraps complex Kubernetes API operations behind
user-friendly commands, targeting operators who may not be familiar with Kubernetes internals.

Five commands are provided:

```bash
okactl scale sandboxset <name> --replicas=N        # Scale SandboxSet replicas
okactl set image sandboxset <name> container=image # Update container images
okactl status sbs <name>                          # Check SandboxSet update progress
okactl status suo <name>                           # Check SandboxUpdateOps progress
okactl restart sandbox <name> -c <container>       # Restart containers via OpenKruise CRR
okactl create suo -l <selector> container=image    # Create SandboxUpdateOps for claimed sandboxes
```

All commands support resource short names: `sandboxset` → `sbs`, `sandbox` → `sbx`, `sandboxupdateops` → `suo`.

## Motivation

The OpenKruise Agents project has four long-running components (controller, manager, gateway,
runtime) but lacks a lightweight CLI tool for day-to-day operations. Currently, operators must
use `kubectl edit`, `kubectl patch` with complex JSON paths, or hand-write YAML manifests to
perform routine tasks like scaling, image updates, or container restarts.

This is error-prone for users who are not Kubernetes experts — they need to know CRD field
paths like `spec.template.spec.containers[].image` and construct valid JSON patches.

### Goals

- Provide a simple, memorable CLI interface for the three most common sandbox operations.
- Validate user input before submitting to the API server (container name existence, template
  mode checks) to prevent silent failures.
- Follow kubectl conventions (`-n` for namespace, `--kubeconfig`, resource-type subcommands)
  so Kubernetes-savvy users feel at home.
- Keep the CLI stateless and lightweight — no daemon, no local database, just API calls.

### Non-Goals/Future Work

- GUI or web-based management interface.
- Replacing kubectl for advanced operations (e.g., `kubectl describe`, `kubectl logs`).
- Implementing a custom controller for container restart — OpenKruise's existing
  `ContainerRecreateRequest` (CRR) mechanism is used directly.
- Supporting SandboxSets that use `TemplateRef` for the `set image` command (users should
  modify the referenced `SandboxTemplate` directly).

## Proposal

### User Stories

#### Story 1: Scale a SandboxSet

As a platform operator, I want to quickly scale a SandboxSet's idle pool size without editing
YAML, so that I can respond to traffic changes in seconds.

```bash
okactl scale sandboxset my-pool --replicas=10 -n production
```

#### Story 2: Rolling Image Update

As a developer, I want to update the container image in a SandboxSet with a single command,
so that new sandboxes pick up the latest version.

```bash
okactl set image sandboxset my-pool app=mirrors-ssl.aliyuncs.com/ghcr.io/openclaw/openclaw:2026.4.24 -n staging
```

#### Story 3: Restart a Misbehaving Container

As an SRE, I want to restart a specific container in a running sandbox without killing the
entire pod, so that other containers and network connections are preserved.

```bash
okactl restart sandbox user-sbx-abc -c app -n production
```

### Implementation Details

#### Architecture

```
┌──────────────────────────────────────────────────────────────┐
│                        okactl (CLI)                          │
│                                                              │
│  scale ──→ JSON Merge Patch SandboxSet.spec.replicas         │
│  set image sbs ──→ Get + Update SandboxSet.spec.template     │
│  set image status ──→ Get SandboxSet.status                  │
│  restart ──→ Create OpenKruise ContainerRecreateRequest      │
└──────────────────────────┬───────────────────────────────────┘
                           │
                           ▼
┌──────────────────────────────────────────────────────────────┐
│                   Kubernetes API Server                      │
└──────────────────────────┬───────────────────────────────────┘
                           │
         ┌─────────────────┼──────────────────────────┐
         ▼                 ▼                          ▼
   SandboxSet        SandboxSet                OpenKruise
   Controller        Controller                kruise-daemon
   (handles scale)   (handles image/status)    (handles CRR)
```

The first two commands are **pure client-side API operations** — they directly modify existing
CRs and rely on the existing SandboxSet controller to reconcile changes.

The third command uses the **CRD-driven pattern** — the CLI creates an OpenKruise
`ContainerRecreateRequest` CR, and the kruise-daemon (running as a DaemonSet) handles the
actual container restart by stopping the target container's main process.

#### Command Design

| Command | K8s Operation | Why This Approach |
|---------|--------------|-------------------|
| `scale` | `Patch` (MergePatch) | Atomic, no conflict risk, only changes one field |
| `set image sandboxset` | `Get` + `Update` | Need to validate container names exist before modifying |
| `status sbs` | `Get` | Read-only status check with auto-diagnosis |
| `status suo` | `Get` | Read-only SUO batch update progress |
| `create suo` | `Create` | Server-side label selector validation, no client-side filtering |
| `restart` | `Create` (CRR) | Leverages OpenKruise's battle-tested container restart mechanism |

#### Directory Structure

```
cmd/okactl/
  main.go              # Cobra root command entry point, custom usage template

pkg/cli/
  options.go           # GlobalOptions (kubeconfig, namespace, context), client builders
  scale.go             # scale sandboxset implementation
  scale_test.go        # Table-driven tests
  setimage.go          # set image sandboxset implementation + --wait polling
  setimage_test.go     # Table-driven tests
  status.go            # status sbs (SandboxSet) and status suo (SandboxUpdateOps) commands
  status_test.go       # Table-driven tests
  restart.go           # restart sandbox implementation (creates CRR via kruise-api typed client)
  restart_test.go     # Table-driven tests
  create.go            # create suo implementation
  create_test.go       # Table-driven tests
```

#### Key Design Decisions

1. **Two client types in `options.go`**:
   - `AgentsClient()` — generated typed clientset for SandboxSet/Sandbox/SandboxUpdateOps CRUD operations.
   - `KruiseClient()` — OpenKruise typed clientset for creating ContainerRecreateRequest (CRR).

2. **Testability via function extraction**: Each command splits logic into `run()` (builds
   client) and `runXxxWithClient()` (pure logic accepting a client interface). Tests inject
   fake clients without needing a real cluster.

3. **TemplateRef guard**: The `set image` command refuses to operate on SandboxSets using
   `TemplateRef` and instructs the user to modify the referenced SandboxTemplate directly.
   This prevents a common mistake where the patch appears to succeed but has no effect.

4. **Container name validation**: Both `set image` and `restart` verify that specified
   container names actually exist in the resource's template before proceeding.

5. **CRR for restart instead of custom CRD**: Uses OpenKruise's existing
   `ContainerRecreateRequest` mechanism rather than inventing a new CRD + controller.
   OpenKruise's kruise-daemon handles the actual container restart reliably.

6. **Resource short names**: All commands accept Kubernetes-style resource short names
   (`sandboxset` → `sbs`, `sandbox` → `sbx`), consistent with `kubectl` conventions.

7. **kubectl-style help output**: Root command and subcommands include `Long` descriptions,
   `Example` snippets, and command grouping (Resource Commands / Other Commands) following
   the `kubectl --help` format.

8. **Top-level `status` command for update progress**: After running `set image sandboxset` or
   `create suo`, users can check the update progress with `status sbs NAME` or `status suo NAME`.
   The `status` command reads resource status fields and displays a one-line progress summary.
   When an update appears stalled, it automatically diagnoses the root cause by checking sandbox
   status, pod conditions, and container statuses. The `--wait` flag on `set image` polls every
   3 seconds until all replicas are updated and available, with a configurable `--timeout`.

### Risks and Mitigations

| Risk | Mitigation |
|------|-----------|
| OpenKruise CRD not installed in cluster | `restart` command will get a clear API error "resource not found"; we document the dependency |
| `set image` Update conflict under high concurrency | Standard K8s optimistic locking; user retries the command |
| User specifies wrong namespace | CLI defaults to "default" namespace; `-n` flag is always available |
| Image pull failure after `set image` | `set image status` shows update progress; users can diagnose via `kubectl describe sbx` |

## Alternatives

1. **kubectl plugin**: Could be distributed as `kubectl-oka` so users run `kubectl oka scale ...`.
   Rejected because it adds kubectl as a hard dependency and complicates distribution.

2. **Custom CRD + Controller for restart**: Initially implemented a `SandboxContainerRestart`
   CRD with a dedicated controller. Rejected in code review — OpenKruise's CRR already
   provides this capability with a production-proven implementation.

3. **Direct pod deletion for restart**: Simpler but kills all containers simultaneously and
   loses in-memory state. CRR allows single-container restart without disrupting siblings.

## Test Plan

- **Unit tests**: 35 table-driven test cases covering all commands, input validation,
  error paths, and edge cases (TemplateRef, missing containers, missing resources).
- **Integration verification**: Build the binary, connect to a development cluster, and
  execute each command against real SandboxSet/Sandbox resources.
- **CI gate**: `go build ./cmd/okactl/` + `go test ./pkg/cli/...` + `go vet`.

## Implementation History

- 2026-06-02: Initial implementation of okactl with three commands
- 2026-06-03: Refactored restart command to use OpenKruise CRR instead of custom CRD
- 2026-06-12: Added resource short name support (sbs/sbx), kubectl-style help output,
  command grouping, and `set image sbs` shorthand
- 2026-06-15: Separated `set image` to sandboxset-only; added `set image status` subcommand
  with `--wait` flag for checking rolling update progress; removed SUO integration (deferred
  to future `create suo` command)
- 2026-06-24: Added Running phase check to `restart sandbox` command; returns error if
  sandbox is not in Running state before creating CRR
- 2026-06-24: Migrated `restart sandbox` to use OpenKruise kruise-api typed client instead of
  dynamic client; added `--all` flag and `--failure-policy` option to `restart` command; added
  sandbox existence validation to `create suo` using server-side label selector filtering; removed
  `deleteActiveSandboxUpdateOps` and `validateSuoImageContainers` (deferred to controller/webhook);
  separated top-level `status` command from `set image status`; added `--timeout` flag to
  `set image --wait`
