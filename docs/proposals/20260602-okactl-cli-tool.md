---
title: okactl CLI Tool
authors:
  - "@mahe"
creation-date: 2026-06-02
last-updated: 2026-06-12
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

Three core commands are provided:

```bash
okactl scale sandboxset <name> --replicas=N        # Scale SandboxSet replicas
okactl set image sandboxset <name> container=image # Update container images
okactl restart sandbox <name> -c <container>       # Restart containers via OpenKruise CRR
```

All commands support resource short names: `sandboxset` → `sbs`, `sandbox` → `sbx`.

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
okactl set image sandboxset my-pool app=myregistry.com/app:v2.1 -n staging
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
┌─────────────────────────────────────────────────────────┐
│                      okactl (CLI)                        │
│                                                         │
│  scale ──→ JSON Merge Patch SandboxSet.spec.replicas    │
│  set image ──→ Get + Update SandboxSet.spec.template    │
│  restart ──→ Create OpenKruise ContainerRecreateRequest │
└────────────────────────┬────────────────────────────────┘
                         │
                         ▼
┌─────────────────────────────────────────────────────────┐
│                  Kubernetes API Server                   │
└────────────────────────┬────────────────────────────────┘
                         │
          ┌──────────────┼──────────────────────┐
          ▼              ▼                      ▼
   SandboxSet       SandboxSet            OpenKruise
   Controller       Controller            kruise-daemon
   (handles scale)  (handles image)       (handles CRR)
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
| `set image` | `Get` + `Update` | Need to validate container names exist before modifying |
| `restart` | `Create` (CRR) | Leverages OpenKruise's battle-tested container restart mechanism |

#### Directory Structure

```
cmd/okactl/
  main.go              # Cobra root command entry point

pkg/cli/
  options.go           # GlobalOptions (kubeconfig, namespace, context), client builders
  scale.go             # scale sandboxset implementation
  scale_test.go        # Table-driven tests (4 cases)
  setimage.go          # set image sandboxset implementation
  setimage_test.go     # Table-driven tests (14 cases)
  restart.go           # restart sandbox implementation (creates CRR)
  restart_test.go      # Table-driven tests (13 cases)
```

#### Key Design Decisions

1. **Two client types in `options.go`**:
   - `AgentsClient()` — generated typed clientset for SandboxSet/Sandbox CRUD operations.
   - `DynamicClient()` — for creating OpenKruise CRR (which has no typed client in this project).

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

### Risks and Mitigations

| Risk | Mitigation |
|------|-----------|
| OpenKruise CRD not installed in cluster | `restart` command will get a clear API error "resource not found"; we document the dependency |
| `set image` Update conflict under high concurrency | Standard K8s optimistic locking; user retries the command |
| User specifies wrong namespace | CLI defaults to "default" namespace; `-n` flag is always available |

## Alternatives

1. **kubectl plugin**: Could be distributed as `kubectl-oka` so users run `kubectl oka scale ...`.
   Rejected because it adds kubectl as a hard dependency and complicates distribution.

2. **Custom CRD + Controller for restart**: Initially implemented a `SandboxContainerRestart`
   CRD with a dedicated controller. Rejected in code review — OpenKruise's CRR already
   provides this capability with a production-proven implementation.

3. **Direct pod deletion for restart**: Simpler but kills all containers simultaneously and
   loses in-memory state. CRR allows single-container restart without disrupting siblings.

## Test Plan

- **Unit tests**: 31 table-driven test cases covering all commands, input validation,
  error paths, and edge cases (TemplateRef, missing containers, missing resources).
- **Integration verification**: Build the binary, connect to a development cluster, and
  execute each command against real SandboxSet/Sandbox resources.
- **CI gate**: `go build ./cmd/okactl/` + `go test ./pkg/cli/...` + `go vet`.

## Implementation History

- 2026-06-02: Initial implementation of okactl with three commands
- 2026-06-03: Refactored restart command to use OpenKruise CRR instead of custom CRD
- 2026-06-12: Added resource short name support (sbs/sbx), kubectl-style help output,
  command grouping, and `set image sbs` shorthand
